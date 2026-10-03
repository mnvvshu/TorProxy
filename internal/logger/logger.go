// Package logger provides structured, rotating log output for the application.
//
// A single root Logger owns the log file, rotation state and an in-memory ring
// buffer of recent entries (used by the dashboard's live log viewer). Scoped
// child loggers created with WithInstance/WithComponent delegate all writes to
// the root, so rotation is always coherent across components.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLevel converts a textual level ("debug", "info", ...) to a Level.
// Unknown values map to LevelInfo.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Entry is a single structured log record.
type Entry struct {
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	Component string    `json:"component"`
	Message   string    `json:"message"`
}

// Format renders an entry as a single text line.
func (e Entry) Format() string {
	return fmt.Sprintf("[%s] [%-5s] [%s] %s",
		e.Time.Format("2006-01-02 15:04:05.000"), e.Level, e.Component, e.Message)
}

const defaultRingSize = 2000

// core is the shared state behind a root logger and all of its children.
type core struct {
	mu          sync.Mutex
	file        *os.File
	filePath    string
	console     io.Writer
	level       Level
	maxSizeMB   int
	maxFiles    int
	currentSize int64

	ring    []Entry
	ringPos int
	ringLen int
	seq     uint64

	subs map[chan Entry]struct{}
}

// Logger is a component-scoped handle onto a shared logging core.
type Logger struct {
	c         *core
	component string
}

// New creates a new root Logger.
// If filePath is empty, logs only go to the console (when available).
func New(filePath string, component string, maxSizeMB, maxFiles int) (*Logger, error) {
	c := &core{
		level:     LevelInfo,
		maxSizeMB: maxSizeMB,
		maxFiles:  maxFiles,
		filePath:  filePath,
		console:   os.Stdout,
		ring:      make([]Entry, defaultRingSize),
		subs:      make(map[chan Entry]struct{}),
	}

	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return nil, fmt.Errorf("logger: failed to create log directory: %w", err)
		}
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("logger: failed to open log file: %w", err)
		}
		c.file = f
		if info, err := f.Stat(); err == nil {
			c.currentSize = info.Size()
		}
	}

	return &Logger{c: c, component: component}, nil
}

// NewNop returns a logger that only keeps entries in memory (useful for tests).
func NewNop() *Logger {
	l, _ := New("", "test", 1, 1)
	l.c.console = nil
	return l
}

// SetConsole replaces the console writer (nil disables console output).
// In a Windows GUI-subsystem build os.Stdout is invalid, so callers may
// disable it or point it at an attached console.
func (l *Logger) SetConsole(w io.Writer) {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	l.c.console = w
}

// SetLevel sets the minimum log level.
func (l *Logger) SetLevel(level Level) {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	l.c.level = level
}

// GetLevel returns the minimum log level.
func (l *Logger) GetLevel() Level {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	return l.c.level
}

// FilePath returns the active log file path ("" when file logging is disabled).
func (l *Logger) FilePath() string { return l.c.filePath }

// Debug logs at DEBUG level.
func (l *Logger) Debug(format string, args ...interface{}) { l.log(LevelDebug, format, args...) }

// Info logs at INFO level.
func (l *Logger) Info(format string, args ...interface{}) { l.log(LevelInfo, format, args...) }

// Warn logs at WARN level.
func (l *Logger) Warn(format string, args ...interface{}) { l.log(LevelWarn, format, args...) }

// Error logs at ERROR level.
func (l *Logger) Error(format string, args ...interface{}) { l.log(LevelError, format, args...) }

// WithInstance returns a sub-logger scoped to a specific instance ID.
func (l *Logger) WithInstance(instanceID int) *Logger {
	return &Logger{c: l.c, component: fmt.Sprintf("%s:instance-%d", l.component, instanceID)}
}

// WithComponent returns a sub-logger with a different component name.
func (l *Logger) WithComponent(name string) *Logger {
	return &Logger{c: l.c, component: name}
}

// log writes a formatted log entry with timestamp, level, and component.
func (l *Logger) log(level Level, format string, args ...interface{}) {
	c := l.c
	c.mu.Lock()
	defer c.mu.Unlock()

	if level < c.level {
		return
	}

	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	msg = redactSensitive(msg)

	c.seq++
	e := Entry{Seq: c.seq, Time: time.Now(), Level: level.String(), Component: l.component, Message: msg}
	line := e.Format() + "\n"

	// Each sink is written independently: a failing console (e.g. in a GUI
	// build without a console) must never prevent file logging.
	if c.console != nil {
		_, _ = io.WriteString(c.console, line)
	}
	if c.file != nil {
		if n, err := c.file.WriteString(line); err == nil {
			c.currentSize += int64(n)
		}
		if c.maxSizeMB > 0 && c.currentSize >= int64(c.maxSizeMB)*1024*1024 {
			c.rotate()
		}
	}

	// Ring buffer
	c.ring[c.ringPos] = e
	c.ringPos = (c.ringPos + 1) % len(c.ring)
	if c.ringLen < len(c.ring) {
		c.ringLen++
	}

	// Live subscribers (non-blocking)
	for ch := range c.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Recent returns up to n of the most recent entries with Seq > afterSeq, oldest first.
func (l *Logger) Recent(n int, afterSeq uint64) []Entry {
	c := l.c
	c.mu.Lock()
	defer c.mu.Unlock()

	if n <= 0 || n > c.ringLen {
		n = c.ringLen
	}
	out := make([]Entry, 0, n)
	start := (c.ringPos - c.ringLen + len(c.ring)) % len(c.ring)
	for i := 0; i < c.ringLen; i++ {
		e := c.ring[(start+i)%len(c.ring)]
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Subscribe returns a channel that receives every new log entry.
func (l *Logger) Subscribe() chan Entry {
	ch := make(chan Entry, 256)
	l.c.mu.Lock()
	l.c.subs[ch] = struct{}{}
	l.c.mu.Unlock()
	return ch
}

// Unsubscribe stops delivery to a channel returned by Subscribe and closes it.
func (l *Logger) Unsubscribe(ch chan Entry) {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	if _, ok := l.c.subs[ch]; ok {
		delete(l.c.subs, ch)
		close(ch)
	}
}

// rotate performs log file rotation. Caller must hold c.mu.
func (c *core) rotate() {
	if c.file == nil || c.filePath == "" {
		return
	}
	_ = c.file.Close()

	ext := filepath.Ext(c.filePath)
	base := strings.TrimSuffix(c.filePath, ext)
	rotatedPath := fmt.Sprintf("%s.%s%s", base, time.Now().Format("20060102-150405.000"), ext)
	_ = os.Rename(c.filePath, rotatedPath)

	c.cleanOldLogs()

	f, err := os.OpenFile(c.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.file = nil
		return
	}
	c.file = f
	c.currentSize = 0
}

// cleanOldLogs removes excess rotated log files, keeping maxFiles-1 rotated files.
func (c *core) cleanOldLogs() {
	dir := filepath.Dir(c.filePath)
	base := filepath.Base(c.filePath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if name != base && strings.HasPrefix(name, prefix+".") && strings.HasSuffix(name, ext) {
			rotated = append(rotated, filepath.Join(dir, name))
		}
	}
	sort.Strings(rotated) // timestamp-based names sort chronologically

	for len(rotated) >= c.maxFiles && len(rotated) > 0 {
		_ = os.Remove(rotated[0])
		rotated = rotated[1:]
	}
}

// Close flushes and closes the log file and all subscriber channels.
func (l *Logger) Close() error {
	c := l.c
	c.mu.Lock()
	defer c.mu.Unlock()

	for ch := range c.subs {
		delete(c.subs, ch)
		close(ch)
	}
	if c.file != nil {
		err := c.file.Close()
		c.file = nil
		return err
	}
	return nil
}

var (
	// Tor control-port cookies / hashed passwords are 32+ hex characters.
	reHexSecret = regexp.MustCompile(`\b[0-9A-Fa-f]{32,}\b`)
	// Anything after AUTHENTICATE on a control-port command line.
	reAuthCmd = regexp.MustCompile(`(?i)(AUTHENTICATE)\s+\S+`)
	// user:pass@ credentials embedded in URLs.
	reURLCreds = regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`)
)

// redactSensitive removes credentials, auth cookies, and other secrets from log entries.
// It is intentionally conservative — better to redact too much than too little.
func redactSensitive(msg string) string {
	msg = reAuthCmd.ReplaceAllString(msg, "$1 [REDACTED]")
	msg = reURLCreds.ReplaceAllString(msg, "${1}[REDACTED]@")
	msg = reHexSecret.ReplaceAllString(msg, "[REDACTED]")
	return msg
}
