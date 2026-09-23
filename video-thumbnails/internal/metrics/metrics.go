// Package metrics defines the Prometheus metrics of the video-thumbnails
// service.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/config"
)

// Metrics of the service. The hyphen of the service name is not allowed in a
// metric name, so the prefix is video_thumbnails.
type Metrics struct {
	// Jobs counts the finished jobs, by result: ok, skipped (file gone or
	// changed), retry (failed, will run again), failed (given up).
	Jobs *prometheus.CounterVec
	// JobDuration is how long a successful generation took, download included.
	JobDuration prometheus.Histogram
	// QueueDepth is how many jobs wait, by subject.
	QueueDepth *prometheus.GaugeVec
	// Events counts the events taken off main-queue, by platform type.
	Events *prometheus.CounterVec
	// ConsumerLag is how many messages of main-queue are still undelivered.
	ConsumerLag prometheus.Gauge
	// HTTPRequests counts the answers of the HTTP side, by route and code.
	HTTPRequests *prometheus.CounterVec
	// CacheHits counts the previews served from a cache, by level: memory
	// (state index), disk, s3.
	CacheHits *prometheus.CounterVec
}

// New registers the metrics on the registry.
func New(registry prometheus.Registerer) *Metrics {
	prefix := obs.MetricPrefix(config.Name) + "_"

	m := &Metrics{
		Jobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "jobs_total",
			Help: "Finished thumbnail jobs, by result.",
		}, []string{"result"}),
		JobDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    prefix + "job_duration_seconds",
			Help:    "Time a successful generation took, download included.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "queue_depth",
			Help: "Jobs waiting in the queue, by subject.",
		}, []string{"subject"}),
		Events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "events_total",
			Help: "Events taken off main-queue, by platform type.",
		}, []string{"type"}),
		ConsumerLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prefix + "consumer_lag",
			Help: "Messages of main-queue not delivered to the service yet.",
		}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "http_requests_total",
			Help: "Answers of the HTTP side, by route and status code.",
		}, []string{"route", "code"}),
		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "cache_hits_total",
			Help: "Previews served from a cache, by level.",
		}, []string{"level"}),
	}

	registry.MustRegister(m.Jobs, m.JobDuration, m.QueueDepth, m.Events, m.ConsumerLag, m.HTTPRequests, m.CacheHits)
	return m
}
