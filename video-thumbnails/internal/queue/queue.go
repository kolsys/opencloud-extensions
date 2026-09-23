// Package queue is the work queue of thumbnail jobs: a JetStream stream with
// one subject for the jobs raised by events and backfill, and one for the
// previews the web asked for and missed. A job is stored once per version of
// a file: its message id is fileid and etag.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/config"
)

// Names of the stream and its subjects.
const (
	Stream        = config.Name + "-jobs"
	SubjectJobs   = config.Name + ".jobs"
	SubjectUrgent = config.Name + ".urgent"
)

// Redelivery of a job whose generation failed.
const (
	// MaxDeliver is how often a job runs before it is given up on and the
	// failure marker is stored.
	MaxDeliver = 5

	// duplicateWindow keeps a redelivered event from storing its job twice;
	// the redeliveries of the reactor end well within it. It also bounds how
	// long a job of a version cannot be raised again after its master was
	// removed by hand, an import or a resync.
	duplicateWindow = 15 * time.Minute
	backoffBase     = 10 * time.Second
	backoffLimit    = 5 * time.Minute
)

// Backoff is the wait before each retry of a failed job. The worker applies
// it when it declines a message: a server side backoff would replace the ack
// wait of the consumer with its first entry.
func Backoff() []time.Duration {
	return jsx.Backoff(MaxDeliver-1, backoffBase, backoffLimit)
}

// Job is one thumbnail to render: a version of a file.
type Job struct {
	StorageID string `json:"storage_id"`
	SpaceID   string `json:"space_id"`
	FileID    string `json:"file_id"`
	ETag      string `json:"etag"`
	Mime      string `json:"mime"`
	Size      uint64 `json:"size"`
	Name      string `json:"name"`
}

// ID is the deduplication key of the job.
func (j Job) ID() string {
	return j.FileID + ":" + j.ETag
}

// Queue is the stream of jobs.
type Queue struct {
	conn *jsx.Conn
}

// Open converges the stream to its configuration and returns the queue.
func Open(ctx context.Context, conn *jsx.Conn) (*Queue, error) {
	if _, err := conn.EnsureStream(ctx, jetstream.StreamConfig{
		Name:        Stream,
		Description: "Thumbnail jobs of video-thumbnails.",
		Subjects:    []string{SubjectJobs, SubjectUrgent},
		Retention:   jetstream.WorkQueuePolicy,
		Storage:     jetstream.FileStorage,
		Discard:     jetstream.DiscardOld,
		Duplicates:  duplicateWindow,
	}); err != nil {
		return nil, err
	}
	return &Queue{conn: conn}, nil
}

// Enqueue appends a job to a subject and reports whether it was stored: the
// same version of a file enqueued twice on one subject inside the duplicate
// window is kept once. The subjects deduplicate apart, so that a preview the
// web missed is not held back by the job an event or a backfill raised.
func (q *Queue) Enqueue(ctx context.Context, subject string, job Job) (bool, error) {
	body, err := json.Marshal(job)
	if err != nil {
		return false, fmt.Errorf("queue: encode job %s: %w", job.ID(), err)
	}
	ack, err := q.conn.Publish(ctx, subject, messageID(subject, job), body)
	if err != nil {
		return false, err
	}
	return !ack.Duplicate, nil
}

// messageID is the deduplication key of a job on a subject.
func messageID(subject string, job Job) string {
	if subject == SubjectUrgent {
		return "urgent:" + job.ID()
	}
	return job.ID()
}

// Consumer returns the durable consumer of one subject. A work queue allows
// one consumer per subject, so the name derives from it.
func (q *Queue) Consumer(ctx context.Context, subject string, ackWait time.Duration) (jetstream.Consumer, error) {
	return q.conn.EnsureConsumer(ctx, Stream, jetstream.ConsumerConfig{
		Durable:       consumerName(subject),
		Description:   "video-thumbnails: workers of " + subject,
		FilterSubject: subject,
		AckWait:       ackWait,
		MaxDeliver:    MaxDeliver,
	})
}

// Depth reports how many jobs wait on a subject.
func (q *Queue) Depth(ctx context.Context, subject string) (uint64, error) {
	consumer, err := q.conn.JetStream().Consumer(ctx, Stream, consumerName(subject))
	if err != nil {
		return 0, fmt.Errorf("queue: consumer of %s: %w", subject, err)
	}
	return jsx.Lag(ctx, consumer)
}

// Decode turns a message back into a job.
func Decode(body []byte) (Job, error) {
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		return Job{}, fmt.Errorf("queue: decode job: %w", err)
	}
	return job, nil
}

func consumerName(subject string) string {
	if subject == SubjectUrgent {
		return config.Name + "-urgent"
	}
	return config.Name + "-jobs"
}
