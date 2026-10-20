package platform

import (
	"log/slog"
	"os"
	"strings"
)

// ParseLevel maps a configuration string to a slog level.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger builds a structured logger from level and format strings.
func NewLogger(level, format string) *slog.Logger {
	logger, _ := NewLoggerLevel(level, format)
	return logger
}

// NewLoggerLevel builds a logger with a mutable level.
func NewLoggerLevel(level, format string) (*slog.Logger, *slog.LevelVar) {
	lvl := new(slog.LevelVar)
	lvl.Set(ParseLevel(level))
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler), lvl
}
