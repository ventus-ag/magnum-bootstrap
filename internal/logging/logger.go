package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// MaxLogSize is the maximum log file size before auto-trimming.
	MaxLogSize = 100 * 1024 * 1024 // 100 MB
	// TrimTarget is the size we trim down to (keep the newest lines).
	TrimTarget = 50 * 1024 * 1024 // 50 MB
)

type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	file   *os.File
	stderr io.Writer
	path   string
	size   int64
}

func New(path string, stderr io.Writer) (*Logger, error) {
	if stderr == nil {
		stderr = io.Discard
	}

	if path == "" {
		return &Logger{writer: stderr}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	// Get current file size for tracking.
	var size int64
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}

	l := &Logger{
		writer: io.MultiWriter(file, stderr),
		file:   file,
		stderr: stderr,
		path:   path,
		size:   size,
	}

	// Trim on startup if already oversized.
	if size > MaxLogSize {
		l.trimLocked()
	}

	return l, nil
}

func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *Logger) Infof(format string, args ...any) {
	l.logf("INFO", format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.logf("WARN", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.logf("ERROR", format, args...)
}

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil || l.writer == nil {
		return
	}

	line := fmt.Sprintf("%s [%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, fmt.Sprintf(format, args...))

	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Fprint(l.writer, line)
	l.size += int64(len(line))

	// Auto-trim when log exceeds max size.
	if l.file != nil && l.size > MaxLogSize {
		l.trimLocked()
	}
}

// trimLocked truncates the log file, keeping only the last TrimTarget bytes.
// Must be called with l.mu held.
func (l *Logger) trimLocked() {
	if l.path == "" || l.file == nil {
		return
	}

	// Read the tail of the file.
	data, err := os.ReadFile(l.path)
	if err != nil {
		return
	}

	if int64(len(data)) <= TrimTarget {
		return
	}

	// Keep only the last TrimTarget bytes, starting at a newline boundary.
	keep := data[len(data)-int(TrimTarget):]
	if idx := indexOf(keep, '\n'); idx >= 0 {
		keep = keep[idx+1:]
	}

	// Prepend a marker line.
	marker := fmt.Sprintf("--- log trimmed at %s (was %d bytes) ---\n",
		time.Now().UTC().Format(time.RFC3339), len(data))
	trimmed := append([]byte(marker), keep...)

	// Close current file, rewrite, reopen.
	l.file.Close()
	if err := os.WriteFile(l.path, trimmed, 0o600); err != nil {
		// Best effort — reopen in append mode.
		l.file, _ = os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		return
	}

	l.file, err = os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.file = nil
		l.writer = nil
		return
	}
	l.size = int64(len(trimmed))

	// Rebuild multiwriter with new file handle.
	if l.stderr != nil {
		l.writer = io.MultiWriter(l.file, l.stderr)
	} else {
		l.writer = l.file
	}
}

func indexOf(data []byte, b byte) int {
	for i, c := range data {
		if c == b {
			return i
		}
	}
	return -1
}
