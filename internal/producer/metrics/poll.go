package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"snmp/snmp-collector/internal/models"
	"snmp/snmp-collector/internal/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// Build options
// ─────────────────────────────────────────────────────────────────────────────

// BuildOptions configures how a DecodedPollResult is assembled into a SNMPMetric.
// All pointer fields are optional; nil disables the corresponding feature.
type BuildOptions struct {
	// CollectorID is written into MetricMetadata.CollectorID.
	CollectorID string

	// PollStatus is "success" or "error".
	PollStatus string

	// ErrorType is the stable failure category ("timeout", "unreachable", …).
	// Empty on success.
	ErrorType string

	// ErrorDetail is the raw error message. Empty on success.
	ErrorDetail string

	// ObjectKey identifies the object definition (e.g. "ifTable") for this poll.
	ObjectKey string

	// Enums, when non-nil, resolves EnumInteger / EnumBitmap / EnumObjectIdentifier
	// values to their text labels. When nil, raw numeric values are left intact.
	Enums *EnumRegistry

	// Counters, when non-nil, replaces raw Counter32/Counter64 values with the
	// per-interval delta. On first observation the metric is still emitted but
	// with Value = 0 and the counter is seeded for the next interval.
	Counters *CounterState
}

// ─────────────────────────────────────────────────────────────────────────────
// Build — core assembly function
// ─────────────────────────────────────────────────────────────────────────────

// Build converts a decoder.DecodedPollResult into a models.SNMPMetric.
//
// Steps:
//  1. Separate tag varbinds from metric varbinds.
//  2. Group both sets by table instance.
//  3. Override resolution: per instance+name keep the highest-priority syntax.
//  4. Apply enum resolution (if opts.Enums != nil).
//  5. Apply counter delta (if opts.Counters != nil and syntax is counter).
//  6. Assemble models.Metric for each non-tag varbind with its instance's tags.
func Build(decoded decoder.DecodedPollResult, opts BuildOptions) models.SNMPMetric {
	now := decoded.CollectedAt
	if now.IsZero() {
		now = time.Now()
	}

	// ── Step 1 & 2: partition and group by instance ──────────────────────────
	// tagsByInstance[instance][attributeName] = string value
	tagsByInstance := make(map[string]map[string]string)
	// metricsByInstance[instance] = slice of candidate varbinds (may have
	// duplicates with same name; resolved in step 3)
	metricsByInstance := make(map[string][]decoder.DecodedVarbind)

	for _, vb := range decoded.Varbinds {
		if vb.IsTag {
			if tagsByInstance[vb.Instance] == nil {
				tagsByInstance[vb.Instance] = make(map[string]string)
			}
			tagsByInstance[vb.Instance][vb.AttributeName] = tagValue(vb.Value)
		} else {
			metricsByInstance[vb.Instance] = append(metricsByInstance[vb.Instance], vb)
		}
	}

	// ── Step 3: override resolution — keep highest-priority syntax per name ──
	// resolved[instance][attributeName] = winning varbind
	resolved := make(map[string]map[string]decoder.DecodedVarbind)
	for instance, vbs := range metricsByInstance {
		byName := make(map[string]decoder.DecodedVarbind, len(vbs))
		for _, vb := range vbs {
			existing, exists := byName[vb.AttributeName]
			if !exists || syntaxPriority(vb.Syntax) > syntaxPriority(existing.Syntax) {
				byName[vb.AttributeName] = vb
			}
		}
		resolved[instance] = byName
	}

	// ── Steps 4 & 5: enum + counter; assemble models.Metric ─────────────────
	metrics := make([]models.Metric, 0, len(decoded.Varbinds))

	for instance, byName := range resolved {
		// Merge this instance's tags with device-level tags from poll tags.
		// Device-level tags live in models.Device.Tags; per-instance tags come
		// from tagsByInstance. Both are propagated: instance tags shadow device tags.
		instanceTags := tagsByInstance[instance] // may be nil

		for _, vb := range byName {
			value := vb.Value

			// Enum resolution.
			// Strip the instance suffix from the full varbind OID so that the
			// lookup key matches the base attribute OID stored in the registry.
			if opts.Enums != nil && isEnumSyntax(vb.Syntax) {
				baseOID := vb.OID
				if vb.Instance != "" {
					baseOID = strings.TrimSuffix(vb.OID, "."+vb.Instance)
				}
				value = opts.Enums.Resolve(baseOID, value)
			}

			// Counter delta.
			if opts.Counters != nil && IsCounterSyntax(vb.Syntax) {
				if raw, ok := toUint64Safe(value); ok {
					key := CounterKey{
						Device:    decoded.Device.Hostname,
						Attribute: vb.AttributeName,
						Instance:  instance,
					}
					dr := opts.Counters.Delta(key, raw, now, WrapForSyntax(vb.Syntax))
					switch {
					case dr.Reset:
						// Counter reset (device reboot / agent restart): emit 0
						// so the downstream alerting system sees no false spike.
						// The counter state has been reseeded; the next interval
						// will produce a correct delta.
						value = uint64(0)
					case dr.Valid:
						value = dr.Delta
					default:
						value = uint64(0) // first observation: emit 0 so the metric exists
					}
				}
			}

			// Build tag map for this metric: copy instance tags (may be nil).
			var tags map[string]string
			if len(instanceTags) > 0 {
				tags = make(map[string]string, len(instanceTags))
				for k, v := range instanceTags {
					tags[k] = v
				}
			}

			metrics = append(metrics, models.Metric{
				OID:      vb.OID,
				Name:     vb.AttributeName,
				Instance: instance,
				Value:    value,
				Type:     vb.SNMPType,
				Syntax:   vb.Syntax,
				Tags:     tags,
			})
		}
	}

	return models.SNMPMetric{
		Timestamp: now,
		Device:    decoded.Device,
		Metrics:   metrics,
		Metadata: models.MetricMetadata{
			CollectorID:    opts.CollectorID,
			PollDurationMs: decoded.PollDurationMs,
			PollStatus:     opts.PollStatus,
			ObjectKey:      opts.ObjectKey,
			ErrorType:      opts.ErrorType,
			ErrorDetail:    opts.ErrorDetail,
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// tagValue converts a DecodedVarbind's value to a string suitable for use as a
// dimension label.
func tagValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return fmt.Sprintf("%g", x)
	case []byte:
		return string(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isEnumSyntax returns true for syntax types that should have enum resolution
// applied. Only the explicit enum types are resolved; raw integer types such as
// plain Integer / Integer32 are left as numeric.
func isEnumSyntax(syntax string) bool {
	switch syntax {
	case "EnumInteger", "EnumIntegerKeepID",
		"EnumBitmap",
		"EnumObjectIdentifier", "EnumObjectIdentifierKeepOID":
		return true
	default:
		return false
	}
}

// toUint64Safe converts a value to uint64 without panicking.
func toUint64Safe(v interface{}) (uint64, bool) {
	switch x := v.(type) {
	case uint64:
		return x, true
	case uint32:
		return uint64(x), true
	case uint:
		return uint64(x), true
	case int64:
		if x >= 0 {
			return uint64(x), true
		}
	case int:
		if x >= 0 {
			return uint64(x), true
		}
	}
	return 0, false
}
