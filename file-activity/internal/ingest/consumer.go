package ingest

import (
	"context"
	stderrors "errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

// Redelivery of a message whose lookup or publish failed.
const (
	maxDeliver   = 10
	backoffBase  = 2 * time.Second
	backoffLimit = 5 * time.Minute
	ackWait      = time.Minute
	lagInterval  = 15 * time.Second
)

// Consumer reads main-queue in its own group, maps every event, appends the
// result to the feed and updates the tree. The message is acknowledged only
// after both are done, so a crash in between replays the event on restart;
// the duplicate window of the feed swallows the second copy and the tree
// takes the same change twice.
type Consumer struct {
	conn    *jsx.Conn
	store   *feed.Store
	mapper  *Mapper
	metrics *metrics.Metrics
	log     *slog.Logger
	group   string
	backoff []time.Duration
}

// New returns a consumer for the group.
func New(conn *jsx.Conn, store *feed.Store, mapper *Mapper, m *metrics.Metrics, group string, log *slog.Logger) *Consumer {
	return &Consumer{
		conn:    conn,
		store:   store,
		mapper:  mapper,
		metrics: m,
		log:     log,
		group:   group,
		backoff: jsx.Backoff(maxDeliver, backoffBase, backoffLimit),
	}
}

// Run consumes until ctx is cancelled. Messages are handled one at a time,
// in order: the feed is a sequence.
func (c *Consumer) Run(ctx context.Context) error {
	consumer, err := c.conn.EnsureConsumer(ctx, ocevents.MainQueue, jetstream.ConsumerConfig{
		Durable:       c.group,
		Description:   "file-activity: file events into the feed",
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		MaxAckPending: 1,
	})
	if err != nil {
		return err
	}

	consume, err := consumer.Consume(func(message jetstream.Msg) {
		c.handle(ctx, message)
	})
	if err != nil {
		return err
	}
	defer consume.Stop()

	c.log.Info("consuming main-queue", slog.String("group", c.group))

	ticker := time.NewTicker(lagInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.observe(ctx, consumer)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, message jetstream.Msg) {
	event, err := ocevents.Decode(message.Data())
	if err != nil {
		// A message that does not decode never will: terminate it instead of
		// letting it come back until MaxDeliver.
		c.log.Error("malformed message on main-queue", slog.Any("error", err))
		c.metrics.Dropped.WithLabelValues(ReasonMalformed).Inc()
		c.terminate(message)
		return
	}
	c.metrics.EventsIn.WithLabelValues(event.Type).Inc()

	log := c.log.With(slog.String("event", event.ID), slog.String("type", event.Type))

	mapped, err := c.mapper.Map(ctx, event)
	var skip *SkipError
	switch {
	case stderrors.As(err, &skip):
		c.metrics.Dropped.WithLabelValues(skip.Reason).Inc()
		if skip.Reason != ReasonIgnored && skip.Reason != ReasonUnknown {
			log.Info("event dropped", slog.String("reason", skip.Reason))
		}
		c.ack(message)
		return
	case err != nil:
		log.Warn("lookup failed, will retry", slog.Any("error", err))
		c.retry(message)
		return
	}

	// The entries go out before the tree changes: a replay after a crash
	// publishes the same ids again, which the feed drops, and applies the
	// same change again, which the tree takes.
	for _, entry := range mapped.Events {
		seq, err := c.store.Publish(ctx, entry)
		if err != nil {
			log.Error("publish to the feed failed, will retry", slog.Any("error", err))
			c.retry(message)
			return
		}
		c.metrics.EventsOut.Inc()
		log.Debug("event published", slog.Uint64("seq", seq), slog.String("feed_type", string(entry.Type)), slog.String("entry", entry.ID))
	}

	if mapped.Apply != nil {
		if err := mapped.Apply(ctx); err != nil {
			log.Error("tree not updated, will retry", slog.Any("error", err))
			c.retry(message)
			return
		}
	}
	c.ack(message)
}

func (c *Consumer) ack(message jetstream.Msg) {
	if err := message.Ack(); err != nil {
		c.log.Warn("ack failed", slog.Any("error", err))
	}
}

func (c *Consumer) terminate(message jetstream.Msg) {
	if err := message.Term(); err != nil {
		c.log.Warn("term failed", slog.Any("error", err))
	}
}

// retry asks for a redelivery after a wait that grows with every attempt.
func (c *Consumer) retry(message jetstream.Msg) {
	var delivered uint64
	if metadata, err := message.Metadata(); err == nil {
		delivered = metadata.NumDelivered
	}
	if err := message.NakWithDelay(c.backoff[jsx.Attempt(delivered, len(c.backoff))]); err != nil {
		c.log.Warn("nak failed", slog.Any("error", err))
	}
}

// observe refreshes the gauges of the consumer and the feed.
func (c *Consumer) observe(ctx context.Context, consumer jetstream.Consumer) {
	if lag, err := jsx.Lag(ctx, consumer); err == nil {
		c.metrics.ConsumerLag.Set(float64(lag))
	}
	if head, err := c.store.Head(ctx); err == nil {
		c.metrics.StreamFirstSeq.Set(float64(head.FirstAvailable))
		c.metrics.StreamLastSeq.Set(float64(head.Last))
	}
}
