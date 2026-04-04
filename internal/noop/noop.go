// Package noop provides a discard io.Writer used as a slog fallback when no
// real output target is configured. Import this instead of defining a local
// noopWriter in each package.
package noop

// Writer is an io.Writer that discards all output.
type Writer struct{}

func (Writer) Write(p []byte) (int, error) { return len(p), nil }
