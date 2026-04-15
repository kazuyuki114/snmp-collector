package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"snmp/snmp-collector/internal/config"
	"snmp/snmp-collector/internal/decoder"
	"snmp/snmp-collector/internal/models"
	"snmp/snmp-collector/internal/noop"

	"github.com/gosnmp/gosnmp"
)

// ─────────────────────────────────────────────────────────────────────────────
// PollJob — unit of work
// ─────────────────────────────────────────────────────────────────────────────

// PollJob describes a single SNMP poll request to be executed.
type PollJob struct {
	// Hostname is the key into LoadedConfig.Devices that identifies the target.
	Hostname string

	// Device carries the models.Device fields used in RawPollResult.
	Device models.Device

	// DeviceConfig is the resolved configuration for the device.
	DeviceConfig config.DeviceConfig

	// ObjectDef is the definition of the SNMP object to poll.
	ObjectDef models.ObjectDefinition
}

// ─────────────────────────────────────────────────────────────────────────────
// Poller interface
// ─────────────────────────────────────────────────────────────────────────────

// Poller executes a single SNMP poll job and returns the raw result.
type Poller interface {
	Poll(ctx context.Context, job PollJob) (decoder.RawPollResult, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMPPoller — production implementation
// ─────────────────────────────────────────────────────────────────────────────

// SNMPPoller is the production Poller backed by a ConnectionPool.
type SNMPPoller struct {
	pool   *ConnectionPool
	logger *slog.Logger
}

// NewSNMPPoller creates a new poller that obtains sessions from pool.
func NewSNMPPoller(pool *ConnectionPool, logger *slog.Logger) *SNMPPoller {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	return &SNMPPoller{pool: pool, logger: logger}
}

// Poll executes the SNMP operation described by job and returns a RawPollResult.
//
// Operation selection:
//   - Scalar object (no Index) → Get all attribute OIDs appended with ".0"
//   - Table object + SNMPv1    → Walk the lowest-common-prefix OID
//   - Table object + v2c / v3  → BulkWalk the lowest-common-prefix OID
func (p *SNMPPoller) Poll(ctx context.Context, job PollJob) (decoder.RawPollResult, error) {
	var result decoder.RawPollResult

	conn, err := p.pool.Get(ctx, job.Hostname, job.DeviceConfig)
	if err != nil {
		return result, fmt.Errorf("pool get %s: %w", job.Hostname, err)
	}

	result.Device = job.Device
	result.ObjectDef = job.ObjectDef

	// Wire context so cancellation and deadlines propagate into gosnmp's
	// marshal loop (checked on every PDU send in marshal.go).
	conn.Context = ctx

	var pdus []gosnmp.SnmpPDU
	result.PollStartedAt = time.Now()

	if isScalar(job.ObjectDef) {
		pdus, err = p.doGet(conn, job.ObjectDef)
	} else if job.DeviceConfig.Version == "1" || !job.DeviceConfig.UseGetBulk {
		pdus, err = p.doWalk(conn, job.ObjectDef)
	} else {
		pdus, err = p.doBulkWalk(conn, job.ObjectDef)
	}
	result.CollectedAt = time.Now()
	result.Varbinds = pdus

	// Reset context before returning the connection to the pool so the next
	// caller does not inherit a cancelled or deadline-exceeded context.
	conn.Context = context.Background()

	if err != nil {
		// Connection might be broken — discard it.
		p.pool.Discard(job.Hostname, conn)
		return result, fmt.Errorf("snmp %s %s: %w", job.Hostname, job.ObjectDef.Key, err)
	}

	// Return connection for reuse.
	p.pool.Put(job.Hostname, conn)

	p.logger.Debug("poll completed",
		"device", job.Hostname,
		"object", job.ObjectDef.Key,
		"pdu_count", len(pdus),
		"duration_ms", result.CollectedAt.Sub(result.PollStartedAt).Milliseconds(),
	)
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMP operation helpers
// ─────────────────────────────────────────────────────────────────────────────

// doGet performs an SNMP Get for scalar objects. Each attribute OID gets ".0"
// appended (scalar instance).
func (p *SNMPPoller) doGet(conn *gosnmp.GoSNMP, objDef models.ObjectDefinition) ([]gosnmp.SnmpPDU, error) {
	oids := make([]string, 0, len(objDef.Attributes))
	for _, attr := range objDef.Attributes {
		oid := attr.OID
		if !strings.HasSuffix(oid, ".0") {
			oid += ".0"
		}
		oids = append(oids, oid)
	}
	if len(oids) == 0 {
		return nil, nil
	}

	// gosnmp.Get has a MaxOids limit; split into batches if necessary.
	maxOids := int(conn.MaxOids)
	if maxOids <= 0 {
		maxOids = 60
	}

	var all []gosnmp.SnmpPDU
	for i := 0; i < len(oids); i += maxOids {
		end := i + maxOids
		if end > len(oids) {
			end = len(oids)
		}
		pkt, err := conn.Get(oids[i:end])
		if err != nil {
			return all, err
		}
		all = append(all, pkt.Variables...)
	}
	return all, nil
}

// doWalk performs an SNMPv1 Walk (GetNext-based) using the lowest common
// OID prefix from the object's attributes.
func (p *SNMPPoller) doWalk(conn *gosnmp.GoSNMP, objDef models.ObjectDefinition) ([]gosnmp.SnmpPDU, error) {
	root := LowestCommonOID(objDef)
	if root == "" {
		return nil, fmt.Errorf("no attribute OIDs in object %s", objDef.Key)
	}
	return conn.WalkAll(root)
}

// doBulkWalk performs a v2c/v3 BulkWalk using the lowest common OID prefix.
func (p *SNMPPoller) doBulkWalk(conn *gosnmp.GoSNMP, objDef models.ObjectDefinition) ([]gosnmp.SnmpPDU, error) {
	root := LowestCommonOID(objDef)
	if root == "" {
		return nil, fmt.Errorf("no attribute OIDs in object %s", objDef.Key)
	}
	return conn.BulkWalkAll(root)
}

// ─────────────────────────────────────────────────────────────────────────────
// OID analysis
// ─────────────────────────────────────────────────────────────────────────────

// classifyError maps a raw SNMP error to a stable category string used in
// MetricMetadata.ErrorType for downstream alerting and querying.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no route"),
		strings.Contains(msg, "unreachable"),
		strings.Contains(msg, "host is down"):
		return "unreachable"
	case strings.Contains(msg, "authentication"),
		strings.Contains(msg, "unknown user"),
		strings.Contains(msg, "invalid community"):
		return "auth_failed"
	case strings.Contains(msg, "nosuchobject"),
		strings.Contains(msg, "no such object"):
		return "no_such_object"
	default:
		return "error"
	}
}

// isScalar returns true when the object definition has no table index —
// meaning all attributes are scalar OIDs.
//
// Objects that use augments: to borrow an index from another table are
// still table objects and must not be treated as scalar, even though their
// Index slice is empty.
func isScalar(objDef models.ObjectDefinition) bool {
	return len(objDef.Index) == 0 && objDef.Augments == ""
}

// LowestCommonOID finds the shortest OID prefix that is a parent of all
// attribute OIDs in the object definition. For example, given:
//
//	.1.3.6.1.2.1.2.2.1.10 (ifInOctets)
//	.1.3.6.1.2.1.2.2.1.16 (ifOutOctets)
//
// the lowest common prefix is ".1.3.6.1.2.1.2.2.1".
func LowestCommonOID(objDef models.ObjectDefinition) string {
	var oids []string
	for _, attr := range objDef.Attributes {
		if attr.OID != "" {
			oids = append(oids, attr.OID)
		}
	}
	if len(oids) == 0 {
		return ""
	}
	if len(oids) == 1 {
		return oids[0]
	}

	// Split first OID into parts, then trim to match every other OID.
	parts := strings.Split(oids[0], ".")
	for _, oid := range oids[1:] {
		other := strings.Split(oid, ".")
		minLen := len(parts)
		if len(other) < minLen {
			minLen = len(other)
		}
		match := 0
		for i := 0; i < minLen; i++ {
			if parts[i] != other[i] {
				break
			}
			match = i + 1
		}
		parts = parts[:match]
	}
	return strings.Join(parts, ".")
}
