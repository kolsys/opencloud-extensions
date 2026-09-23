package config

import (
	"encoding/json"
	"log/slog"
)

// maskedSecret replaces a secret wherever it would be printed.
const maskedSecret = "***"

// Secret is a configuration value that must never reach a log or an error
// message. Printing it or logging it yields a mask; Reveal returns the value.
type Secret string

// String implements fmt.Stringer with the value masked.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return maskedSecret
}

// GoString implements fmt.GoStringer with the value masked.
func (s Secret) GoString() string {
	return s.String()
}

// LogValue implements slog.LogValuer with the value masked.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// MarshalJSON implements json.Marshaler with the value masked, so that
// serialising a whole configuration cannot leak it.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// Reveal returns the value itself. Call it only where the secret is used.
func (s Secret) Reveal() string {
	return string(s)
}

// Empty reports whether the secret is unset.
func (s Secret) Empty() bool {
	return s == ""
}
