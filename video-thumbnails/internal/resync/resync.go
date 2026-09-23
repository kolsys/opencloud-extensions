// Package resync brings the bucket of thumbnails in line with the platform
// in one pass over two listings: the videos the platform holds, read through
// the gateway, and the masters the bucket holds. A video without a current
// master gets a job; a master without a file is an orphan. Nothing but
// keys and metadata is read.
package resync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

// Platform is the side of the gateway the resync reads.
type Platform interface {
	ListSpaces(ctx context.Context) ([]cs3.Space, error)
	ListContainer(ctx context.Context, ref cs3.Ref) ([]cs3.ResourceInfo, error)
	ListRecycle(ctx context.Context, root cs3.Ref) ([]cs3.RecycleItem, error)
}

// Thumbs is the bucket side the resync needs.
type Thumbs interface {
	WalkMasters(ctx context.Context, fn func(spaceID, fileID string) error) error
	HeadMaster(ctx context.Context, spaceID, fileID string) (*store.Master, error)
	DeleteFile(ctx context.Context, spaceID, fileID string) (int, error)
}

// Enqueuer raises the jobs of the videos without a current master.
type Enqueuer interface {
	Enqueue(ctx context.Context, subject string, job queue.Job) (bool, error)
}

// Options narrow a run.
type Options struct {
	// Spaces limits the run to these space ids; empty means every personal
	// and project space, and every master of a space that is gone.
	Spaces []string
	// DryRun reports what would be done and does nothing.
	DryRun bool
	// DeleteOrphans removes the masters of files the platform does not hold
	// any more. The files inside a folder in the trash bin cannot be told
	// apart from them: the platform lists such a folder without the ids of
	// its files. Their masters go too and are rendered again once the folder
	// is restored, by the next run or by the first view.
	DeleteOrphans bool
}

// Report is what a run found and did.
type Report struct {
	Spaces  int
	Videos  int
	Masters int
	// Current is how many videos have the master of their version.
	Current int
	// Missing and Stale are the videos a job was raised for: without a
	// master, and with the master of an older version.
	Missing int
	Stale   int
	// Orphans is how many masters belong to no file, Deleted how many of
	// them were removed.
	Orphans int
	Deleted int
	// OrphanKeys lists the orphans as space/file, for a run that reports
	// them instead of removing them.
	OrphanKeys []string
}

// Run resyncs the bucket with the platform.
func Run(ctx context.Context, platform Platform, thumbs Thumbs, q Enqueuer, matcher *video.Matcher, opts Options, log *slog.Logger) (*Report, error) {
	spaces, err := platform.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}

	// Every master of the bucket, by space and file.
	report := &Report{}
	masters := map[string]map[string]bool{}
	if err := thumbs.WalkMasters(ctx, func(spaceID, fileID string) error {
		if masters[spaceID] == nil {
			masters[spaceID] = map[string]bool{}
		}
		masters[spaceID][fileID] = true
		report.Masters++
		return nil
	}); err != nil {
		return nil, err
	}

	for _, space := range spaces {
		if !wanted(space, opts.Spaces) {
			continue
		}
		report.Spaces++
		if err := syncSpace(ctx, platform, thumbs, q, matcher, space, masters[space.Root.SpaceID], opts, report, log.With(slog.String("space", space.Root.SpaceID), slog.String("name", space.Name))); err != nil {
			return report, fmt.Errorf("resync: space %s (%s): %w", space.Root.SpaceID, space.Name, err)
		}
		delete(masters, space.Root.SpaceID)
	}

	// What is left belongs to spaces the platform does not list any more.
	if len(opts.Spaces) == 0 {
		for spaceID, files := range masters {
			for fileID := range files {
				if err := orphan(ctx, thumbs, spaceID, fileID, opts, report, log); err != nil {
					return report, err
				}
			}
		}
	}

	sort.Strings(report.OrphanKeys)
	return report, nil
}

func wanted(space cs3.Space, only []string) bool {
	if space.Type != "personal" && space.Type != "project" {
		return false
	}
	if len(only) == 0 {
		return true
	}
	for _, id := range only {
		if id == space.Root.SpaceID || id == space.ID {
			return true
		}
	}
	return false
}

func syncSpace(ctx context.Context, platform Platform, thumbs Thumbs, q Enqueuer, matcher *video.Matcher, space cs3.Space, masters map[string]bool, opts Options, report *Report, log *slog.Logger) error {
	spaceID := space.Root.SpaceID

	videos := map[string]cs3.ResourceInfo{}
	if err := walk(ctx, platform, matcher, space.Root, videos); err != nil {
		return err
	}
	report.Videos += len(videos)

	for fileID, info := range videos {
		if !masters[fileID] {
			report.Missing++
			if err := raise(ctx, q, info, opts, log); err != nil {
				return err
			}
			continue
		}
		master, err := thumbs.HeadMaster(ctx, spaceID, fileID)
		if err != nil && !errors.Is(err, s3store.ErrNotFound) {
			return err
		}
		if master.Current(strings.Trim(info.ETag, `"`)) {
			report.Current++
			continue
		}
		report.Stale++
		if err := raise(ctx, q, info, opts, log); err != nil {
			return err
		}
	}

	// A master of a file in the trash bin is kept: the file may come back.
	items, err := platform.ListRecycle(ctx, space.Root)
	if err != nil {
		return err
	}
	trashed := map[string]bool{}
	for _, item := range items {
		trashed[item.Key] = true
	}
	for fileID := range masters {
		if _, live := videos[fileID]; live || trashed[fileID] {
			continue
		}
		if err := orphan(ctx, thumbs, spaceID, fileID, opts, report, log); err != nil {
			return err
		}
	}
	return nil
}

// walk lists a folder and descends, collecting the videos by id.
func walk(ctx context.Context, platform Platform, matcher *video.Matcher, ref cs3.Ref, into map[string]cs3.ResourceInfo) error {
	infos, err := platform.ListContainer(ctx, ref)
	if err != nil {
		if errors.Is(err, cs3.ErrNotFound) {
			return nil
		}
		return err
	}
	for _, info := range infos {
		if info.IsDir {
			if err := walk(ctx, platform, matcher, info.ID, into); err != nil {
				return err
			}
			continue
		}
		if matcher.Match(info.MimeType, path.Base(info.Name)) {
			into[info.ID.OpaqueID] = info
		}
	}
	return nil
}

// raise puts the job of a video into the queue.
func raise(ctx context.Context, q Enqueuer, info cs3.ResourceInfo, opts Options, log *slog.Logger) error {
	job := queue.Job{
		StorageID: info.ID.StorageID,
		SpaceID:   info.ID.SpaceID,
		FileID:    info.ID.OpaqueID,
		ETag:      strings.Trim(info.ETag, `"`),
		Mime:      info.MimeType,
		Size:      info.Size,
		Name:      info.Name,
	}
	if opts.DryRun {
		log.Info("would raise a job", slog.String("job", job.ID()), slog.String("name", info.Name))
		return nil
	}
	if _, err := q.Enqueue(ctx, queue.SubjectJobs, job); err != nil {
		return err
	}
	log.Info("job raised", slog.String("job", job.ID()), slog.String("name", info.Name))
	return nil
}

// orphan records a master without a file and removes it when asked to.
func orphan(ctx context.Context, thumbs Thumbs, spaceID, fileID string, opts Options, report *Report, log *slog.Logger) error {
	report.Orphans++
	report.OrphanKeys = append(report.OrphanKeys, spaceID+"/"+fileID)
	if !opts.DeleteOrphans || opts.DryRun {
		return nil
	}
	if _, err := thumbs.DeleteFile(ctx, spaceID, fileID); err != nil {
		return err
	}
	report.Deleted++
	log.Info("orphan removed", slog.String("space", spaceID), slog.String("file", fileID))
	return nil
}
