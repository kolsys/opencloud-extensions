// Package webhook pushes every entry of the feed to an HTTP endpoint, in
// order, retrying until the endpoint accepts it.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

// Headers of a delivery.
const (
	// SignatureHeader carries the HMAC-SHA256 of the body, hex encoded and
	// prefixed with the algorithm, like "sha256=…".
	SignatureHeader = "X-File-Activity-Signature"
	// SeqHeader repeats the sequence number of the entry.
	SeqHeader = "X-File-Activity-Seq"
)

// Delivery and redelivery.
const (
	requestTimeout = 30 * time.Second
	ackWait        = 2 * time.Minute
	backoffSteps   = 8
	backoffBase    = 5 * time.Second
	backoffLimit   = 10 * time.Minute
	maxDrain       = 4096
)

// Pusher is the consumer of the feed that posts every entry to the endpoint.
type Pusher struct {
	conn    *jsx.Conn
	store   *feed.Store
	url     string
	secret  config.Secret
	http    *http.Client
	metrics *metrics.Metrics
	log     *slog.Logger
	group   string
	agent   string
	backoff []time.Duration
}

// New returns a pusher for the endpoint. The group names the durable
// consumer on the feed.
func New(conn *jsx.Conn, store *feed.Store, url string, secret config.Secret, group, agent string, m *metrics.Metrics, log *slog.Logger) *Pusher {
	return &Pusher{
		conn:    conn,
		store:   store,
		url:     url,
		secret:  secret,
		http:    &http.Client{Timeout: requestTimeout},
		metrics: m,
		log:     log,
		group:   group,
		agent:   agent,
		backoff: jsx.Backoff(backoffSteps, backoffBase, backoffLimit),
	}
}

// Run delivers until ctx is cancelled. Entries go one at a time and an entry
// is acknowledged only on a 2xx, so a failing endpoint stalls the delivery
// instead of losing entries; there is no limit on the retries.
func (p *Pusher) Run(ctx context.Context) error {
	consumer, err := p.conn.EnsureConsumer(ctx, p.store.Stream(), jetstream.ConsumerConfig{
		Durable:       p.group,
		Description:   "file-activity: push of the feed to the webhook",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       ackWait,
		MaxDeliver:    -1,
		MaxAckPending: 1,
	})
	if err != nil {
		return err
	}

	consume, err := consumer.Consume(func(message jetstream.Msg) {
		p.handle(ctx, message)
	})
	if err != nil {
		return err
	}
	defer consume.Stop()

	p.log.Info("pushing the feed", slog.String("url", p.url), slog.String("group", p.group))
	<-ctx.Done()
	return nil
}

func (p *Pusher) handle(ctx context.Context, message jetstream.Msg) {
	metadata, err := message.Metadata()
	if err != nil {
		p.log.Error("message without metadata", slog.Any("error", err))
		_ = message.Term()
		return
	}
	seq := metadata.Sequence.Stream

	entry, err := feed.Decode(message.Data(), seq)
	if err != nil {
		p.log.Error("malformed entry in the feed", slog.Uint64("seq", seq), slog.Any("error", err))
		_ = message.Term()
		return
	}

	code, err := p.deliver(ctx, entry)
	p.metrics.Webhook.WithLabelValues(strconv.Itoa(code)).Inc()
	if err != nil {
		attempt := jsx.Attempt(metadata.NumDelivered, len(p.backoff))
		p.log.Warn("delivery failed, will retry",
			slog.Uint64("seq", seq), slog.Int("status", code), slog.Duration("in", p.backoff[attempt]), slog.Any("error", err))
		_ = message.NakWithDelay(p.backoff[attempt])
		return
	}

	if err := message.Ack(); err != nil {
		p.log.Warn("ack failed", slog.Uint64("seq", seq), slog.Any("error", err))
	}
}

// deliver posts one entry and returns the status code of the endpoint, zero
// when it did not answer at all.
func (p *Pusher) deliver(ctx context.Context, entry feed.Event) (int, error) {
	body, err := json.Marshal(entry)
	if err != nil {
		return 0, fmt.Errorf("webhook: encode %d: %w", entry.Seq, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("webhook: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", p.agent)
	request.Header.Set(SeqHeader, strconv.FormatUint(entry.Seq, 10))
	if !p.secret.Empty() {
		request.Header.Set(SignatureHeader, Sign(p.secret.Reveal(), body))
	}

	// The endpoint is what the operator configured, not user input.
	response, err := p.http.Do(request) //nolint:gosec // G704
	if err != nil {
		return 0, fmt.Errorf("webhook: post %d: %w", entry.Seq, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrain))

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, fmt.Errorf("webhook: post %d: endpoint answered %s", entry.Seq, response.Status)
	}
	return response.StatusCode, nil
}

// Sign computes the signature of a body the way the header carries it.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature against a body. Receivers use it.
func Verify(secret string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}
