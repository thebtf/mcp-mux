// Package rotlog provides a size-based rotating io.Writer for log files.
// Safe for concurrent use. Rotation happens when a Write would push the
// active file above MaxSize — the active file is closed, shifted to .1,
// older backups are shifted up, and a fresh active file is opened.
//
// If a single Write payload exceeds MaxSize, the write proceeds anyway
// (into the new file) to preserve per-line atomicity. The file is then
// rotated on the NEXT Write that would overflow.
package rotlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrClosed is returned by Write after Close has been called.
var ErrClosed = errors.New("rotlog: writer is closed")

// Writer is a size-based rotating io.Writer. Safe for concurrent use.
type Writer struct {
	cfg    Config
	mu     sync.Mutex
	file   *os.File
	size   int64
	closed bool
}

// Config controls rotation behavior.
type Config struct {
	// Path to the active log file. Rotated copies are written as Path.1, Path.2, ...
	Path string
	// MaxSize in bytes. When a Write would push the active file above MaxSize,
	// rotation happens BEFORE the write lands in a new file. Must be > 0.
	MaxSize int64
	// MaxFiles is the total number of files kept, counting the active one.
	// E.g. MaxFiles=3 keeps Path (active) + Path.1 + Path.2. Path.2 is
	// discarded on each rotation to keep the total at MaxFiles. Must be >= 1.
	MaxFiles int
	// FileMode for newly created files. Defaults to 0644 if zero.
	FileMode os.FileMode
}

// New opens (or creates) the active log file per cfg and returns a Writer.
// If the file already exists and is >= MaxSize, rotation is deferred until
// the first Write (so that an already-too-big file is rotated on first Write).
func New(cfg Config) (*Writer, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("rotlog: Path must not be empty")
	}
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("rotlog: MaxSize must be > 0, got %d", cfg.MaxSize)
	}
	if cfg.MaxFiles < 1 {
		return nil, fmt.Errorf("rotlog: MaxFiles must be >= 1, got %d", cfg.MaxFiles)
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = 0644
	}

	f, size, err := openActive(cfg)
	if err != nil {
		return nil, err
	}

	return &Writer{
		cfg:  cfg,
		file: f,
		size: size,
	}, nil
}

// openActive opens (or creates) the active log file and returns the file and its current size.
func openActive(cfg Config) (*os.File, int64, error) {
	f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, cfg.FileMode)
	if err != nil {
		return nil, 0, fmt.Errorf("rotlog: open %s: %w", cfg.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("rotlog: stat %s: %w", cfg.Path, err)
	}
	return f, info.Size(), nil
}

// rotate closes the current file, shifts backups, opens a new active file.
// Caller must hold w.mu.
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		_ = err // non-fatal: proceed with rotation even if close had issues
	}
	w.file = nil

	// Delete the oldest backup file to make room.
	// MaxFiles=3 means: active + .1 + .2. The oldest backup is .2 (index MaxFiles-1).
	// After shifting .1→.2, active→.1, we open a fresh active — total stays at MaxFiles.
	oldest := w.cfg.MaxFiles - 1
	if oldest >= 1 {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.cfg.Path, oldest))
	}

	// Shift backups from highest down: .(N-2) → .(N-1), ..., .1 → .2
	for i := w.cfg.MaxFiles - 2; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.cfg.Path, i)
		dst := fmt.Sprintf("%s.%d", w.cfg.Path, i+1)
		_ = os.Rename(src, dst) // ignore error (file may not exist)
	}

	// Rename active → .1 (only when MaxFiles >= 2; with MaxFiles=1, discard in place)
	if w.cfg.MaxFiles >= 2 {
		_ = os.Rename(w.cfg.Path, w.cfg.Path+".1")
	}

	// Open fresh active file
	f, err := os.OpenFile(w.cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, w.cfg.FileMode)
	if err != nil {
		return fmt.Errorf("rotlog: open new active file %s: %w", w.cfg.Path, err)
	}
	w.file = f
	w.size = 0
	return nil
}

// Write implements io.Writer. Thread-safe. Rotates atomically on size overflow.
//
// If a single Write payload exceeds MaxSize, the write proceeds anyway
// (to preserve per-line atomicity). Rotation fires on the next Write that overflows.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, ErrClosed
	}

	// Rotate if this write would push us over the limit (and file is non-empty).
	if w.size > 0 && w.size+int64(len(p)) > w.cfg.MaxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, fmt.Errorf("rotlog: short write: %w", io.ErrShortWrite)
	}
	w.size += int64(n)
	return n, nil
}

// Close flushes and closes the active file. After Close, Write returns ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
