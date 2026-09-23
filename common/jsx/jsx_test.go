package jsx

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The tests run against the NATS of a stand. They are skipped when
// OC_EVENTS_ENDPOINT is unset, so that go test stays hermetic.
func connect(t *testing.T) *Conn {
	t.Helper()

	endpoint := os.Getenv("OC_EVENTS_ENDPOINT")
	if endpoint == "" {
		t.Skip("OC_EVENTS_ENDPOINT is unset, no stand to talk to")
	}

	conn, err := Connect(Config{Name: "jsx-test", Endpoint: endpoint}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func TestPing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := connect(t).Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	conn := connect(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	const stream, subject = "jsx-test", "jsx-test.events"
	t.Cleanup(func() { _ = conn.JetStream().DeleteStream(context.Background(), stream) })

	if _, err := conn.EnsureStream(ctx, jetstream.StreamConfig{
		Name:       stream,
		Subjects:   []string{subject},
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		MaxAge:     time.Hour,
		Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	// Calling it again has to converge, not fail.
	if _, err := conn.EnsureStream(ctx, jetstream.StreamConfig{
		Name:       stream,
		Subjects:   []string{subject},
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		MaxAge:     2 * time.Hour,
		Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("EnsureStream twice: %v", err)
	}

	first, err := conn.Publish(ctx, subject, "event-1", []byte(`{"n":1}`))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := conn.Publish(ctx, subject, "event-2", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The same id inside the duplicate window is stored once: this is what
	// keeps an at-least-once redelivery from doubling the feed.
	again, err := conn.Publish(ctx, subject, "event-1", []byte(`{"n":1}`))
	if err != nil {
		t.Fatalf("Publish duplicate: %v", err)
	}
	if again.Seq != first.Seq || !again.Duplicate || first.Duplicate {
		t.Errorf("duplicate got %+v, first was %+v", again, first)
	}

	bounds, err := conn.Bounds(ctx, stream)
	if err != nil {
		t.Fatalf("Bounds: %v", err)
	}
	if bounds.Messages != 2 {
		t.Errorf("Messages = %d, want 2", bounds.Messages)
	}
	if bounds.FirstSeq != first.Seq {
		t.Errorf("FirstSeq = %d, want %d", bounds.FirstSeq, first.Seq)
	}

	messages, err := conn.ReadFrom(ctx, stream, bounds.FirstSeq, 10)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("ReadFrom returned %d messages, want 2", len(messages))
	}
	if messages[0].Seq != bounds.FirstSeq || string(messages[0].Data) != `{"n":1}` {
		t.Errorf("first message = %+v", messages[0])
	}

	// Reading past the end is how a caught up client polls: empty, no error.
	tail, err := conn.ReadFrom(ctx, stream, bounds.LastSeq+1, 10)
	if err != nil {
		t.Fatalf("ReadFrom past the end: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("ReadFrom past the end returned %d messages", len(tail))
	}
}

// The extensions consume the stream of the platform without changing it.
func TestConsumerOnMainQueue(t *testing.T) {
	conn := connect(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	const durable = "jsx-test"
	t.Cleanup(func() { _ = conn.JetStream().DeleteConsumer(context.Background(), "main-queue", durable) })

	before, err := conn.Bounds(ctx, "main-queue")
	if err != nil {
		t.Fatalf("Bounds of main-queue: %v", err)
	}

	consumer, err := conn.EnsureConsumer(ctx, "main-queue", jetstream.ConsumerConfig{
		Durable:       durable,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		BackOff:       Backoff(3, time.Second, time.Minute),
		MaxDeliver:    3,
	})
	if err != nil {
		t.Fatalf("EnsureConsumer: %v", err)
	}

	batch, err := consumer.FetchNoWait(1)
	if err != nil {
		t.Fatalf("FetchNoWait: %v", err)
	}
	got := 0
	for message := range batch.Messages() {
		got++
		if err := message.Ack(); err != nil {
			t.Errorf("Ack: %v", err)
		}
	}
	if before.Messages > 0 && got == 0 {
		t.Error("the stream holds messages but the consumer delivered none")
	}

	lag, err := Lag(ctx, consumer)
	if err != nil {
		t.Fatalf("Lag: %v", err)
	}
	if before.Messages > 0 && lag == 0 {
		t.Error("lag is zero although the stream was not consumed to the end")
	}

	after, err := conn.Bounds(ctx, "main-queue")
	if err != nil {
		t.Fatalf("Bounds of main-queue: %v", err)
	}
	if after.FirstSeq != before.FirstSeq || after.LastSeq < before.LastSeq {
		t.Errorf("main-queue changed under us: %+v then %+v", before, after)
	}
}

func TestEnsureConsumerNeedsAName(t *testing.T) {
	conn := connect(t)
	if _, err := conn.EnsureConsumer(t.Context(), "main-queue", jetstream.ConsumerConfig{}); err != ErrNoDurableName {
		t.Errorf("EnsureConsumer without a name: %v, want ErrNoDurableName", err)
	}
}

func TestBackoff(t *testing.T) {
	schedule := Backoff(5, time.Second, 4*time.Second)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	for i := range want {
		if schedule[i] != want[i] {
			t.Errorf("step %d = %s, want %s", i, schedule[i], want[i])
		}
	}
	if Backoff(0, time.Second, time.Minute) != nil {
		t.Error("Backoff(0) returned a schedule")
	}
}

func TestAttempt(t *testing.T) {
	for _, tc := range []struct {
		delivered uint64
		steps     int
		want      int
	}{{0, 5, 0}, {1, 5, 0}, {2, 5, 1}, {5, 5, 4}, {6, 5, 4}, {1 << 40, 5, 4}, {3, 0, 0}} {
		if got := Attempt(tc.delivered, tc.steps); got != tc.want {
			t.Errorf("Attempt(%d, %d) = %d, want %d", tc.delivered, tc.steps, got, tc.want)
		}
	}
}
