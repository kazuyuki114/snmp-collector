// Package stdout implements a Transport plugin that writes formatted messages
// to os.Stdout. Each Send call writes one JSON record followed by a newline.
//
// This is the default development/debugging transport — zero configuration
// required.
package stdout

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"snmp/snmp-collector/internal/noop"
	"snmp/snmp-collector/plugin"
)

// compile-time interface check.
var _ plugin.Transport = (*Transport)(nil)

// Transport writes formatted byte slices to os.Stdout. It is safe for
// concurrent use — a mutex serialises writes to prevent interleaving.
type Transport struct {
	mu     sync.Mutex
	w      io.Writer
	nl     []byte
	logger *slog.Logger
}

// Config controls Stdout transport behaviour.
type Config struct {
	// Writer overrides the output destination. nil defaults to os.Stdout.
	// Exposed for testing.
	Writer io.Writer

	// Newline is appended after each message. Default "\n".
	Newline string
}

// New constructs a Stdout Transport.
func New(cfg Config, logger *slog.Logger) *Transport {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	w := cfg.Writer
	if w == nil {
		w = os.Stdout
	}
	nl := cfg.Newline
	if nl == "" {
		nl = "\n"
	}
	return &Transport{
		w:      w,
		nl:     []byte(nl),
		logger: logger,
	}
}

// Name implements plugin.Transport.
func (t *Transport) Name() string { return "stdout" }

// Send implements plugin.Transport. It writes data + newline to stdout,
// holding a mutex so concurrent goroutines produce un-interleaved output.
func (t *Transport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.w.Write(data); err != nil {
		t.logger.Error("stdout: write failed", "error", err.Error(), "bytes", len(data))
		return fmt.Errorf("stdout: write: %w", err)
	}
	if _, err := t.w.Write(t.nl); err != nil {
		t.logger.Error("stdout: newline write failed", "error", err.Error())
		return fmt.Errorf("stdout: write newline: %w", err)
	}

	t.logger.Debug("stdout: sent message", "bytes", len(data))
	return nil
}

// Close implements plugin.Transport. It is a no-op — os.Stdout is not owned
// by this transport.
func (t *Transport) Close() error { return nil }

