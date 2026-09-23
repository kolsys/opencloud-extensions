package jsx

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// Bounds are the sequence numbers a stream currently holds. A feed reports
// them so that a client can tell whether its cursor is still inside.
type Bounds struct {
	FirstSeq uint64
	LastSeq  uint64
	Messages uint64
}

// EnsureStream creates the stream or converges an existing one to cfg. It is
// called at every start, so a changed retention or limit takes effect without
// touching the server by hand.
func (c *Conn) EnsureStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	stream, err := c.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("jsx: stream %s: %w", cfg.Name, err)
	}
	return stream, nil
}

// Bounds reads the sequence numbers of a stream.
func (c *Conn) Bounds(ctx context.Context, stream string) (Bounds, error) {
	handle, err := c.js.Stream(ctx, stream)
	if err != nil {
		return Bounds{}, fmt.Errorf("jsx: stream %s: %w", stream, err)
	}

	info, err := handle.Info(ctx)
	if err != nil {
		return Bounds{}, fmt.Errorf("jsx: stream %s: info: %w", stream, err)
	}

	return Bounds{
		FirstSeq: info.State.FirstSeq,
		LastSeq:  info.State.LastSeq,
		Messages: info.State.Msgs,
	}, nil
}

// Ack is what the server says about a published message.
type Ack struct {
	// Seq is the sequence of the message in the stream.
	Seq uint64
	// Duplicate reports that a message with the same id was stored inside
	// the duplicate window already; Seq is the one of that message.
	Duplicate bool
}

// Publish writes a message to a subject. The id becomes the Nats-Msg-Id
// header, so that a redelivered event published twice is stored once as long
// as it falls inside the duplicate window of the stream.
func (c *Conn) Publish(ctx context.Context, subject, id string, data []byte) (Ack, error) {
	options := []jetstream.PublishOpt{}
	if id != "" {
		options = append(options, jetstream.WithMsgID(id))
	}

	ack, err := c.js.Publish(ctx, subject, data, options...)
	if err != nil {
		return Ack{}, fmt.Errorf("jsx: publish to %s: %w", subject, err)
	}
	return Ack{Seq: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

// Message is one message read out of a stream by sequence number.
type Message struct {
	Seq  uint64
	Data []byte
}

// ReadFrom returns up to limit messages of the stream starting at sequence
// seq, without waiting for messages that are not there yet. An empty result
// means the reader has caught up.
//
// It reads through an ordered consumer, which the server cleans up on its
// own once it goes idle.
func (c *Conn) ReadFrom(ctx context.Context, stream string, seq uint64, limit int) ([]Message, error) {
	if limit < 1 {
		return nil, nil
	}

	consumer, err := c.js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   seq,
	})
	if err != nil {
		return nil, fmt.Errorf("jsx: read %s from %d: %w", stream, seq, err)
	}

	batch, err := consumer.FetchNoWait(limit)
	if err != nil {
		return nil, fmt.Errorf("jsx: read %s from %d: %w", stream, seq, err)
	}

	messages := make([]Message, 0, limit)
	for message := range batch.Messages() {
		metadata, err := message.Metadata()
		if err != nil {
			return nil, fmt.Errorf("jsx: read %s from %d: metadata: %w", stream, seq, err)
		}
		messages = append(messages, Message{Seq: metadata.Sequence.Stream, Data: message.Data()})
	}
	if err := batch.Error(); err != nil {
		return nil, fmt.Errorf("jsx: read %s from %d: %w", stream, seq, err)
	}

	return messages, nil
}
