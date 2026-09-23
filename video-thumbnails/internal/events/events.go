// Package events reacts to the file events of the platform: a readable video
// becomes a job, a purged file or a deleted space loses its thumbnails.
//
// A file taken out of the trash bin can come back under a new id, so a
// restore raises a job too; the job is skipped when the master is already
// there, and the thumbnails left under the old id are for gc to collect.
package events

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

// Redelivery of an event whose handling failed.
const (
	maxDeliver   = 10
	backoffBase  = 2 * time.Second
	backoffLimit = 5 * time.Minute
	ackWait      = time.Minute
	lagInterval  = 15 * time.Second
)

// Resolver looks a reference up. The gateway client implements it.
type Resolver interface {
	Stat(ctx context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error)
}

// Enqueuer raises jobs. The queue implements it.
type Enqueuer interface {
	Enqueue(ctx context.Context, subject string, job queue.Job) (bool, error)
}

// Cleaner removes stored thumbnails. The store implements it.
type Cleaner interface {
	DeleteFile(ctx context.Context, spaceID, fileID string) (int, error)
	DeleteSpace(ctx context.Context, spaceID string) (int, error)
}

// Reactor consumes main-queue in its own group and turns the events into
// jobs and deletions. Every reaction is idempotent: delivery is
// at-least-once.
type Reactor struct {
	conn    *jsx.Conn
	cs3     Resolver
	queue   Enqueuer
	store   Cleaner
	metrics *metrics.Metrics
	log     *slog.Logger
	group   string
	video   *video.Matcher
	backoff []time.Duration
}

// New returns a reactor raising a job for every video the matcher recognises.
func New(conn *jsx.Conn, client Resolver, q Enqueuer, st Cleaner, matcher *video.Matcher, m *metrics.Metrics, group string, log *slog.Logger) *Reactor {
	return &Reactor{
		conn:    conn,
		cs3:     client,
		queue:   q,
		store:   st,
		metrics: m,
		log:     log,
		group:   group,
		video:   matcher,
		backoff: jsx.Backoff(maxDeliver, backoffBase, backoffLimit),
	}
}

// Run consumes until ctx is cancelled.
func (r *Reactor) Run(ctx context.Context) error {
	consumer, err := r.conn.EnsureConsumer(ctx, ocevents.MainQueue, jetstream.ConsumerConfig{
		Durable:       r.group,
		Description:   "video-thumbnails: file events into jobs",
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		MaxAckPending: 1,
	})
	if err != nil {
		return err
	}

	consume, err := consumer.Consume(func(message jetstream.Msg) {
		r.handle(ctx, message)
	})
	if err != nil {
		return err
	}
	defer consume.Stop()

	r.log.Info("consuming main-queue", slog.String("group", r.group))

	ticker := time.NewTicker(lagInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if lag, err := jsx.Lag(ctx, consumer); err == nil {
				r.metrics.ConsumerLag.Set(float64(lag))
			}
		}
	}
}

func (r *Reactor) handle(ctx context.Context, message jetstream.Msg) {
	event, err := ocevents.Decode(message.Data())
	if err != nil {
		r.log.Error("malformed message on main-queue", slog.Any("error", err))
		_ = message.Term()
		return
	}
	r.metrics.Events.WithLabelValues(event.Type).Inc()

	log := r.log.With(slog.String("event", event.ID), slog.String("type", event.Type))
	if err := r.react(ctx, event, log); err != nil {
		log.Warn("reaction failed, will retry", slog.Any("error", err))
		var delivered uint64
		if metadata, err := message.Metadata(); err == nil {
			delivered = metadata.NumDelivered
		}
		_ = message.NakWithDelay(r.backoff[jsx.Attempt(delivered, len(r.backoff))])
		return
	}
	if err := message.Ack(); err != nil {
		log.Warn("ack failed", slog.Any("error", err))
	}
}

// react performs the reaction of the event. A file that is gone by the time
// it is looked up is no error: nothing is left to do for it.
func (r *Reactor) react(ctx context.Context, event ocevents.Event, log *slog.Logger) error {
	switch payload := event.Payload.(type) {
	case *ocevents.UploadReady:
		if payload.Failed {
			return nil
		}
		return r.enqueue(ctx, refOf(payload.FileRef), log)
	case *ocevents.FileVersionRestored:
		return r.enqueue(ctx, refOf(payload.Ref), log)
	case *ocevents.ItemRestored:
		return r.enqueue(ctx, refOf(payload.Ref), log)
	case *ocevents.ItemPurged:
		// ID is empty on the wire; the ids of the purged item are in Ref.
		id := payload.Ref
		if id == nil || id.ResourceID.Empty() {
			return nil
		}
		removed, err := r.store.DeleteFile(ctx, id.ResourceID.SpaceID, id.ResourceID.OpaqueID)
		if err != nil {
			return err
		}
		log.Info("thumbnails of a purged file removed", slog.Int("objects", removed))
		return nil
	case *ocevents.SpaceDeleted:
		spaceID := spaceIDOf(payload.ID)
		if spaceID == "" {
			return nil
		}
		removed, err := r.store.DeleteSpace(ctx, spaceID)
		if err != nil {
			return err
		}
		log.Info("thumbnails of a deleted space removed", slog.String("space", spaceID), slog.Int("objects", removed))
		return nil
	default:
		return nil
	}
}

// enqueue looks the file up and raises a job when it is a video.
func (r *Reactor) enqueue(ctx context.Context, ref cs3.Ref, log *slog.Logger) error {
	info, err := r.cs3.Stat(ctx, ref)
	switch {
	case errors.Is(err, cs3.ErrNotFound):
		log.Info("file gone before it was looked up")
		return nil
	case err != nil:
		return err
	case info.IsDir || !r.video.Match(info.MimeType, info.Name):
		return nil
	}

	job := queue.Job{
		StorageID: info.ID.StorageID,
		SpaceID:   info.ID.SpaceID,
		FileID:    info.ID.OpaqueID,
		ETag:      strings.Trim(info.ETag, `"`),
		Mime:      info.MimeType,
		Size:      info.Size,
		Name:      info.Name,
	}
	stored, err := r.queue.Enqueue(ctx, queue.SubjectJobs, job)
	if err != nil {
		return err
	}
	if !stored {
		log.Debug("job already queued", slog.String("job", job.ID()))
		return nil
	}
	log.Info("job enqueued", slog.String("job", job.ID()), slog.String("mime", job.Mime))
	return nil
}

func refOf(ref *ocevents.Reference) cs3.Ref {
	if ref == nil {
		return cs3.Ref{}
	}
	out := cs3.Ref{Path: ref.Path}
	if ref.ResourceID != nil {
		out.StorageID = ref.ResourceID.StorageID
		out.SpaceID = ref.ResourceID.SpaceID
		out.OpaqueID = ref.ResourceID.OpaqueID
	}
	return out
}

// spaceIDOf takes the space out of storageid$spaceid or storageid$spaceid!id.
func spaceIDOf(id *ocevents.StorageSpaceID) string {
	if id == nil {
		return ""
	}
	composite := id.OpaqueID
	if _, rest, found := strings.Cut(composite, "$"); found {
		composite = rest
	}
	spaceID, _, _ := strings.Cut(composite, "!")
	return spaceID
}
