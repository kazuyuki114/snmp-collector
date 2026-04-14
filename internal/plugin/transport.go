package plugin

// Transport is the interface that every output/delivery plugin must implement.
//
// Stdout and File are the initial implementations. Future transports (e.g.
// Kafka, InfluxDB, Prometheus remote-write) implement this same contract so
// the engine can fan-out formatted data to all of them uniformly.
//
// The engine calls Send concurrently from one or more transport-stage
// goroutines, so implementations MUST be safe for concurrent use.
//
// Lifecycle:
//
//  1. The engine constructs the transport and begins calling Send with
//     formatted byte slices (one per Envelope, already serialised by the
//     formatter stage).
//  2. On shutdown the engine calls Close after all Send calls have returned.
//     Close flushes buffered data and releases resources.
type Transport interface {
	// Name returns a short, unique, human-readable identifier for this
	// transport plugin instance (e.g. "stdout", "file", "kafka").
	// Used in log messages and metrics labels.
	Name() string

	// Send delivers a single pre-formatted message (e.g. a JSON byte slice
	// produced by format/json) to the transport destination. It is called
	// from transport-stage worker goroutines and must be safe for concurrent
	// use.
	//
	// Transient errors (network blip, disk full) should be retried
	// internally up to a reasonable limit before returning an error.
	// Permanent errors (invalid config) should be returned immediately.
	Send(data []byte) error

	// Close flushes any pending/buffered data and releases resources
	// (connections, file handles, etc.). It is called exactly once during
	// shutdown after all Send calls have completed. After Close returns the
	// transport will not receive further Send calls.
	Close() error
}
