package decoder

import (
	"fmt"
	"log/slog"
	"time"

	"snmp/snmp-collector/internal/models"

	"github.com/gosnmp/gosnmp"
)

// ─────────────────────────────────────────────────────────────────────────────
// Channel message types
// ─────────────────────────────────────────────────────────────────────────────

// RawPollResult is the message placed on the raw-data channel by the Poller after
// a poll attempt (successful or failed). It is the sole input type consumed by the Decoder.
type RawPollResult struct {
	// Device carries identifying context about the polled network device.
	Device models.Device

	// ObjectDef is the configuration definition that drove this poll request.
	// It tells the Decoder how to map raw OIDs to attribute names and syntax.
	ObjectDef models.ObjectDefinition

	// Varbinds contains the raw SNMP variable bindings from the PDU response,
	// exactly as returned by the gosnmp library. Empty on poll failure.
	Varbinds []gosnmp.SnmpPDU

	// CollectedAt is the wall-clock time at which the SNMP response was received.
	CollectedAt time.Time

	// PollStartedAt is the wall-clock time at which the SNMP request was sent.
	// Together with CollectedAt it yields the round-trip poll duration.
	PollStartedAt time.Time

	// PollStatus is "success" when the poll completed without error, "error" otherwise.
	PollStatus string

	// ErrorType is a stable category for the failure: "timeout", "unreachable",
	// "auth_failed", "no_such_object", or "error". Empty on success.
	ErrorType string

	// PollError is the raw error message returned by the SNMP library. Empty on success.
	PollError string
}

// DecodedPollResult is the message placed on the decoded-data channel by the
// Decoder. It is consumed by the Producer stage.
//
// Varbinds is a flat slice — both tag and metric attributes are included. The
// Producer is responsible for grouping by instance and assembling final Metric
// values with their dimension tags.
type DecodedPollResult struct {
	// Device is forwarded unchanged from RawPollResult.
	Device models.Device

	// ObjectDefKey identifies which ObjectDefinition produced this result,
	// e.g. "IF-MIB::ifEntry". Useful for debugging and routing in the Producer.
	ObjectDefKey string

	// Varbinds contains the fully decoded variable bindings. Empty on poll failure.
	Varbinds []DecodedVarbind

	// CollectedAt is the wall-clock time at which the SNMP response was received.
	CollectedAt time.Time

	// PollDurationMs is the round-trip poll duration in milliseconds.
	PollDurationMs int64

	// PollStatus is forwarded from RawPollResult: "success" or "error".
	PollStatus string

	// ErrorType is forwarded from RawPollResult: stable failure category.
	ErrorType string

	// PollError is forwarded from RawPollResult: raw error message.
	PollError string
}

// ─────────────────────────────────────────────────────────────────────────────
// Decoder interface
// ─────────────────────────────────────────────────────────────────────────────

// Decoder converts a RawPollResult into a DecodedPollResult. Implementations
// must be safe for concurrent use — in production the pipeline runs multiple
// decoder goroutines calling the same instance.
type Decoder interface {
	Decode(raw RawPollResult) (DecodedPollResult, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMPDecoder — production implementation
// ─────────────────────────────────────────────────────────────────────────────

// SNMPDecoder is the production Decoder implementation. It is stateless once
// constructed and safe for concurrent calls to Decode.
type SNMPDecoder struct {
	logger *slog.Logger
}

// NewSNMPDecoder constructs an SNMPDecoder. Pass a structured logger configured
// for JSON output (matching -log.fmt=json in the architecture).
//
//	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
//	    Level: slog.LevelInfo,
//	}))
//	dec := decoder.NewSNMPDecoder(logger)
func NewSNMPDecoder(logger *slog.Logger) *SNMPDecoder {
	if logger == nil {
		// Provide a no-op logger rather than panic — this eliminates nil checks below.
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	return &SNMPDecoder{logger: logger}
}

// Decode implements Decoder.
//
// For each gosnmp PDU in raw.Varbinds it:
//  1. Matches the OID against the attribute definitions in raw.ObjectDef.
//  2. Extracts the table row instance suffix from the OID.
//  3. Converts the raw value to a native Go type using the configured syntax.
//
// A partial result is returned alongside any error so the caller can decide
// whether to use or discard the decoded varbinds collected before the error.
func (d *SNMPDecoder) Decode(raw RawPollResult) (DecodedPollResult, error) {
	result := DecodedPollResult{
		Device:         raw.Device,
		ObjectDefKey:   raw.ObjectDef.Key,
		CollectedAt:    raw.CollectedAt,
		PollDurationMs: raw.CollectedAt.Sub(raw.PollStartedAt).Milliseconds(),
		PollStatus:     raw.PollStatus,
		ErrorType:      raw.ErrorType,
		PollError:      raw.PollError,
	}

	if len(raw.Varbinds) == 0 {
		// Intentional failure record — forward metadata downstream unchanged.
		if raw.PollStatus == "error" {
			return result, nil
		}
		d.logger.Warn("decode: empty varbind list",
			"device", raw.Device.Hostname,
			"object", raw.ObjectDef.Key,
		)
		return result, nil
	}

	parser, err := NewVarbindParser(raw.ObjectDef)
	if err != nil {
		return result, fmt.Errorf(
			"decode: failed to build varbind parser for object %q on device %q: %w",
			raw.ObjectDef.Key, raw.Device.Hostname, err,
		)
	}

	decoded, err := parser.Parse(raw.Varbinds)
	if err != nil {
		// Parse returns a partial result even on error. Log and surface both.
		d.logger.Error("decode: partial varbind parse error",
			"device", raw.Device.Hostname,
			"object", raw.ObjectDef.Key,
			"decoded_count", len(decoded),
			"total_pdus", len(raw.Varbinds),
			"error", err.Error(),
		)
		result.Varbinds = decoded
		return result, fmt.Errorf(
			"decode: object %q device %q: %w",
			raw.ObjectDef.Key, raw.Device.Hostname, err,
		)
	}

	if len(decoded) == 0 {
		// Distinguish between "device doesn't support this MIB" (all PDUs are
		// error sentinels like NoSuchObject/NoSuchInstance) vs "OIDs are outside
		// the configured attribute tree" (real data returned but nothing matched).
		allErrors := true
		for i := range raw.Varbinds {
			if !IsErrorType(raw.Varbinds[i].Type) {
				allErrors = false
				break
			}
		}
		if allErrors {
			d.logger.Warn("decode: empty varbind list",
				"device", raw.Device.Hostname,
				"object", raw.ObjectDef.Key,
			)
		} else {
			oids := make([]string, 0, len(raw.Varbinds))
			for i := range raw.Varbinds {
				oids = append(oids, raw.Varbinds[i].Name)
			}
			d.logger.Warn("decode: no attributes matched — PDUs may be outside the configured object tree",
				"device", raw.Device.Hostname,
				"object", raw.ObjectDef.Key,
				"pdu_count", len(raw.Varbinds),
				"received_oids", oids,
			)
		}
	}

	d.logger.Debug("decode: completed",
		"device", raw.Device.Hostname,
		"object", raw.ObjectDef.Key,
		"pdu_count", len(raw.Varbinds),
		"decoded_count", len(decoded),
		"poll_duration_ms", result.PollDurationMs,
	)

	result.Varbinds = decoded
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// noopWriter — discard all log output when no logger is provided
// ─────────────────────────────────────────────────────────────────────────────

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
