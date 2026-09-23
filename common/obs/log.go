// Package obs provides the logging, metrics and health endpoints shared by the
// services.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Log levels the platform knows and slog does not.
const (
	LevelTrace = slog.LevelDebug - 4
	LevelFatal = slog.LevelError + 4
	LevelPanic = slog.LevelError + 8
)

var levelNames = map[string]slog.Level{
	"trace": LevelTrace,
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
	"fatal": LevelFatal,
	"panic": LevelPanic,
}

// ParseLevel maps a log level name of the platform to a slog level.
func ParseLevel(name string) (slog.Level, error) {
	level, ok := levelNames[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return slog.LevelInfo, fmt.Errorf("obs: unknown log level %q", name)
	}
	return level, nil
}

// NewLogger returns a JSON logger tagged with the service name and version.
func NewLogger(w io.Writer, service, version string, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceLevel,
	})
	return slog.New(handler).With(
		slog.String("service", service),
		slog.String("version", version),
	)
}

// replaceLevel prints the platform names for the levels slog does not know.
func replaceLevel(_ []string, attr slog.Attr) slog.Attr {
	if attr.Key != slog.LevelKey {
		return attr
	}
	level, ok := attr.Value.Any().(slog.Level)
	if !ok {
		return attr
	}
	for name, known := range levelNames {
		if known == level {
			attr.Value = slog.StringValue(strings.ToUpper(name))
			break
		}
	}
	return attr
}
