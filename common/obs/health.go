package obs

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// DefaultProbeTimeout bounds one readiness check.
const DefaultProbeTimeout = 3 * time.Second

// Probe reports whether a dependency is usable right now.
type Probe func(ctx context.Context) error

// Health collects the readiness probes of a service. The zero value is not
// usable, call NewHealth.
type Health struct {
	mu      sync.RWMutex
	probes  map[string]Probe
	timeout time.Duration
}

// NewHealth returns an empty set of probes.
func NewHealth() *Health {
	return &Health{probes: map[string]Probe{}, timeout: DefaultProbeTimeout}
}

// Register adds a probe under a name, replacing a probe of the same name.
func (h *Health) Register(name string, probe Probe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes[name] = probe
}

// Check runs every probe and returns the failure of each one that failed.
func (h *Health) Check(ctx context.Context) map[string]error {
	h.mu.RLock()
	probes := make(map[string]Probe, len(h.probes))
	maps.Copy(probes, h.probes)
	timeout := h.timeout
	h.mu.RUnlock()

	results := make(map[string]error, len(probes))
	for name, probe := range probes {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		results[name] = probe(probeCtx)
		cancel()
	}
	return results
}

// LiveHandler answers /healthz: the process runs and serves HTTP.
func (h *Health) LiveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]string{})
	}
}

// ReadyHandler answers /readyz: every dependency of the service is reachable.
func (h *Health) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		checks, status := map[string]string{}, http.StatusOK
		for name, err := range h.Check(r.Context()) {
			if err != nil {
				checks[name], status = err.Error(), http.StatusServiceUnavailable
				continue
			}
			checks[name] = "ok"
		}
		writeHealth(w, status, checks)
	}
}

func writeHealth(w http.ResponseWriter, status int, checks map[string]string) {
	body := struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks,omitempty"`
	}{Status: "ok", Checks: checks}
	if status != http.StatusOK {
		body.Status = "unavailable"
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		encoded, status = []byte(`{"status":"unavailable"}`), http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// Names returns the registered probe names, sorted.
func (h *Health) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return slices.Sorted(maps.Keys(h.probes))
}
