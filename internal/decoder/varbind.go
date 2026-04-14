package decoder

import (
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"
	"snmp/snmp-collector/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// DecodedVarbind
// ─────────────────────────────────────────────────────────────────────────────

// DecodedVarbind is the result of decoding a single gosnmp.SnmpPDU against an
// ObjectDefinition. It is an intermediate type used only within this package;
// the Decoder assembles a slice of DecodedVarbinds into models.Metric values.
type DecodedVarbind struct {
	// OID is the full numeric OID of the varbind, normalised without leading dot.
	OID string

	// AttributeName is the resolved config attribute name, e.g. "netif.bytes.in".
	AttributeName string

	// Instance is the table row index suffix extracted from the OID,
	// e.g. "1" for a scalar instance or "192.168.1.1.5" for a compound index.
	// Empty string for scalar (non-table) objects.
	Instance string

	// Value is the converted native Go value (int64, uint64, float64, string, []byte).
	Value interface{}

	// SNMPType is the human-readable SNMP PDU type string, e.g. "Counter64".
	SNMPType string

	// Syntax is the config syntax string verbatim, e.g. "BandwidthMBits".
	Syntax string

	// IsTag indicates whether this attribute is a dimension label rather than
	// a numeric metric.
	IsTag bool
}

// ─────────────────────────────────────────────────────────────────────────────
// VarbindParser
// ─────────────────────────────────────────────────────────────────────────────

// VarbindParser parses a slice of raw gosnmp PDUs against a known ObjectDefinition,
// producing a slice of DecodedVarbinds. It is stateless and safe for concurrent use.
type VarbindParser struct {
	// attrByOID maps normalised attribute OIDs (no leading dot) to their
	// AttributeDefinition. Built once from the ObjectDefinition at construction.
	attrByOID map[string]models.AttributeDefinition
}

// NewVarbindParser constructs a VarbindParser for the given ObjectDefinition.
// It pre-computes the OID lookup map so that parsing each varbind is O(1).
func NewVarbindParser(def models.ObjectDefinition) (*VarbindParser, error) {
	if len(def.Attributes) == 0 {
		return nil, fmt.Errorf("object definition %q has no attributes", def.Key)
	}

	attrByOID := make(map[string]models.AttributeDefinition, len(def.Attributes))
	for _, attr := range def.Attributes {
		norm := normaliseOID(attr.OID)
		if norm == "" {
			return nil, fmt.Errorf(
				"attribute %q in object %q has an empty OID", attr.Name, def.Key,
			)
		}
		attrByOID[norm] = attr
	}

	return &VarbindParser{attrByOID: attrByOID}, nil
}

// Parse converts raw gosnmp PDUs to DecodedVarbinds.
//
// PDUs whose OID does not match any attribute in the ObjectDefinition are
// silently skipped — this is normal behaviour for GetBulk responses which often
// return OIDs beyond the requested range.
//
// PDUs with error types (NoSuchObject, NoSuchInstance, EndOfMibView) are also
// silently skipped; the caller can inspect the returned slice length to detect
// whether all expected attributes were collected.
func (p *VarbindParser) Parse(pdus []gosnmp.SnmpPDU) ([]DecodedVarbind, error) {
	result := make([]DecodedVarbind, 0, len(pdus))

	for i := range pdus {
		pdu := &pdus[i]

		// Skip error sentinels before doing any work.
		if IsErrorType(pdu.Type) {
			continue
		}

		fullOID := normaliseOID(pdu.Name)

		attr, instance, found := p.matchAttribute(fullOID)
		if !found {
			// OID is outside the polled sub-tree; normal for bulk walks.
			continue
		}

		convertedValue, err := ConvertValue(pdu.Type, pdu.Value, attr.Syntax)
		if err != nil {
			// Non-fatal: log-worthy but should not abort the rest of the batch.
			// The caller (Decoder) is responsible for structured logging.
			return result, fmt.Errorf(
				"oid %s (attr %s, syntax %s): value conversion failed: %w",
				fullOID, attr.Name, attr.Syntax, err,
			)
		}

		result = append(result, DecodedVarbind{
			OID:           fullOID,
			AttributeName: attr.Name,
			Instance:      instance,
			Value:         convertedValue,
			SNMPType:      PDUTypeString(pdu.Type),
			Syntax:        attr.Syntax,
			IsTag:         attr.IsTag,
		})
	}

	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// matchAttribute looks up an attribute by OID prefix matching.
//
// A varbind OID like "1.3.6.1.2.1.2.2.1.10.1" matches attribute OID
// "1.3.6.1.2.1.2.2.1.10"; the suffix ".1" is returned as the instance.
//
// For scalars (index is ".0"), the instance is returned as "0".
func (p *VarbindParser) matchAttribute(fullOID string) (attr models.AttributeDefinition, instance string, found bool) {
	// Direct match first (scalar OIDs include ".0").
	if a, ok := p.attrByOID[fullOID]; ok {
		return a, "0", true
	}

	// Prefix scan: walk the OID right-to-left to find the longest matching prefix.
	// This handles table OIDs where the instance is appended after the column OID.
	remaining := fullOID
	for {
		dot := strings.LastIndex(remaining, ".")
		if dot < 0 {
			break
		}
		prefix := remaining[:dot]
		suffix := remaining[dot+1:]

		if a, ok := p.attrByOID[prefix]; ok {
			// Reconstruct full instance from suffix components collected so far.
			// We peel from the right, so rebuild in reverse: prefix.suffix is the
			// true attribute OID and everything after it is the instance.
			instanceSuffix := fullOID[len(prefix)+1:] // everything after "prefix."
			_ = suffix                                // already captured above
			return a, instanceSuffix, true
		}

		remaining = prefix
	}

	return models.AttributeDefinition{}, "", false
}

// normaliseOID strips a leading dot and any whitespace from an OID string.
// All OIDs inside this package are stored and compared in the no-leading-dot form.
func normaliseOID(oid string) string {
	oid = strings.TrimSpace(oid)
	return strings.TrimPrefix(oid, ".")
}
