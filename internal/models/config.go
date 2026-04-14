package models

// ObjectDefinition is the parsed form of a single object YAML file
// (e.g. IF-MIB_ifEntry.yml). The decoder uses this to map a raw OID from the
// SNMP PDU back to a human-readable name and syntax type.
type ObjectDefinition struct {
	// Key is the SNMP object identifier, e.g. "IF-MIB::ifEntry".
	Key string

	// MIB is the MIB module name, e.g. "IF-MIB".
	MIB string

	// Object is the SNMP object name within the MIB, e.g. "ifEntry".
	Object string

	// Augments, when non-empty, references another ObjectDefinition whose index
	// this object shares, e.g. "IF-MIB::ifEntry".
	Augments string

	// Index describes the table index components in declaration order.
	// Scalar objects have an empty slice.
	Index []IndexDefinition

	// DiscoveryAttribute names the attribute used to detect whether a table row
	// is present (e.g. "ifDescr"). Empty for non-discovery objects.
	DiscoveryAttribute string

	// Attributes is the full set of SNMP attributes (columns) within this object.
	// Keyed by the SNMP attribute name, e.g. "ifInOctets".
	Attributes map[string]AttributeDefinition
}

// IndexDefinition describes a single component of a table's OID index.
type IndexDefinition struct {
	// Type is the OID index encoding, e.g. "Integer", "IpAddress", "OctetString".
	// See the Object Index Types section of the architecture document.
	Type string

	// OID is the numeric OID of the index object, e.g. ".1.3.6.1.2.1.2.2.1.1".
	OID string

	// Name is the semantic name assigned to this index in the output, e.g. "netif".
	Name string

	// Syntax is the display/conversion hint for the index value.
	Syntax string
}

// AttributeDefinition describes a single column within an SNMP table (or a scalar
// field). This is the resolved form of each item under `attributes:` in the
// objects YAML.
type AttributeDefinition struct {
	// OID is the full numeric OID of the attribute, e.g. ".1.3.6.1.2.1.2.2.1.10".
	OID string

	// Name is the metric name used in the output, e.g. "netif.bytes.in".
	Name string

	// Syntax controls how the raw SNMP value is converted and normalized.
	// See the Syntax Types section of the architecture document.
	Syntax string

	// IsTag, when true, means this attribute is a dimension label rather than a
	// numeric metric. Its value is stored in Metric.Tags of sibling metrics.
	IsTag bool

	// Overrides, when set, identifies the object+attribute that this attribute
	// replaces (e.g. Counter64 overriding the 32-bit Counter32 version).
	Overrides *OverrideReference

	// Rediscover controls when an index entry triggers re-discovery.
	// Valid values: "", "OnChange", "OnReset".
	Rediscover string
}

// OverrideReference identifies the object+attribute that a newer attribute supersedes.
type OverrideReference struct {
	// Object is the ObjectDefinition key, e.g. "IF-MIB::ifEntry".
	Object string

	// Attribute is the SNMP attribute name within Object, e.g. "ifInOctets".
	Attribute string
}
