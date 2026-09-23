package obs

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricPrefix turns a service name into a metric name prefix: Prometheus
// does not allow the hyphen the rest of the naming uses.
func MetricPrefix(service string) string {
	return strings.ReplaceAll(service, "-", "_")
}

// NewRegistry returns a registry holding the Go runtime, process and build
// info collectors of the service.
func NewRegistry(service, version string) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: MetricPrefix(service) + "_build_info",
		Help: "Build information of the service, always 1.",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)
	registry.MustRegister(buildInfo)

	return registry
}

// MetricsHandler serves a registry in the Prometheus text format.
func MetricsHandler(registry *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry})
}

// Mount registers the observability endpoints on the mux of the service.
func Mount(mux *http.ServeMux, registry *prometheus.Registry, health *Health) {
	mux.Handle("GET /metrics", MetricsHandler(registry))
	mux.HandleFunc("GET /healthz", health.LiveHandler())
	mux.HandleFunc("GET /readyz", health.ReadyHandler())
}
