package config

// collector_config.go defines the top-level YAML configuration file structure
// for the collector process itself — distinct from the SNMP device/object
// definition trees loaded by loader.go.
//
// Precedence (highest wins):
//
//  1. CLI flags
//  2. YAML config file  (-config /path/to/config.yaml)
//  3. Environment variables  (config_paths only, via PathsFromEnv)
//  4. Hardcoded defaults  (see DefaultCollectorConfig)
//
// Example file:
//
//	log:
//	  level: info
//	  format: json
//
//	collector_id: "dc1-collector-01"
//
//	pipeline:
//	  buffer_size: 10000
//	  decode_workers: 1
//	  produce_workers: 1
//
//	poller:
//	  workers: 500
//	  pool:
//	    max_idle: 2
//	    idle_timeout: 30s
//
//	processors:
//	  enum:
//	    enable: false
//	  counter:
//	    delta_enable: true
//	    purge_interval: 5m
//
//	format:
//	  pretty: false
//
//	config_reload_interval: 0s
//
//	# Omit config_paths to rely on environment variables.
//	config_paths:
//	  devices:       /etc/snmp_collector/snmp/devices
//	  device_groups: /etc/snmp_collector/snmp/device_groups
//	  object_groups: /etc/snmp_collector/snmp/object_groups
//	  objects:       /etc/snmp_collector/snmp/objects
//	  enums:         /etc/snmp_collector/snmp/enums
//
//	health:
//	  addr: ":8080"
//
//	output:
//	  # Only one output is active.
//	  # Kafka takes precedence if brokers is non-empty; then file; then stdout.
//	  send_max_retry: 3      # retries per failed Send before dropping (0 = none)
//	  send_retry_delay: 1s   # pause between retries
//
//	  kafka:
//	    brokers:
//	      - broker1:9092
//	      - broker2:9092
//	    topic: snmp-metrics
//	    max_events: 1000
//	    flush_interval: 5s
//	    buffer_size: 10000
//	    client_id: snmp-collector
//	    required_acks: local   # none | local | all
//	    max_retry: 3
//	    compression: none      # none | gzip | snappy | lz4 | zstd
//	    version: ""            # e.g. "3.6.0"; empty = auto
//	    tls:
//	      enable: false
//	      insecure_skip_verify: false
//	      ca_cert: ""
//	      client_cert: ""
//	      client_key: ""
//	    sasl:
//	      enable: false
//	      user: ""
//	      password: ""
//	      mechanism: PLAIN     # PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
//
//	  file:
//	    path: /var/log/snmpcollector/metrics.json
//	    max_bytes: 52428800    # 50 MB
//	    max_backups: 5

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// Top-level struct
// ─────────────────────────────────────────────────────────────────────────────

// CollectorConfig is the parsed form of the collector's own YAML config file.
type CollectorConfig struct {
	Log struct {
		Level  string `yaml:"level"`  // debug|info|warn|error
		Format string `yaml:"format"` // json|text
	} `yaml:"log"`

	// CollectorID identifies this instance in output metadata.
	// Defaults to the system hostname when empty.
	CollectorID string `yaml:"collector_id"`

	// MaxProcs sets the number of OS threads the Go runtime may use (GOMAXPROCS).
	// 0 means use all available CPU cores (default Go behaviour).
	MaxProcs int `yaml:"max_procs"`

	Pipeline struct {
		BufferSize     int `yaml:"buffer_size"`
		DecodeWorkers  int `yaml:"decode_workers"`
		ProduceWorkers int `yaml:"produce_workers"`
	} `yaml:"pipeline"`

	Poller struct {
		Workers      int `yaml:"workers"`
		JobQueueSize int `yaml:"job_queue_size"`
		Pool         struct {
			MaxIdle     int    `yaml:"max_idle"`
			IdleTimeout string `yaml:"idle_timeout"` // e.g. "30s"
		} `yaml:"pool"`
	} `yaml:"poller"`

	Processors struct {
		Enum struct {
			Enable bool `yaml:"enable"`
		} `yaml:"enum"`
		Counter struct {
			DeltaEnable   bool   `yaml:"delta_enable"`
			PurgeInterval string `yaml:"purge_interval"` // e.g. "5m"
		} `yaml:"counter"`
	} `yaml:"processors"`

	Format struct {
		Pretty bool `yaml:"pretty"`
	} `yaml:"format"`

	// ConfigPaths overrides the env-variable-driven data directories.
	// An empty string means "fall back to the environment variable".
	ConfigPaths struct {
		Devices      string `yaml:"devices"`
		DeviceGroups string `yaml:"device_groups"`
		ObjectGroups string `yaml:"object_groups"`
		Objects      string `yaml:"objects"`
		Enums        string `yaml:"enums"`
	} `yaml:"config_paths"`

	// ConfigReloadInterval sets how often data directories are re-scanned.
	// "0s" or "" disables reload.
	ConfigReloadInterval string `yaml:"config_reload_interval"`

	Health struct {
		Addr string `yaml:"addr"` // e.g. ":8080"; empty disables
	} `yaml:"health"`

	// HA configures the Active/Standby High Availability manager.
	// When enabled, the manager drives app start/stop; the app is not started
	// directly on launch. Set enabled: false (the default) to run as a
	// standalone collector without HA.
	HA CollectorHAConfig `yaml:"ha"`

	Output struct {
		Stdout CollectorStdoutConfig `yaml:"stdout"`
		File   CollectorFileConfig   `yaml:"file"`
		Kafka  CollectorKafkaConfig  `yaml:"kafka"`

		// SendMaxRetry is the number of times to retry a failed Send before
		// dropping the message. The program keeps running; no fallback transport
		// is used.  Default: 3.
		SendMaxRetry int `yaml:"send_max_retry"`

		// SendRetryDelay is the pause between Send retries. e.g. "1s", "500ms".
		// Default: "1s".
		SendRetryDelay string `yaml:"send_retry_delay"`
	} `yaml:"output"`
}

// CollectorHAConfig holds settings for the Active/Standby HA manager.
//
// Example YAML:
//
//	ha:
//	  enabled: true
//	  role: primary              # primary | standby
//	  peer_url: "http://10.0.0.2:9080"
//	  health_check_interval: 5s
//	  health_check_timeout: 5s
//	  failover_threshold: 3
//	  demote_timeout: 30s
type CollectorHAConfig struct {
	Enabled bool `yaml:"enabled"`

	// Role is "primary" (DC, preferred) or "standby" (DR).
	Role string `yaml:"role"`

	// PeerURL is the base HTTP URL of the peer node, e.g. "http://10.0.0.2:9080".
	PeerURL string `yaml:"peer_url"`

	// HealthCheckInterval is how often the Standby polls GET /health on the peer.
	// e.g. "5s". Default: "5s".
	HealthCheckInterval string `yaml:"health_check_interval"`

	// HealthCheckTimeout is the per-request HTTP timeout for health checks.
	// e.g. "5s". Default: "5s".
	HealthCheckTimeout string `yaml:"health_check_timeout"`

	// FailoverThreshold is the number of consecutive health-check failures
	// before the Standby promotes itself to Active. Default: 3.
	FailoverThreshold int `yaml:"failover_threshold"`

	// DemoteTimeout is the HTTP client timeout for the POST /demote call
	// the Primary sends to the Standby on startup. Must be long enough for
	// the Standby's pipeline to fully drain. e.g. "30s". Default: "30s".
	DemoteTimeout string `yaml:"demote_timeout"`
}

// CollectorStdoutConfig holds settings for the stdout output transport.
type CollectorStdoutConfig struct {
	Enabled bool `yaml:"enabled"`
}

// CollectorFileConfig holds settings for the file output transport.
type CollectorFileConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Path       string `yaml:"path"`
	MaxBytes   int64  `yaml:"max_bytes"`
	MaxBackups int    `yaml:"max_backups"`
}

// CollectorKafkaConfig holds settings for the Kafka output transport.
type CollectorKafkaConfig struct {
	Enabled       bool               `yaml:"enabled"`
	Brokers       []string           `yaml:"brokers"`
	Topic         string             `yaml:"topic"`
	MaxEvents     int                `yaml:"max_events"`
	FlushInterval string             `yaml:"flush_interval"`
	BufferSize    int                `yaml:"buffer_size"`
	ClientID      string             `yaml:"client_id"`
	RequiredAcks  string             `yaml:"required_acks"` // none|local|all
	MaxRetry      int                `yaml:"max_retry"`
	Compression   string             `yaml:"compression"` // none|gzip|snappy|lz4|zstd
	Version       string             `yaml:"version"`
	TLS           CollectorKafkaTLS  `yaml:"tls"`
	SASL          CollectorKafkaSASL `yaml:"sasl"`
}

// CollectorKafkaTLS holds mTLS settings for the Kafka transport.
type CollectorKafkaTLS struct {
	Enable             bool   `yaml:"enable"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CACert             string `yaml:"ca_cert"`
	ClientCert         string `yaml:"client_cert"`
	ClientKey          string `yaml:"client_key"`
}

// CollectorKafkaSASL holds SASL authentication settings for the Kafka transport.
type CollectorKafkaSASL struct {
	Enable    bool   `yaml:"enable"`
	User      string `yaml:"user"`
	Password  string `yaml:"password"`
	Mechanism string `yaml:"mechanism"` // PLAIN|SCRAM-SHA-256|SCRAM-SHA-512
}

// ─────────────────────────────────────────────────────────────────────────────
// Constructor and loader
// ─────────────────────────────────────────────────────────────────────────────

// DefaultCollectorConfig returns a CollectorConfig with all fields set to the
// same hardcoded defaults as the CLI flags. It is used as the merge base when
// loading a YAML file so that omitted keys keep sensible values.
func DefaultCollectorConfig() *CollectorConfig {
	c := &CollectorConfig{}
	c.Log.Level = "info"
	c.Log.Format = "json"
	c.MaxProcs = 0 // 0 = all CPUs (runtime default)
	c.Pipeline.BufferSize = 10_000
	c.Pipeline.DecodeWorkers = 1
	c.Pipeline.ProduceWorkers = 1
	c.Poller.Workers = 500
	c.Poller.Pool.MaxIdle = 2
	c.Poller.Pool.IdleTimeout = "30s"
	c.Processors.Counter.DeltaEnable = true
	c.Processors.Counter.PurgeInterval = "5m"
	c.ConfigReloadInterval = "0s"
	c.HA.HealthCheckInterval = "5s"
	c.HA.HealthCheckTimeout = "5s"
	c.HA.FailoverThreshold = 3
	c.HA.DemoteTimeout = "30s"
	c.Output.File.MaxBytes = 50 * 1024 * 1024
	c.Output.File.MaxBackups = 5
	c.Output.Kafka.MaxEvents = 1_000
	c.Output.Kafka.FlushInterval = "5s"
	c.Output.Kafka.BufferSize = 10_000
	c.Output.Kafka.ClientID = "snmp-collector"
	c.Output.Kafka.RequiredAcks = "local"
	c.Output.Kafka.MaxRetry = 3
	c.Output.Kafka.Compression = "none"
	c.Output.Kafka.SASL.Mechanism = "PLAIN"
	c.Output.SendMaxRetry = 3
	c.Output.SendRetryDelay = "1s"
	return c
}

// LoadCollectorConfig reads path as YAML and merges it on top of
// DefaultCollectorConfig so that any key absent from the file keeps its
// default value.
func LoadCollectorConfig(path string) (*CollectorConfig, error) {
	c := DefaultCollectorConfig()

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("collector config: open %q: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(false) // tolerate unknown keys so the file can have comments/extras
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("collector config: parse %q: %w", path, err)
	}
	return c, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Duration helper
// ─────────────────────────────────────────────────────────────────────────────

// ParseDuration parses a Go duration string (e.g. "5s", "30s", "5m").
// An empty string or "0" is treated as zero (disabled) without an error,
// matching the CLI convention where 0 means disabled.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" || s == "0s" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}
