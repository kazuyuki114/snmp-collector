// Package decoder implements the SNMP Response Decoder — the first stage of the
// processing pipeline after the poller. It converts raw gosnmp PDU responses into
// the canonical models.Metric representation consumed by the producer stage.
package decoder

import (
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// ─────────────────────────────────────────────────────────────────────────────
// SNMP PDU Type → String
// ─────────────────────────────────────────────────────────────────────────────

// PDUTypeString returns the human-readable name for a gosnmp Asn1BER type tag.
// The string is used verbatim in models.Metric.Type.
func PDUTypeString(t gosnmp.Asn1BER) string {
	switch t {
	case gosnmp.Integer:
		return "Integer"
	case gosnmp.BitString:
		return "BitString"
	case gosnmp.OctetString:
		return "OctetString"
	case gosnmp.Null:
		return "Null"
	case gosnmp.ObjectIdentifier:
		return "ObjectIdentifier"
	case gosnmp.ObjectDescription:
		return "ObjectDescription"
	case gosnmp.IPAddress:
		return "IpAddress"
	case gosnmp.Counter32:
		return "Counter32"
	case gosnmp.Gauge32:
		return "Gauge32"
	case gosnmp.TimeTicks:
		return "TimeTicks"
	case gosnmp.Opaque:
		return "Opaque"
	case gosnmp.NsapAddress:
		return "NsapAddress"
	case gosnmp.Counter64:
		return "Counter64"
	case gosnmp.Uinteger32:
		return "Unsigned32"
	case gosnmp.OpaqueFloat:
		return "OpaqueFloat"
	case gosnmp.OpaqueDouble:
		return "OpaqueDouble"
	case gosnmp.NoSuchObject:
		return "NoSuchObject"
	case gosnmp.NoSuchInstance:
		return "NoSuchInstance"
	case gosnmp.EndOfMibView:
		return "EndOfMibView"
	default:
		return fmt.Sprintf("Unknown(0x%02X)", uint8(t))
	}
}

// IsErrorType returns true when the PDU type signals an SNMP retrieval error
// rather than an actual value. Callers should skip these varbinds.
func IsErrorType(t gosnmp.Asn1BER) bool {
	return t == gosnmp.NoSuchObject || t == gosnmp.NoSuchInstance || t == gosnmp.EndOfMibView || t == gosnmp.Null
}

// ─────────────────────────────────────────────────────────────────────────────
// Value Conversion
// ─────────────────────────────────────────────────────────────────────────────

// ConvertValue converts a raw gosnmp PDU value to the native Go type dictated by
// the config syntax. The returned value will be one of: int64, uint64, float64,
// string, []byte — matching the models.Metric.Value contract.
//
// rawType is the ASN.1 type from the PDU.
// rawValue is the value interface{} from gosnmp.SnmpPDU.Value.
// syntax is the config-level syntax string (e.g. "Counter64", "BandwidthMBits").
//
// Enumeration resolution (EnumInteger, EnumObjectIdentifier…) is intentionally
// NOT performed here — it is the responsibility of the Producer stage, which has
// access to the enum registry. The decoder just normalises the numeric value.
func ConvertValue(rawType gosnmp.Asn1BER, rawValue interface{}, syntax string) (interface{}, error) {
	// Error sentinels — should have been filtered before this call, but guard anyway.
	if IsErrorType(rawType) {
		return nil, fmt.Errorf("skipped: PDU type is %s", PDUTypeString(rawType))
	}

	switch syntax {
	// ── Integer types ────────────────────────────────────────────────────────
	case "Integer", "Integer32", "InterfaceIndex", "InterfaceIndexOrZero",
		"TruthValue", "RowStatus", "TimeStamp", "TimeInterval",
		"EnumInteger", "EnumIntegerKeepID", "EnumBitmap":
		return toInt64(rawValue)

	// ── Unsigned / Counter types ──────────────────────────────────────────────
	case "Unsigned32", "Gauge32", "Counter32",
		"Counter64", "TimeTicks", "Opaque":
		return toUint64(rawValue)

	// ── String types ──────────────────────────────────────────────────────────
	case "DisplayString", "OctetString", "DateAndTime":
		return toDisplayString(rawValue)

	// ── Binary / byte types ───────────────────────────────────────────────────
	case "PhysAddress", "MacAddress":
		return toMACString(rawValue)

	// ── OID types ─────────────────────────────────────────────────────────────
	case "ObjectIdentifier",
		"EnumObjectIdentifier", "EnumObjectIdentifierKeepOID":
		return toOIDString(rawValue)

	// ── IP Address ────────────────────────────────────────────────────────────
	case "IpAddress", "IpAddressNoSuffix":
		return toIPString(rawValue)

	// ── Bandwidth (normalised to bits/sec) ────────────────────────────────────
	case "BandwidthBits":
		v, err := toFloat64(rawValue)
		return v, err
	case "BandwidthKBits":
		v, err := toFloat64(rawValue)
		return v * 1_000, err
	case "BandwidthMBits":
		v, err := toFloat64(rawValue)
		return v * 1_000_000, err
	case "BandwidthGBits":
		v, err := toFloat64(rawValue)
		return v * 1_000_000_000, err

	// ── Bytes (normalised to bytes) ───────────────────────────────────────────
	case "BytesB":
		return toUint64(rawValue)
	case "BytesKB":
		v, err := toFloat64(rawValue)
		return v * 1_000, err
	case "BytesMB":
		v, err := toFloat64(rawValue)
		return v * 1_000_000, err
	case "BytesGB":
		v, err := toFloat64(rawValue)
		return v * 1_000_000_000, err
	case "BytesTB":
		v, err := toFloat64(rawValue)
		return v * 1_000_000_000_000, err
	case "BytesKiB":
		v, err := toFloat64(rawValue)
		return v * 1_024, err
	case "BytesMiB":
		v, err := toFloat64(rawValue)
		return v * 1_048_576, err
	case "BytesGiB":
		v, err := toFloat64(rawValue)
		return v * 1_073_741_824, err

	// ── Temperature (normalised to Celsius, float64) ──────────────────────────
	case "TemperatureC":
		return toFloat64(rawValue)
	case "TemperatureDeciC":
		v, err := toFloat64(rawValue)
		return v / 10.0, err
	case "TemperatureCentiC":
		v, err := toFloat64(rawValue)
		return v / 100.0, err

	// ── Power (normalised to Watts) ───────────────────────────────────────────
	case "PowerWatt":
		return toFloat64(rawValue)
	case "PowerMilliWatt":
		v, err := toFloat64(rawValue)
		return v / 1_000.0, err
	case "PowerKiloWatt":
		v, err := toFloat64(rawValue)
		return v * 1_000.0, err

	// ── Current (normalised to Amps) ───────────────────────────────────────────
	case "CurrentAmp":
		return toFloat64(rawValue)
	case "CurrentMilliAmp":
		v, err := toFloat64(rawValue)
		return v / 1_000.0, err
	case "CurrentMicroAmp":
		v, err := toFloat64(rawValue)
		return v / 1_000_000.0, err

	// ── Voltage (normalised to Volts) ──────────────────────────────────────────
	case "VoltageVolt":
		return toFloat64(rawValue)
	case "VoltageMilliVolt":
		v, err := toFloat64(rawValue)
		return v / 1_000.0, err
	case "VoltageMicroVolt":
		v, err := toFloat64(rawValue)
		return v / 1_000_000.0, err

	// ── Frequency (normalised to Hz) ───────────────────────────────────────────
	case "FreqHz":
		return toFloat64(rawValue)
	case "FreqKHz":
		v, err := toFloat64(rawValue)
		return v * 1_000, err
	case "FreqMHz":
		v, err := toFloat64(rawValue)
		return v * 1_000_000, err
	case "FreqGHz":
		v, err := toFloat64(rawValue)
		return v * 1_000_000_000, err

	// ── Duration ticks (raw uint64; unit normalization is CLI-configured) ──────
	case "TicksSec", "TicksMilliSec", "TicksMicroSec":
		return toUint64(rawValue)

	// ── Percentage ─────────────────────────────────────────────────────────────
	// Percent1: value already in 0-1 range.
	// Percent100: value in 0-100; caller may re-normalise per PROCESSOR_PERCENT_NORM.
	// PercentDeci100: value in 0-1000 representing 0-100.0%.
	case "Percent1", "Percent100", "PercentDeci100":
		return toFloat64(rawValue)

	default:
		// Unknown or future syntax: fall back to a best-effort type conversion
		// based on the raw PDU type so the pipeline is not broken.
		return fallbackConvert(rawType, rawValue)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Low-level conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

// toInt64 converts the raw gosnmp value to int64.
// gosnmp returns integers as int / int32 / int64 depending on the PDU.
func toInt64(v interface{}) (int64, error) {
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
		if x > math.MaxInt64 {
			return 0, fmt.Errorf("uint64 value %d overflows int64", x)
		}
		return int64(x), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}

// toUint64 converts the raw gosnmp value to uint64.
func toUint64(v interface{}) (uint64, error) {
	switch x := v.(type) {
	case int:
		if x < 0 {
			return 0, fmt.Errorf("negative value %d cannot be converted to uint64", x)
		}
		return uint64(x), nil
	case int32:
		if x < 0 {
			return 0, fmt.Errorf("negative value %d cannot be converted to uint64", x)
		}
		return uint64(x), nil
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("negative value %d cannot be converted to uint64", x)
		}
		return uint64(x), nil
	case uint:
		return uint64(x), nil
	case uint32:
		return uint64(x), nil
	case uint64:
		return x, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to uint64", v)
	}
}

// toFloat64 widens any numeric type to float64 for unit-scale conversions.
func toFloat64(v interface{}) (float64, error) {
	switch x := v.(type) {
	case int:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

// toDisplayString converts an OctetString byte slice to a UTF-8 string, stripping
// any trailing null bytes that devices sometimes append.
func toDisplayString(v interface{}) (string, error) {
	switch x := v.(type) {
	case string:
		return strings.TrimRight(x, "\x00"), nil
	case []byte:
		return strings.TrimRight(string(x), "\x00"), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// toMACString formats a PhysAddress (6-byte) or longer octet string as a
// colon-separated hex string, e.g. "00:1a:2b:3c:4d:5e".
func toMACString(v interface{}) (string, error) {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		return fmt.Sprintf("%v", v), nil
	}

	if len(b) == 0 {
		return "", nil
	}

	// Use net.HardwareAddr formatting when it's a valid 6-byte MAC.
	if len(b) == 6 {
		return net.HardwareAddr(b).String(), nil
	}

	// Fall back to raw hex for non-standard lengths (e.g. EUI-64).
	parts := make([]string, len(b))
	for i, octet := range b {
		parts[i] = hex.EncodeToString([]byte{octet})
	}
	return strings.Join(parts, ":"), nil
}

// toOIDString returns the dotted-decimal OID string. gosnmp already returns
// ObjectIdentifier values as strings; this handles edge-cases.
func toOIDString(v interface{}) (string, error) {
	switch x := v.(type) {
	case string:
		// gosnmp includes a leading dot; normalise to no-leading-dot form.
		return strings.TrimPrefix(x, "."), nil
	case []byte:
		return strings.TrimPrefix(string(x), "."), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// toIPString converts an IpAddress value (4-byte slice or string) to dotted-
// decimal notation, e.g. "192.168.1.1".
func toIPString(v interface{}) (string, error) {
	switch x := v.(type) {
	case string:
		// gosnmp may return as a raw byte string.
		b := []byte(x)
		if len(b) == 4 {
			return net.IP(b).String(), nil
		}
		// Already dotted or IPv6.
		return x, nil
	case []byte:
		if len(x) == 4 {
			return net.IP(x).String(), nil
		}
		if len(x) == 16 {
			return net.IP(x).String(), nil
		}
		return hex.EncodeToString(x), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// fallbackConvert is a best-effort conversion used when the syntax field is
// unrecognised. It delegates to the raw PDU type to avoid losing data.
func fallbackConvert(t gosnmp.Asn1BER, v interface{}) (interface{}, error) {
	switch t {
	case gosnmp.Integer:
		return toInt64(v)
	case gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Uinteger32:
		return toUint64(v)
	case gosnmp.Counter64:
		return toUint64(v)
	case gosnmp.OctetString, gosnmp.ObjectDescription:
		return toDisplayString(v)
	case gosnmp.ObjectIdentifier:
		return toOIDString(v)
	case gosnmp.IPAddress:
		return toIPString(v)
	case gosnmp.OpaqueFloat:
		if f, ok := v.(float32); ok {
			return float64(f), nil
		}
		return toFloat64(v)
	case gosnmp.OpaqueDouble:
		return toFloat64(v)
	default:
		// Last resort: return raw bytes if available, else stringify.
		if b, ok := v.([]byte); ok {
			return b, nil
		}
		return fmt.Sprintf("%v", v), nil
	}
}
