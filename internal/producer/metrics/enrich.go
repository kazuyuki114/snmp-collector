// Package metrics implements Stage 4 of the processing pipeline.
// It converts decoder.DecodedPollResult values into models.SNMPMetric values
// by grouping varbinds by table instance, building dimension tag maps,
// applying enum resolution, and handling counter overrides.
package metrics

import (
	"fmt"
	"strings"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// EnumRegistry — translates raw SNMP integer/OID values to text labels
// ─────────────────────────────────────────────────────────────────────────────

// EnumRegistry holds per-OID translation tables for integer enumerations,
// bitmap enumerations, and OID enumerations. It is safe for concurrent reads
// after construction.
//
// Loaded from YAML files under PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH.
// Enabled at runtime with PROCESSOR_SNMP_ENUM_ENABLE=true.
type EnumRegistry struct {
	mu   sync.RWMutex
	ints map[string]IntEnum // OID → integer value translations
	oids map[string]string  // OID → label (OID-valued enumerations)
}

// IntEnum maps integer values (and bitmap bit-positions) to text labels.
type IntEnum struct {
	IsBitmap bool
	Values   map[int64]string // integer value → label
}

// NewEnumRegistry creates an empty, ready-to-use EnumRegistry.
func NewEnumRegistry() *EnumRegistry {
	return &EnumRegistry{
		ints: make(map[string]IntEnum),
		oids: make(map[string]string),
	}
}

// RegisterIntEnum adds or replaces an integer enumeration for the given OID.
// oid must be in dotted-decimal form without a leading dot.
func (r *EnumRegistry) RegisterIntEnum(oid string, isBitmap bool, values map[int64]string) {
	oid = normaliseRegistryOID(oid)
	r.mu.Lock()
	r.ints[oid] = IntEnum{IsBitmap: isBitmap, Values: values}
	r.mu.Unlock()
}

// RegisterOIDEnum adds an OID-to-label mapping (OID enumeration type).
// Both the key OID and the label can be OID strings.
func (r *EnumRegistry) RegisterOIDEnum(oid, label string) {
	oid = normaliseRegistryOID(oid)
	r.mu.Lock()
	r.oids[oid] = label
	r.mu.Unlock()
}

// Resolve attempts to translate a raw SNMP value to a text label using the
// enum table registered for the given OID. The oid argument is the attribute
// OID (no leading dot).
//
// Resolution rules:
//   - For integer enums: value must be int64 or convertible numeric type.
//   - For bitmap enums: returns a comma-joined list of set bit labels.
//   - For OID enums: value must be a string OID; looked up as a key.
//
// If no enum is registered or no match is found, rawValue is returned unchanged.
// This is the safe-fallback contract so missing enum files never crash the pipeline.
func (r *EnumRegistry) Resolve(oid string, rawValue interface{}) interface{} {
	oid = normaliseRegistryOID(oid)
	r.mu.RLock()
	defer r.mu.RUnlock()

	// OID enumeration (OID → label lookup).
	if label, ok := r.oids[oid]; ok {
		return label
	}
	if strVal, ok := rawValue.(string); ok {
		if label, ok := r.oids[normaliseRegistryOID(strVal)]; ok {
			return label
		}
	}

	// Integer or bitmap enumeration.
	intEnum, ok := r.ints[oid]
	if !ok {
		return rawValue
	}

	intVal, err := toInt64ForEnum(rawValue)
	if err != nil {
		return rawValue
	}

	if intEnum.IsBitmap {
		return resolveBitmap(intEnum.Values, intVal)
	}

	if label, ok := intEnum.Values[intVal]; ok {
		return label
	}
	return rawValue
}

// resolveBitmap returns a comma-separated list of active bit labels.
// If no bits are set or no labels are registered, returns the raw integer string.
func resolveBitmap(values map[int64]string, mask int64) interface{} {
	var active []string
	// Iterate bit positions 0..63 in order.
	for bit := int64(0); bit < 64; bit++ {
		if mask&(1<<bit) != 0 {
			if label, ok := values[bit]; ok {
				active = append(active, label)
			}
		}
	}
	if len(active) == 0 {
		return mask
	}
	return strings.Join(active, ",")
}

// toInt64ForEnum attempts a best-effort conversion to int64 for enum lookup.
func toInt64ForEnum(v interface{}) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil // intentional narrowing; enums are small integers
	default:
		return 0, fmt.Errorf("cannot convert %T to int64 for enum lookup", v)
	}
}

// normaliseRegistryOID strips a leading dot for consistent map keying.
func normaliseRegistryOID(oid string) string {
	return strings.TrimPrefix(strings.TrimSpace(oid), ".")
}
