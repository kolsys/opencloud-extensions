// Package metrics defines the Prometheus metrics of the file-activity service.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
)

// Metrics of the service. The hyphen of the service name is not allowed in a
// metric name, so the prefix is file_activity.
type Metrics struct {
	// EventsIn counts the events taken off main-queue, by platform type.
	EventsIn *prometheus.CounterVec
	// EventsOut counts the events published to the feed.
	EventsOut prometheus.Counter
	// Dropped counts the events of main-queue that did not reach the feed,
	// by reason: ignored (type not consumed), failed (upload failed), gone
	// (resource vanished before the lookup), unknown (type not decoded).
	Dropped *prometheus.CounterVec
	// StreamFirstSeq and StreamLastSeq mirror the bounds of the feed.
	StreamFirstSeq prometheus.Gauge
	StreamLastSeq  prometheus.Gauge
	// HTTPRequests counts the answers of the API, by status code.
	HTTPRequests *prometheus.CounterVec
	// ConsumerLag is how many messages of main-queue are still undelivered.
	ConsumerLag prometheus.Gauge
	// Webhook counts the deliveries, by status code of the endpoint.
	Webhook *prometheus.CounterVec
}

// New registers the metrics on the registry.
func New(registry prometheus.Registerer) *Metrics {
	prefix := obs.MetricPrefix(config.Name) + "_"

	m := &Metrics{
		EventsIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "events_in_total",
			Help: "Events taken off main-queue, by platform type.",
		}, []string{"type"}),
		EventsOut: prometheus.NewCounter(prometheus.CounterOpts{
			Name: prefix + "events_out_total",
			Help: "Events published to the feed.",
		}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "dropped_total",
			Help: "Events of main-queue that did not reach the feed, by reason.",
		}, []string{"reason"}),
		StreamFirstSeq: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prefix + "stream_first_seq",
			Help: "First sequence number the feed still holds.",
		}),
		StreamLastSeq: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prefix + "stream_last_seq",
			Help: "Last sequence number of the feed.",
		}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "http_requests_total",
			Help: "Answers of the API, by status code.",
		}, []string{"code"}),
		ConsumerLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prefix + "consumer_lag",
			Help: "Messages of main-queue not delivered to the service yet.",
		}),
		Webhook: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "webhook_total",
			Help: "Deliveries to the webhook, by status code of the endpoint.",
		}, []string{"code"}),
	}

	registry.MustRegister(
		m.EventsIn, m.EventsOut, m.Dropped, m.StreamFirstSeq, m.StreamLastSeq,
		m.HTTPRequests, m.ConsumerLag, m.Webhook,
	)
	return m
}
