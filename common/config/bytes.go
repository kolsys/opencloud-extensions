package config

import (
	"errors"
	"strconv"
	"strings"
)

// Bytes is a byte size, written plain or with a binary (Ki, Mi, Gi, Ti) or
// decimal (K, M, G, T) suffix.
type Bytes int64

var byteSuffixes = []struct {
	name string
	size int64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *Bytes) UnmarshalText(text []byte) error {
	invalid := errors.New("must be a byte size, like 2Gi")

	number := strings.TrimSpace(string(text))
	unit := int64(1)
	for _, suffix := range byteSuffixes {
		if rest, ok := strings.CutSuffix(number, suffix.name); ok {
			number, unit = strings.TrimSpace(rest), suffix.size
			break
		}
	}

	parsed, err := strconv.ParseInt(number, 10, 64)
	if err != nil || parsed < 0 {
		return invalid
	}
	*b = Bytes(parsed * unit)
	return nil
}

// String renders the size with the largest binary suffix that divides it.
func (b Bytes) String() string {
	for i := len(byteSuffixes) - 1; i >= 0; i-- {
		suffix := byteSuffixes[i]
		if !strings.HasSuffix(suffix.name, "i") {
			continue
		}
		if b >= Bytes(suffix.size) && int64(b)%suffix.size == 0 {
			return strconv.FormatInt(int64(b)/suffix.size, 10) + suffix.name
		}
	}
	return strconv.FormatInt(int64(b), 10)
}
