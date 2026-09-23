package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type nested struct {
	Workers int `env:"TEST_WORKERS"`
}

type testConfig struct {
	nested

	Endpoint   string        `env:"TEST_ENDPOINT"`
	Level      string        `env:"TEST_GLOBAL_LEVEL;TEST_LEVEL"`
	Enabled    bool          `env:"TEST_ENABLED"`
	Timeout    time.Duration `env:"TEST_TIMEOUT"`
	CacheSize  Bytes         `env:"TEST_CACHE_SIZE"`
	Extensions []string      `env:"TEST_EXTENSIONS"`
	Password   Secret        `env:"TEST_PASSWORD"`
	Untagged   string
}

func defaultTestConfig() testConfig {
	return testConfig{
		nested:     nested{Workers: 4},
		Endpoint:   "opencloud:9233",
		Level:      "info",
		Extensions: []string{"mp4"},
		Untagged:   "kept",
	}
}

func TestDecodeKeepsDefaults(t *testing.T) {
	cfg := defaultTestConfig()
	if err := Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := defaultTestConfig()
	if cfg.Endpoint != want.Endpoint || cfg.Workers != want.Workers || cfg.Untagged != want.Untagged {
		t.Errorf("defaults changed: %+v", cfg)
	}
}

func TestDecodeOverrides(t *testing.T) {
	t.Setenv("TEST_ENDPOINT", "nats:4222")
	t.Setenv("TEST_WORKERS", "8")
	t.Setenv("TEST_ENABLED", "true")
	t.Setenv("TEST_TIMEOUT", "90s")
	t.Setenv("TEST_CACHE_SIZE", "2Gi")
	t.Setenv("TEST_EXTENSIONS", "mp4, mov ,,webm")
	t.Setenv("TEST_PASSWORD", "hunter2")

	cfg := defaultTestConfig()
	if err := Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if cfg.Endpoint != "nats:4222" {
		t.Errorf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Workers != 8 {
		t.Errorf("Workers = %d, nested struct not walked through", cfg.Workers)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false")
	}
	if cfg.Timeout != 90*time.Second {
		t.Errorf("Timeout = %s", cfg.Timeout)
	}
	if cfg.CacheSize != 2<<30 {
		t.Errorf("CacheSize = %d", cfg.CacheSize)
	}
	if got := fmt.Sprint(cfg.Extensions); got != "[mp4 mov webm]" {
		t.Errorf("Extensions = %s", got)
	}
	if cfg.Password.Reveal() != "hunter2" {
		t.Errorf("Password not read")
	}
}

// The platform resolves a chain by looking up every name in order and keeping
// the last one that is set.
func TestDecodeChainLastSetWins(t *testing.T) {
	t.Setenv("TEST_GLOBAL_LEVEL", "warn")

	cfg := defaultTestConfig()
	if err := Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Level != "warn" {
		t.Fatalf("Level = %q, want the global name to apply", cfg.Level)
	}

	t.Setenv("TEST_LEVEL", "debug")
	cfg = defaultTestConfig()
	if err := Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Level != "debug" {
		t.Errorf("Level = %q, want the service name to override the global one", cfg.Level)
	}
}

func TestDecodeReportsEveryError(t *testing.T) {
	t.Setenv("TEST_ENABLED", "yes please")
	t.Setenv("TEST_TIMEOUT", "90")
	t.Setenv("TEST_CACHE_SIZE", "2 bananas")

	cfg := defaultTestConfig()
	err := Decode(&cfg)
	if err == nil {
		t.Fatal("Decode: no error")
	}
	for _, name := range []string{"TEST_ENABLED", "TEST_TIMEOUT", "TEST_CACHE_SIZE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s: %v", name, err)
		}
	}
}

type requiredConfig struct {
	Secret Secret `env:"TEST_REQUIRED_SECRET,required"`
}

func TestDecodeRequired(t *testing.T) {
	cfg := requiredConfig{}
	err := Decode(&cfg)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("Decode: %v, want ErrMissing", err)
	}

	t.Setenv("TEST_REQUIRED_SECRET", "")
	if err := Decode(&cfg); !errors.Is(err, ErrMissing) {
		t.Errorf("Decode with an empty value: %v, want ErrMissing", err)
	}

	t.Setenv("TEST_REQUIRED_SECRET", "s3cr3t")
	if err := Decode(&cfg); err != nil {
		t.Errorf("Decode: %v", err)
	}
}

type validatedConfig struct {
	Endpoint string `env:"TEST_VALIDATED_ENDPOINT"`
}

var errNotValidated = errors.New("not validated")

func (c *validatedConfig) Validate() error { return errNotValidated }

func TestDecodeCallsValidate(t *testing.T) {
	if err := Decode(&validatedConfig{}); !errors.Is(err, errNotValidated) {
		t.Errorf("Decode: %v, want the error of Validate", err)
	}
}

func TestDecodeInvalidTarget(t *testing.T) {
	cfg := defaultTestConfig()
	for _, target := range []any{cfg, nil, (*testConfig)(nil), &cfg.Endpoint} {
		if err := Decode(target); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("Decode(%T): %v, want ErrInvalidTarget", target, err)
		}
	}
}

func TestSecretIsMasked(t *testing.T) {
	secret := Secret("hunter2")
	if got := fmt.Sprintf("%v %s %q %#v", secret, secret, secret, secret); strings.Contains(got, "hunter2") {
		t.Errorf("secret leaked: %s", got)
	}
	if got := Secret("").String(); got != "" {
		t.Errorf("empty secret = %q", got)
	}
}

func TestBytesString(t *testing.T) {
	for _, tc := range []struct {
		size Bytes
		want string
	}{{2 << 30, "2Gi"}, {512 << 10, "512Ki"}, {1000, "1000"}, {0, "0"}} {
		if got := tc.size.String(); got != tc.want {
			t.Errorf("Bytes(%d).String() = %q, want %q", tc.size, got, tc.want)
		}
	}
}
