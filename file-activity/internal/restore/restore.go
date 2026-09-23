// Package restore rebuilds the files of the platform somewhere else from the
// tree and the blobs: every file the tree knows is read from the bucket of
// the platform by its blob and written to a WebDAV endpoint at its path.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// Blobs is the bucket of the platform, read by key.
type Blobs interface {
	Get(ctx context.Context, key string) (io.ReadCloser, *s3store.Object, error)
}

// Sink is where the files go.
type Sink interface {
	// Exists reports whether a file of this size is already there.
	Exists(ctx context.Context, space tree.Space, e tree.Entry) (bool, error)
	// Put writes a file at its path, creating the folders above it.
	Put(ctx context.Context, space tree.Space, e tree.Entry, body io.Reader) error
}

// Options narrow and pace a run.
type Options struct {
	// Spaces limits the run to these space ids; empty means every space the
	// tree holds files of.
	Spaces []string
	// Prefix limits the run to the files under this path of each space.
	Prefix string
	// Workers is how many files travel at once.
	Workers int
	// DryRun lists what would be written and writes nothing.
	DryRun bool
	// SkipExisting leaves alone what the sink already holds with the same
	// size, which makes a run resumable.
	SkipExisting bool
}

// Report is what a run did.
type Report struct {
	mu       sync.Mutex
	Spaces   int
	Restored int
	Skipped  int
	Failed   int
	// NoBlob lists the files that cannot be restored, as space/path: the
	// tree knows them but not their blob.
	NoBlob []string
	// Errors lists what failed, as space/path: reason.
	Errors []string
}

// OK reports whether every file the tree knows came through.
func (r *Report) OK() bool {
	return r.Failed == 0 && len(r.NoBlob) == 0
}

func (r *Report) add(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

// Run restores the files of the tree into the sink.
func Run(ctx context.Context, t *tree.Tree, blobs Blobs, sink Sink, opts Options, log *slog.Logger) (*Report, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}

	ids, err := t.SpaceIDs(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)

	report := &Report{}
	for _, id := range ids {
		if len(opts.Spaces) > 0 && !contains(opts.Spaces, id) {
			continue
		}
		space, err := t.Space(ctx, id)
		if errors.Is(err, tree.ErrNotFound) {
			// A space that predates the service: known by its files only.
			space = &tree.Space{ID: id, Name: id}
		} else if err != nil {
			return report, err
		}
		report.Spaces++
		if err := restoreSpace(ctx, t, blobs, sink, *space, opts, report, log.With(slog.String("space", id), slog.String("name", space.Name))); err != nil {
			return report, fmt.Errorf("restore: space %s: %w", id, err)
		}
	}
	sort.Strings(report.NoBlob)
	sort.Strings(report.Errors)
	return report, nil
}

func restoreSpace(ctx context.Context, t *tree.Tree, blobs Blobs, sink Sink, space tree.Space, opts Options, report *Report, log *slog.Logger) error {
	jobs := make(chan tree.Entry)
	var wg sync.WaitGroup
	for range opts.Workers {
		wg.Go(func() {
			for e := range jobs {
				restoreFile(ctx, blobs, sink, space, e, opts, report, log)
			}
		})
	}

	err := t.Walk(ctx, space.ID, opts.Prefix, func(e tree.Entry) error {
		select {
		case jobs <- e:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(jobs)
	wg.Wait()
	return err
}

func restoreFile(ctx context.Context, blobs Blobs, sink Sink, space tree.Space, e tree.Entry, opts Options, report *Report, log *slog.Logger) {
	where := space.ID + e.Path
	if e.BlobID == "" {
		report.add(func() { report.NoBlob = append(report.NoBlob, where) })
		return
	}

	fail := func(err error) {
		log.Error("not restored", slog.String("path", e.Path), slog.Any("error", err))
		report.add(func() {
			report.Failed++
			report.Errors = append(report.Errors, where+": "+err.Error())
		})
	}

	if opts.SkipExisting {
		exists, err := sink.Exists(ctx, space, e)
		if err != nil {
			fail(err)
			return
		}
		if exists {
			report.add(func() { report.Skipped++ })
			return
		}
	}
	if opts.DryRun {
		log.Info("would restore", slog.String("path", e.Path), slog.String("blob", e.BlobKey()), slog.Uint64("size", e.Size))
		report.add(func() { report.Restored++ })
		return
	}

	body, object, err := blobs.Get(ctx, e.BlobKey())
	if err != nil {
		fail(fmt.Errorf("blob %s: %w", e.BlobKey(), err))
		return
	}
	defer body.Close()
	if object.Size >= 0 && uint64(object.Size) != e.Size {
		fail(fmt.Errorf("blob %s is %d bytes, the tree says %d", e.BlobKey(), object.Size, e.Size))
		return
	}

	if err := sink.Put(ctx, space, e, body); err != nil {
		fail(err)
		return
	}
	log.Info("restored", slog.String("path", e.Path), slog.Uint64("size", e.Size))
	report.add(func() { report.Restored++ })
}

func contains(ids []string, id string) bool {
	for _, known := range ids {
		if known == id || strings.HasSuffix(known, "$"+id) {
			return true
		}
	}
	return false
}
