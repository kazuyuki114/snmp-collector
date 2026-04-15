package config

// DeviceConfig is the fully-resolved configuration for a single monitored device.
// Optional fields that are zero-valued in the YAML are filled with hard-coded
// fallbacks during resolution.
type DeviceConfig struct {
	// IP is the management IP address of the device.
	IP string

	// Port is the UDP port for SNMP requests (default 161).
	Port int

	// PollInterval is the polling interval in seconds (default 60).
	PollInterval int

	// Timeout is the per-request timeout in milliseconds (default 3000).
	Timeout int

	// Retries is the number of retry attempts on timeout (default 2).
	Retries int

	// ExponentialTimeout enables exponential backoff between retries.
	ExponentialTimeout bool

	// Version is the SNMP version: "1", "2c", or "3".
	Version string

	// Communities is the list of community strings to try (v1/v2c only).
	Communities []string

	// V3Credentials is the list of SNMPv3 credential sets to try (v3 only).
	V3Credentials []V3Credentials

	// DeviceGroups lists the device group names applied to this device.
	DeviceGroups []string

	// MaxConcurrentPolls limits how many concurrent SNMP requests may be
	// in-flight to this device at any time (default 4).
	MaxConcurrentPolls int

	// UseGetBulk controls whether BulkWalk (GETBULK) is used for table objects
	// on v2c/v3 devices. Set to false to fall back to regular Walk (GETNEXT),
	// which is more compatible with devices like Windows SNMP service that
	// cannot handle large bulk responses. Default true.
	UseGetBulk bool
}

// V3Credentials holds a single set of SNMPv3 security parameters.
type V3Credentials struct {
	// Username is the SNMPv3 security name.
	Username string `yaml:"username"`

	// AuthenticationProtocol is one of: noauth, md5, sha (aliases: sha1, sha128),
	// sha224, sha256, sha384, sha512.
	AuthenticationProtocol string `yaml:"authentication_protocol"`

	// AuthenticationPassphrase is the passphrase for the chosen auth protocol.
	AuthenticationPassphrase string `yaml:"authentication_passphrase"`

	// PrivacyProtocol is one of: nopriv, des (alias: des56), aes (alias: aes128),
	// aes192, aes256, aes192c, aes256c.
	PrivacyProtocol string `yaml:"privacy_protocol"`

	// PrivacyPassphrase is the passphrase for the chosen privacy protocol.
	PrivacyPassphrase string `yaml:"privacy_passphrase"`
}

// DeviceGroup lists the object group names applied to devices in this group.
type DeviceGroup struct {
	ObjectGroups []string
}

// ObjectGroup lists the object definition keys that belong to this group.
type ObjectGroup struct {
	Objects []string
}

// rawDeviceEntry is the intermediate YAML-decoded form of a single device.
// It maps 1-to-1 with the device YAML schema. Hard-coded fallbacks are applied
// for zero-valued fields during resolution.
type rawDeviceEntry struct {
	IP                 string          `yaml:"ip"`
	Port               int             `yaml:"port"`
	PollInterval       int             `yaml:"poll_interval"`
	Timeout            int             `yaml:"timeout"`
	Retries            int             `yaml:"retries"`
	ExponentialTimeout bool            `yaml:"exponential_timeout"`
	Version            string          `yaml:"version"`
	Communities        []string        `yaml:"communities"`
	V3Credentials      []V3Credentials `yaml:"v3_credentials"`
	DeviceGroups       []string        `yaml:"device_groups"`
	MaxConcurrentPolls int             `yaml:"max_concurrent_polls"`
	UseGetBulk         *bool           `yaml:"use_getbulk"`
}
