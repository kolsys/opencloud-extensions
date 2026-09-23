package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	vterrors "github.com/kolsys/opencloud-extensions/video-thumbnails/internal/errors"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
)

// Results of a job, the label of the jobs metric.
const (
	ResultOK      = "ok"
	ResultSkipped = "skipped"
	ResultRetry   = "retry"
	ResultFailed  = "failed"
)

// Timing of a job.
const (
	// ackWait is how long a job may run without a sign of life before the
	// server hands it to another worker; progress is reported well within.
	ackWait       = 2 * time.Minute
	progressEvery = ackWait / 3
	depthInterval = 15 * time.Second
)

// Worker runs the jobs of the queue: one pool of goroutines per subject, so
// that the previews missed by the web never wait behind a backfill.
type Worker struct {
	queue   *queue.Queue
	store   *store.Store
	cs3     *cs3.Client
	source  *Source
	ffmpeg  FFmpeg
	metrics *metrics.Metrics
	log     *slog.Logger
	workers int
	urgent  int
	backoff []time.Duration
}

// New returns a worker with the given pool sizes.
func New(q *queue.Queue, st *store.Store, client *cs3.Client, source *Source, ffmpeg FFmpeg, m *metrics.Metrics, workers, urgent int, log *slog.Logger) *Worker {
	return &Worker{
		queue:   q,
		store:   st,
		cs3:     client,
		source:  source,
		ffmpeg:  ffmpeg,
		metrics: m,
		log:     log,
		workers: workers,
		urgent:  urgent,
		backoff: queue.Backoff(),
	}
}

// Run processes jobs until ctx is cancelled and waits for the running ones.
func (w *Worker) Run(ctx context.Context) error {
	jobs, err := w.queue.Consumer(ctx, queue.SubjectJobs, ackWait)
	if err != nil {
		return err
	}
	urgent, err := w.queue.Consumer(ctx, queue.SubjectUrgent, ackWait)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for range w.workers {
		wg.Go(func() { w.pull(ctx, jobs, queue.SubjectJobs) })
	}
	for range w.urgent {
		wg.Go(func() { w.pull(ctx, urgent, queue.SubjectUrgent) })
	}
	wg.Go(func() { w.observe(ctx) })

	w.log.Info("workers started", slog.Int("jobs", w.workers), slog.Int("urgent", w.urgent))
	wg.Wait()
	return nil
}

// pull takes jobs one at a time until ctx is cancelled.
func (w *Worker) pull(ctx context.Context, consumer jetstream.Consumer, subject string) {
	for ctx.Err() == nil {
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(progressEvery))
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("fetch failed", slog.String("subject", subject), slog.Any("error", err))
				time.Sleep(time.Second)
			}
			continue
		}
		for message := range batch.Messages() {
			w.process(ctx, message)
		}
	}
}

// process runs one job. The message is acknowledged on success and on a
// skip; a failure is redelivered after a growing wait until MaxDeliver, when
// the failure marker is stored instead.
func (w *Worker) process(ctx context.Context, message jetstream.Msg) {
	job, err := queue.Decode(message.Data())
	if err != nil {
		w.log.Error("malformed job", slog.Any("error", err))
		_ = message.Term()
		return
	}

	attempt := uint64(1)
	if metadata, err := message.Metadata(); err == nil {
		attempt = metadata.NumDelivered
	}
	log := w.log.With(slog.String("job", job.ID()), slog.Uint64("attempt", attempt))

	stop := w.keepAlive(ctx, message)
	started := time.Now()
	err = w.render(ctx, job)
	stop()

	switch {
	case err == nil:
		w.metrics.Jobs.WithLabelValues(ResultOK).Inc()
		w.metrics.JobDuration.Observe(time.Since(started).Seconds())
		log.Info("thumbnail stored", slog.Duration("took", time.Since(started)))
		w.ack(message, log)
	case errors.Is(err, vterrors.ErrGone):
		w.metrics.Jobs.WithLabelValues(ResultSkipped).Inc()
		log.Info("job skipped", slog.Any("reason", err))
		w.ack(message, log)
	case attempt >= queue.MaxDeliver:
		w.metrics.Jobs.WithLabelValues(ResultFailed).Inc()
		log.Error("generation given up", slog.Any("error", err))
		w.giveUp(ctx, job, attempt, err, log)
		_ = message.Term()
	default:
		w.metrics.Jobs.WithLabelValues(ResultRetry).Inc()
		wait := w.backoff[jsx.Attempt(attempt, len(w.backoff))]
		log.Warn("generation failed, will retry", slog.Duration("in", wait), slog.Any("error", err))
		if err := message.NakWithDelay(wait); err != nil {
			log.Warn("nak failed", slog.Any("error", err))
		}
	}
}

// render produces and stores the master of a job.
func (w *Worker) render(ctx context.Context, job queue.Job) error {
	ref := cs3.Ref{StorageID: job.StorageID, SpaceID: job.SpaceID, OpaqueID: job.FileID}

	// The file may be gone or replaced since the job was raised; a replaced
	// file has a job of its own.
	info, err := w.cs3.Stat(ctx, ref)
	switch {
	case errors.Is(err, cs3.ErrNotFound):
		return vterrors.ErrGone
	case err != nil:
		return err
	case strings.Trim(info.ETag, `"`) != job.ETag:
		return vterrors.ErrGone
	}

	// At-least-once: the job may run again after a crash between the store
	// and the ack.
	master, err := w.store.HeadMaster(ctx, job.SpaceID, job.FileID)
	if err != nil && !errors.Is(err, s3store.ErrNotFound) {
		return err
	}
	if master.Current(job.ETag) {
		return nil
	}

	// The bytes are read by space root and path: the data server does not
	// serve a download by id.
	source, cleanup, err := w.source.Open(ctx, info.PathRef(), info.Size, info.Name)
	if err != nil {
		return err
	}
	defer cleanup()

	frame, err := w.ffmpeg.Frame(ctx, source)
	if err != nil {
		return err
	}

	return w.store.PutMaster(ctx, job.SpaceID, job.FileID, frame, store.Master{
		ETag:       job.ETag,
		Mime:       info.MimeType,
		SourceSize: int64(info.Size), //nolint:gosec // a file size fits
	})
}

// giveUp stores the failure marker so that the previews stop asking.
func (w *Worker) giveUp(ctx context.Context, job queue.Job, attempts uint64, cause error, log *slog.Logger) {
	err := w.store.PutFailure(ctx, job.SpaceID, job.FileID, store.Failure{
		ETag:     job.ETag,
		Error:    cause.Error(),
		Attempts: int(attempts), //nolint:gosec // bounded by MaxDeliver
		FailedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Error("failure marker not stored", slog.Any("error", err))
	}
}

// keepAlive tells the server the job is still running, so that a long
// download or a slow ffmpeg does not get the job redelivered mid-flight.
func (w *Worker) keepAlive(ctx context.Context, message jetstream.Msg) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = message.InProgress()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (w *Worker) ack(message jetstream.Msg, log *slog.Logger) {
	if err := message.Ack(); err != nil {
		log.Warn("ack failed", slog.Any("error", err))
	}
}

// observe refreshes the depth gauges of both subjects.
func (w *Worker) observe(ctx context.Context) {
	ticker := time.NewTicker(depthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, subject := range []string{queue.SubjectJobs, queue.SubjectUrgent} {
				if depth, err := w.queue.Depth(ctx, subject); err == nil {
					w.metrics.QueueDepth.WithLabelValues(subject).Set(float64(depth))
				}
			}
		}
	}
}
