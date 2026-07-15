// Package logger provides the service's structured logger: a thin wrapper
// over log/slog so every package logs the same way and tests can silence
// output. The TS service used a hand-rolled leveled logger (src/logger.ts);
// the Go port standardizes on slog with the same level vocabulary
// (debug|info|warn|error).
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level is the configured log verbosity. Mirrors the TS LogLevel union.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// New returns a slog.Logger writing JSON to stderr at the given level.
// Unknown levels fall back to info (matching the TS oneOf default).
func New(level Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel(level)}))
}

// NewTo is New with an explicit writer (used by tests that capture output).
func NewTo(w io.Writer, level Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slogLevel(level)}))
}

// Silent returns a logger that discards everything. The default for
// library-ish packages (usage emitter, auth) so a nil logger is never
// dereferenced and tests stay quiet.
func Silent() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func slogLevel(level Level) slog.Level {
	switch Level(strings.ToLower(string(level))) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
