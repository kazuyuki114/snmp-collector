// Package file implements a Transport plugin that writes formatted messages
// to a file on disk with optional size-based rotation.
//
// The plugin is fully self-contained: rotation logic lives in rotate.go within
// this package.  It satisfies the [plugin.Transport] interface so the engine
// treats it identically to every other transport.
package file

import (
	"fmt"
	"log/slog"
	"sync"

	"snmp/snmp-collector/internal/noop"
	"snmp/snmp-collector/plugin"
)

// compile-time interface check.
var _ plugin.Transport = (*Transport)(nil)

// Config controls the File transport.
type Config struct {
	// FilePath is the output file (required).
	FilePath string

	// MaxBytes triggers file rotation when the active file exceeds this size.
	// Zero disables rotation.
	MaxBytes int64

	// MaxBackups is the number of rotated backup files to keep. Zero keeps all.
	MaxBackups int

	// Newline is appended after each message. Default "\n".
	Newline string
}

// Transport writes formatted byte slices to a file with optional rotation.
// It is safe for concurrent use.
type Transport struct {
	mu     sync.Mutex
	rf     *RotatingFile
	nl     []byte
	logger *slog.Logger
}

// New constructs a File Transport. Returns an error if the file cannot be
// opened or the parent directory cannot be created.
func New(cfg Config, logger *slog.Logger) (*Transport, error) {
	if cfg.FilePath == "" {
		return nil, fmt.Errorf("file transport: FilePath is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	nl := cfg.Newline
	if nl == "" {
		nl = "\n"
	}

	rf, err := NewRotatingFile(RotateConfig{
		FilePath:   cfg.FilePath,
		MaxBytes:   cfg.MaxBytes,
		MaxBackups: cfg.MaxBackups,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("file transport: %w", err)
	}

	logger.Info("file transport: configured",
		"path", cfg.FilePath,
		"max_bytes", cfg.MaxBytes,
		"max_backups", cfg.MaxBackups,
	)

	return &Transport{
		rf:     rf,
		nl:     []byte(nl),
		logger: logger,
	}, nil
}

// Name implements plugin.Transport.
func (t *Transport) Name() string { return "file" }

// Send implements plugin.Transport. It writes data + newline to the underlying
// rotating file. Safe for concurrent use.
func (t *Transport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.rf.Write(data); err != nil {
		t.logger.Error("file transport: write failed", "error", err.Error(), "bytes", len(data))
		return fmt.Errorf("file transport: write: %w", err)
	}
	if _, err := t.rf.Write(t.nl); err != nil {
		t.logger.Error("file transport: newline write failed", "error", err.Error())
		return fmt.Errorf("file transport: write newline: %w", err)
	}

	t.logger.Debug("file transport: sent message", "bytes", len(data))
	return nil
}

// Close implements plugin.Transport. It flushes and closes the underlying file.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.rf != nil {
		if err := t.rf.Close(); err != nil {
			return fmt.Errorf("file transport: close: %w", err)
		}
	}
	t.logger.Info("file transport: closed")
	return nil
}

