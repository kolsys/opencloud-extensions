package jsx

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Defaults of a durable consumer.
const (
	// DefaultAckWait is how long a message may stay unacknowledged before it
	// is redelivered. It bounds how long a killed worker leaves a job stuck.
	DefaultAckWait = 60 * time.Second

	// DefaultMaxDeliver is how often a message is redelivered before it is
	// given up on.
	DefaultMaxDeliver = 5
)

// EnsureConsumer creates a durable pull consumer on a stream or converges an
// existing one to cfg.
//
// The acknowledgement is always explicit: a message is acknowledged once the
// work it describes is done, never on delivery. Delivery is at-least-once, so
// every handler has to be idempotent.
func (c *Conn) EnsureConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	if cfg.Durable == "" {
		return nil, ErrNoDurableName
	}

	cfg.AckPolicy = jetstream.AckExplicitPolicy
	if cfg.AckWait == 0 {
		cfg.AckWait = DefaultAckWait
	}
	if cfg.MaxDeliver == 0 {
		cfg.MaxDeliver = DefaultMaxDeliver
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Durable
	}

	consumer, err := c.js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, fmt.Errorf("jsx: consumer %s on %s: %w", cfg.Durable, stream, err)
	}
	return consumer, nil
}

// Backoff returns a redelivery schedule that doubles the wait every step,
// capped at ceiling. JetStream applies one entry per delivery attempt.
func Backoff(steps int, base, ceiling time.Duration) []time.Duration {
	if steps < 1 {
		return nil
	}

	schedule := make([]time.Duration, 0, steps)
	wait := base
	for range steps {
		if wait > ceiling {
			wait = ceiling
		}
		schedule = append(schedule, wait)
		wait *= 2
	}
	return schedule
}

// Attempt maps the delivery count of a message to an index into a backoff
// schedule of the given length: the first delivery is attempt 0 and the last
// entry of the schedule is reused once it is exhausted.
func Attempt(delivered uint64, steps int) int {
	if steps < 1 || delivered <= 1 {
		return 0
	}
	if delivered > uint64(steps) {
		return steps - 1
	}
	return int(delivered) - 1 //nolint:gosec // bounded by steps above
}

// Lag reports how many messages a consumer has not delivered yet. It backs
// the consumer_lag metric of both extensions.
func Lag(ctx context.Context, consumer jetstream.Consumer) (uint64, error) {
	info, err := consumer.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("jsx: consumer info: %w", err)
	}
	return info.NumPending, nil
}
