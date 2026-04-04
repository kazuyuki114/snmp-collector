// Package kafka implements a [plugin.Transport] that publishes formatted
// messages to Apache Kafka using a sarama SyncProducer.
//
// Architecture overview
//
//	Send([]byte) ──► buffered channel ──► worker goroutine ──► sarama.SendMessages
//	                 (BufferSize)          (owns batch slice)    (MaxEvents | FlushInterval)
//
// The transport starts a single background worker in [New].  That worker is
// the only goroutine that ever touches the batch slice, so no mutex is needed
// on the hot path.  Concurrent callers of [Transport.Send] only write into the
// buffered channel.
//
// Shutdown sequence (triggered by [Transport.Close]):
//  1. Close the quit channel — unblocks any Send callers waiting for space and
//     signals the worker to stop accepting new messages.
//  2. Worker drains whatever is left in the channel (non-blocking), flushes the
//     final batch, then closes the done channel.
//  3. Close blocks on done, then tears down the sarama producer.
package kafka

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"

	"snmp/snmp-collector/plugin"
)

// compile-time interface check.
var _ plugin.Transport = (*Transport)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// Config holds all tunable parameters for the Kafka transport.
type Config struct {
	// Brokers is the list of bootstrap broker addresses ("host:port").
	// At least one broker is required.
	Brokers []string

	// Topic is the Kafka topic to publish metrics to.
	Topic string

	// MaxEvents is the maximum number of messages accumulated in a batch
	// before the batch is force-flushed regardless of FlushInterval.
	// Default: 1000.
	MaxEvents int

	// FlushInterval is the maximum time a batch can age before it is
	// flushed even when MaxEvents has not been reached.  This ensures
	// low-volume periods do not leave stale data sitting in memory.
	// Default: 5s.
	FlushInterval time.Duration

	// BufferSize is the capacity of the internal channel that decouples
	// Send callers from the batching worker.  When the channel is full,
	// Send blocks, providing natural back-pressure to the pipeline.
	// Default: 10 000 (matches the app pipeline default).
	BufferSize int

	// ClientID identifies this producer to the Kafka brokers.
	// Default: "snmp-collector".
	ClientID string

	// RequiredAcks controls the producer acknowledgement guarantee.
	//   "none"  – fire and forget (fastest, no durability guarantee)
	//   "local" – wait for the partition leader to acknowledge (default)
	//   "all"   – wait for all in-sync replicas to acknowledge (safest)
	RequiredAcks string

	// MaxRetry is the number of times sarama will retry a failed send
	// before surfacing the error to the transport.  Sarama handles the
	// retry internally with exponential back-off.
	// Default: 3.
	MaxRetry int

	// Compression algorithm applied to message payloads.
	// Valid values: "none", "gzip", "snappy", "lz4", "zstd".
	// Default: "none".
	Compression string

	// Version is the Kafka protocol version to negotiate (e.g. "3.6.0").
	// When empty, sarama uses its built-in default (oldest stable).
	Version string

	// TLS holds mTLS settings.  Nil disables TLS.
	TLS *TLSConfig

	// SASL holds authentication settings.  Nil disables SASL.
	SASL *SASLConfig
}

// TLSConfig holds TLS/mTLS options for the broker connection.
type TLSConfig struct {
	// Enable activates TLS.
	Enable bool

	// InsecureSkipVerify disables server certificate verification.
	// Never set this in production.
	InsecureSkipVerify bool

	// CACert is the path to a PEM-encoded CA certificate used to verify
	// the broker's certificate.
	CACert string

	// ClientCert and ClientKey are paths to PEM-encoded files used for
	// mutual TLS authentication.  Both must be provided together.
	ClientCert string
	ClientKey  string
}

// SASLConfig holds SASL authentication options.
type SASLConfig struct {
	// Enable activates SASL.
	Enable bool

	// User and Password are the SASL credentials.
	User     string
	Password string

	// Mechanism selects the SASL exchange.
	// Valid values: "PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512".
	// Default: "PLAIN".
	Mechanism string
}

// defaults for optional Config fields.
const (
	defaultMaxEvents     = 1000
	defaultFlushInterval = 5 * time.Second
	defaultBufferSize    = 10_000
	defaultClientID      = "snmp-collector"
	defaultMaxRetry      = 3
)

func applyDefaults(c *Config) {
	if c.MaxEvents <= 0 {
		c.MaxEvents = defaultMaxEvents
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = defaultFlushInterval
	}
	if c.BufferSize <= 0 {
		c.BufferSize = defaultBufferSize
	}
	if c.ClientID == "" {
		c.ClientID = defaultClientID
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = defaultMaxRetry
	}
	if c.RequiredAcks == "" {
		c.RequiredAcks = "local"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Transport
// ─────────────────────────────────────────────────────────────────────────────

// Transport implements [plugin.Transport] by writing batched messages to
// Kafka via a sarama SyncProducer.  It is safe for concurrent use.
type Transport struct {
	cfg      Config
	producer sarama.SyncProducer
	logger   *slog.Logger

	// ch is the buffered channel that decouples Send from the worker.
	// It is never closed; items are drained by the worker on shutdown.
	ch chan []byte

	// quit is closed by Close to signal the worker to begin shutdown.
	quit chan struct{}

	// done is closed by the worker once it has flushed and exited.
	done chan struct{}

	closeOnce sync.Once
}

// New creates a Kafka transport and starts the background batching worker.
// The caller must eventually call [Transport.Close] to flush pending data and
// release resources.
func New(cfg Config, logger *slog.Logger) (*Transport, error) {
	applyDefaults(&cfg)

	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafka transport: at least one broker address is required")
	}
	if cfg.Topic == "" {
		return nil, errors.New("kafka transport: topic is required")
	}

	sc, err := buildSaramaConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kafka transport: %w", err)
	}

	producer, err := sarama.NewSyncProducer(cfg.Brokers, sc)
	if err != nil {
		return nil, fmt.Errorf("kafka transport: connecting to brokers %v: %w", cfg.Brokers, err)
	}

	t := &Transport{
		cfg:      cfg,
		producer: producer,
		logger:   logger,
		ch:       make(chan []byte, cfg.BufferSize),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	go t.worker()

	logger.Info("kafka transport: ready",
		slog.Any("brokers", cfg.Brokers),
		slog.String("topic", cfg.Topic),
		slog.Int("max_events", cfg.MaxEvents),
		slog.Duration("flush_interval", cfg.FlushInterval),
		slog.Int("buffer_size", cfg.BufferSize),
	)

	return t, nil
}

// Name implements [plugin.Transport].
func (t *Transport) Name() string { return "kafka" }

// Send enqueues a pre-formatted message for batched delivery to Kafka.
//
// It blocks when the internal buffer is full, providing natural back-pressure
// to the upstream pipeline.  It returns immediately with an error when the
// transport has been closed.
//
// Safe for concurrent use by multiple goroutines.
func (t *Transport) Send(data []byte) error {
	// Copy the slice: sarama holds a reference to it until the broker
	// acknowledges the message, so we must not let the caller reuse the buffer.
	msg := make([]byte, len(data))
	copy(msg, data)

	select {
	case t.ch <- msg:
		return nil
	case <-t.quit:
		return errors.New("kafka transport: transport is closed")
	}
}

// Close signals the batching worker to flush whatever remains in the batch to
// Kafka and then shuts down the producer.  It blocks until the flush is
// complete.  Safe to call exactly once (additional calls are no-ops).
func (t *Transport) Close() error {
	var producerErr error
	t.closeOnce.Do(func() {
		// Signal: (a) unblock any Send callers waiting for channel space,
		//         (b) tell the worker to begin draining and exit.
		close(t.quit)

		// Wait for the worker to drain the channel and flush the last batch.
		<-t.done

		// Tear down the sarama producer only after the worker has finished.
		// AsyncClose would be fine too, but SyncClose keeps the error path simple.
		if err := t.producer.Close(); err != nil {
			producerErr = fmt.Errorf("kafka transport: closing producer: %w", err)
			t.logger.Error("kafka transport: error closing producer", slog.String("error", err.Error()))
		} else {
			t.logger.Info("kafka transport: closed")
		}
	})
	return producerErr
}

// ─────────────────────────────────────────────────────────────────────────────
// Background worker
// ─────────────────────────────────────────────────────────────────────────────

// worker is the single goroutine that owns the batch slice.  Because it is the
// only writer/reader of batch, no mutex is required.
//
// Two flush conditions:
//  1. len(batch) >= cfg.MaxEvents  — size limit reached
//  2. ticker fires                 — stale batch age reached
//
// On shutdown (quit closed):
//  - drain the channel non-blocking (pick up messages already buffered)
//  - flush the final batch
//  - close done to unblock Close()
func (t *Transport) worker() {
	defer close(t.done)

	ticker := time.NewTicker(t.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]*sarama.ProducerMessage, 0, t.cfg.MaxEvents)

	for {
		select {

		case data := <-t.ch:
			batch = append(batch, t.makeMessage(data))
			if len(batch) >= t.cfg.MaxEvents {
				t.flush(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				t.flush(batch)
				batch = batch[:0]
			}

		case <-t.quit:
			// Drain any messages already buffered in the channel before
			// shutting down.  This is a non-blocking loop; we stop as
			// soon as the channel is empty.
			for {
				select {
				case data := <-t.ch:
					batch = append(batch, t.makeMessage(data))
				default:
					// Channel is empty — flush remaining batch and exit.
					if len(batch) > 0 {
						t.flush(batch)
					}
					return
				}
			}
		}
	}
}

// makeMessage wraps a raw byte slice in a sarama.ProducerMessage.
// The topic comes from the transport config; no partition key is set,
// so Kafka round-robins messages across partitions.
func (t *Transport) makeMessage(data []byte) *sarama.ProducerMessage {
	return &sarama.ProducerMessage{
		Topic: t.cfg.Topic,
		Value: sarama.ByteEncoder(data),
	}
}

// flush sends a batch of messages to Kafka in a single SendMessages call and
// logs the outcome.  On partial failure, each per-message error is logged
// individually.
func (t *Transport) flush(batch []*sarama.ProducerMessage) {
	if err := t.producer.SendMessages(batch); err != nil {
		// sarama may return a ProducerErrors that contains per-message
		// failures alongside successfully delivered messages in the same
		// batch.  Unwrap and log each failure so we know which messages
		// were lost.
		var perMsgErrs sarama.ProducerErrors
		if errors.As(err, &perMsgErrs) {
			for _, pe := range perMsgErrs {
				t.logger.Error("kafka transport: message delivery failed",
					slog.String("topic", pe.Msg.Topic),
					slog.String("error", pe.Err.Error()),
				)
			}
			t.logger.Error("kafka transport: batch partially failed",
				slog.Int("batch_size", len(batch)),
				slog.Int("failed", len(perMsgErrs)),
			)
		} else {
			// Wholesale batch failure (e.g. producer closed, network down).
			t.logger.Error("kafka transport: batch send failed",
				slog.Int("batch_size", len(batch)),
				slog.String("error", err.Error()),
			)
		}
		return
	}

	t.logger.Debug("kafka transport: batch flushed",
		slog.Int("messages", len(batch)),
		slog.String("topic", t.cfg.Topic),
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Sarama configuration builder
// ─────────────────────────────────────────────────────────────────────────────

func buildSaramaConfig(cfg Config) (*sarama.Config, error) {
	sc := sarama.NewConfig()
	sc.ClientID = cfg.ClientID

	// Kafka protocol version.
	if cfg.Version != "" {
		v, err := sarama.ParseKafkaVersion(cfg.Version)
		if err != nil {
			return nil, fmt.Errorf("invalid kafka version %q: %w", cfg.Version, err)
		}
		sc.Version = v
	}

	// Producer settings.
	// SyncProducer requires both Return.Successes and Return.Errors = true.
	sc.Producer.Return.Successes = true
	sc.Producer.Return.Errors = true
	sc.Producer.Retry.Max = cfg.MaxRetry

	switch strings.ToLower(cfg.RequiredAcks) {
	case "none":
		sc.Producer.RequiredAcks = sarama.NoResponse
	case "all":
		sc.Producer.RequiredAcks = sarama.WaitForAll
	default: // "local"
		sc.Producer.RequiredAcks = sarama.WaitForLocal
	}

	switch strings.ToLower(cfg.Compression) {
	case "gzip":
		sc.Producer.Compression = sarama.CompressionGZIP
	case "snappy":
		sc.Producer.Compression = sarama.CompressionSnappy
	case "lz4":
		sc.Producer.Compression = sarama.CompressionLZ4
	case "zstd":
		sc.Producer.Compression = sarama.CompressionZSTD
	default: // "none"
		sc.Producer.Compression = sarama.CompressionNone
	}

	// TLS.
	if cfg.TLS != nil && cfg.TLS.Enable {
		tlsCfg, err := buildTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		sc.Net.TLS.Enable = true
		sc.Net.TLS.Config = tlsCfg
	}

	// SASL.
	if cfg.SASL != nil && cfg.SASL.Enable {
		if err := applySASL(sc, cfg.SASL); err != nil {
			return nil, err
		}
	}

	return sc, nil
}

func buildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // user-opt-in
	}

	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert %q: %w", cfg.CACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parsing CA cert %q: no valid certificates found", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		if cfg.ClientCert == "" || cfg.ClientKey == "" {
			return nil, errors.New("TLS: both client_cert and client_key must be set together")
		}
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate key pair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

func applySASL(sc *sarama.Config, cfg *SASLConfig) error {
	sc.Net.SASL.Enable = true
	sc.Net.SASL.User = cfg.User
	sc.Net.SASL.Password = cfg.Password

	switch strings.ToUpper(cfg.Mechanism) {
	case "SCRAM-SHA-256":
		sc.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
		sc.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
			return &scramClient{HashGeneratorFcn: sha256HashGenerator}
		}
	case "SCRAM-SHA-512":
		sc.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
		sc.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
			return &scramClient{HashGeneratorFcn: sha512HashGenerator}
		}
	case "PLAIN", "":
		sc.Net.SASL.Mechanism = sarama.SASLTypePlaintext
	default:
		return fmt.Errorf("unsupported SASL mechanism %q (valid: PLAIN, SCRAM-SHA-256, SCRAM-SHA-512)", cfg.Mechanism)
	}

	return nil
}
