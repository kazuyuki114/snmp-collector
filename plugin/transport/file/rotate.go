// rotate.go — size-based file rotation for the File transport plugin.
//
// When MaxBytes have been written to the active file it is renamed with a
// numeric suffix (e.g. metrics.json → metrics.json.1) and a fresh file is
// opened.  Up to MaxBackups old files are kept; older ones are removed.
//
// RotatingFile satisfies io.Writer and io.Closer so it can be used directly
// as the underlying writer for Transport.Send.
package file

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"snmp/snmp-collector/internal/noop"
)

// ─────────────────────────────────────────────────────────────────────────────
// RotateConfig
// ─────────────────────────────────────────────────────────────────────────────

// RotateConfig controls log rotation behaviour.
type RotateConfig struct {
	// FilePath is the active file name (required).
	FilePath string

	// MaxBytes triggers rotation when the active file exceeds this size.
	// Zero disables rotation (the file grows without bound).
	MaxBytes int64

	// MaxBackups is the number of rotated files to keep.
	// Zero means keep all rotated files.
	MaxBackups int
}

// ─────────────────────────────────────────────────────────────────────────────
// RotatingFile
// ─────────────────────────────────────────────────────────────────────────────

// RotatingFile is an io.WriteCloser that performs size-based rotation.
// It is safe for concurrent use.
type RotatingFile struct {
	mu     sync.Mutex
	cfg    RotateConfig
	file   *os.File
	size   int64
	logger *slog.Logger
}

// NewRotatingFile opens (or creates) the file at cfg.FilePath and returns a
// RotatingFile writer.  The caller must call Close when finished.
func NewRotatingFile(cfg RotateConfig, logger *slog.Logger) (*RotatingFile, error) {
	if cfg.FilePath == "" {
		return nil, fmt.Errorf("file transport: rotate: FilePath is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}

	// Ensure parent directory exists.
	dir := filepath.Dir(cfg.FilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("file transport: rotate: mkdir %s: %w", dir, err)
	}

	rf := &RotatingFile{
		cfg:    cfg,
		logger: logger,
	}
	if err := rf.openFile(); err != nil {
		return nil, err
	}
	return rf, nil
}

// Write implements io.Writer.  It rotates the file when MaxBytes is exceeded.
func (rf *RotatingFile) Write(p []byte) (int, error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// Check if rotation is needed before writing.
	if rf.cfg.MaxBytes > 0 && rf.size+int64(len(p)) > rf.cfg.MaxBytes {
		if err := rf.rotate(); err != nil {
			rf.logger.Error("file transport: rotate failed", "error", err.Error())
			// Continue writing to the current file rather than losing data.
		}
	}

	n, err := rf.file.Write(p)
	rf.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (rf *RotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.file != nil {
		return rf.file.Close()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// openFile opens (or creates) the active file and sets the current size.
func (rf *RotatingFile) openFile() error {
	f, err := os.OpenFile(rf.cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("file transport: rotate: open %s: %w", rf.cfg.FilePath, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("file transport: rotate: stat %s: %w", rf.cfg.FilePath, err)
	}
	rf.file = f
	rf.size = info.Size()
	return nil
}

// rotate renames the active file with numbered suffixes and opens a new one.
//
// Rotation scheme:
//
//	metrics.json   → metrics.json.1
//	metrics.json.1 → metrics.json.2
//	...
//	metrics.json.N → (removed if N > MaxBackups)
func (rf *RotatingFile) rotate() error {
	// Close current file.
	if rf.file != nil {
		if err := rf.file.Close(); err != nil {
			rf.logger.Warn("file transport: rotate: close error", "error", err.Error())
		}
		rf.file = nil
	}

	base := rf.cfg.FilePath

	// Shift existing backups up by one.
	if rf.cfg.MaxBackups > 0 {
		// Remove the oldest if it would exceed MaxBackups.
		oldest := fmt.Sprintf("%s.%d", base, rf.cfg.MaxBackups)
		_ = os.Remove(oldest) // ignore error if it doesn't exist
	}

	// Shift .N-1 → .N, .N-2 → .N-1, etc.
	limit := rf.cfg.MaxBackups
	if limit == 0 {
		// When unlimited, find the highest existing backup.
		limit = rf.findMaxBackup()
	}
	for i := limit; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", base, i)
		dst := fmt.Sprintf("%s.%d", base, i+1)
		_ = os.Rename(src, dst) // ignore error if src doesn't exist
	}

	// Rename active file → .1
	if err := os.Rename(base, base+".1"); err != nil && !os.IsNotExist(err) {
		rf.logger.Warn("file transport: rotate: rename error", "error", err.Error())
	}

	// Prune excess backups when MaxBackups > 0.
	if rf.cfg.MaxBackups > 0 {
		rf.prune()
	}

	rf.logger.Info("file transport: rotated", "file", base)

	// Open a fresh file.
	rf.size = 0
	return rf.openFile()
}

// findMaxBackup returns the highest numbered backup that currently exists.
func (rf *RotatingFile) findMaxBackup() int {
	base := rf.cfg.FilePath
	max := 0
	for i := 1; ; i++ {
		name := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(name); os.IsNotExist(err) {
			break
		}
		max = i
	}
	return max
}

// prune removes backup files beyond MaxBackups.
func (rf *RotatingFile) prune() {
	base := rf.cfg.FilePath
	for i := rf.cfg.MaxBackups + 1; ; i++ {
		name := fmt.Sprintf("%s.%d", base, i)
		if err := os.Remove(name); err != nil {
			break // no more files
		}
		rf.logger.Debug("file transport: pruned old backup", "file", name)
	}
}
