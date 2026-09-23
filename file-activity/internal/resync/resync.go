// Package resync brings the tree back in line with the platform: what the
// platform holds is what the tree records, at the paths of today. Run on a
// platform the service is new to, or after it was down, it is the forced
// backup. The blob of a file whose upload the service never saw is the one
// thing the platform does not tell over its APIs; it is read from the
// metadata of the platform on disk when that is at hand, and left unknown
// and reported otherwise.
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
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// BlobIDs tells the blob of a node the way the platform stores it. Nil
// leaves the blobs of files the service never saw uploaded unknown.
type BlobIDs interface {
	BlobID(ctx context.Context, spaceID, nodeID string) (string, error)
}

// Platform is the side of the gateway the resync reads.
type Platform interface {
	ListSpaces(ctx context.Context) ([]cs3.Space, error)
	ListContainer(ctx context.Context, ref cs3.Ref) ([]cs3.ResourceInfo, error)
	ListRecycle(ctx context.Context, root cs3.Ref) ([]cs3.RecycleItem, error)
}

// Options narrow a run.
type Options struct {
	// Spaces limits the run to these space ids; empty means every personal
	// and project space.
	Spaces []string
	// DryRun reports what would change and changes nothing.
	DryRun bool
}

// Report is what a run found and did.
type Report struct {
	Spaces    int
	Files     int
	Kept      int
	Rewritten int
	Stale     int
	// FromMetadata is how many files got their blob from the metadata of
	// the platform.
	FromMetadata int
	// NoBlob lists the files recorded without a blob, as space/path: their
	// upload was never seen and the metadata was not at hand or has none.
	NoBlob []string
}

// Run resyncs the tree with the platform.
func Run(ctx context.Context, platform Platform, t *tree.Tree, blobs BlobIDs, opts Options, log *slog.Logger) (*Report, error) {
	spaces, err := platform.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}

	report := &Report{}
	for _, space := range spaces {
		if !wanted(space, opts.Spaces) {
			continue
		}
		report.Spaces++
		if err := syncSpace(ctx, platform, t, blobs, space, opts.DryRun, report, log.With(slog.String("space", space.Root.SpaceID), slog.String("name", space.Name))); err != nil {
			return report, fmt.Errorf("resync: space %s (%s): %w", space.Root.SpaceID, space.Name, err)
		}
	}
	sort.Strings(report.NoBlob)
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

// found is a file of the platform at its current path.
type found struct {
	path string
	info cs3.ResourceInfo
}

func syncSpace(ctx context.Context, platform Platform, t *tree.Tree, blobs BlobIDs, space cs3.Space, dryRun bool, report *Report, log *slog.Logger) error {
	spaceID := space.Root.SpaceID

	live := map[string]found{}
	if err := walk(ctx, platform, space.Root, "/", live); err != nil {
		return err
	}
	report.Files += len(live)

	known := map[string]tree.Entry{}
	if err := t.Walk(ctx, spaceID, "/", func(e tree.Entry) error {
		known[e.FileID] = e
		return nil
	}); err != nil {
		return err
	}

	// The entries of today: the blob stays known while the version does,
	// otherwise the metadata of the platform names it, when it is at hand.
	entries := make(map[string]*tree.Entry, len(live))
	for id, f := range live {
		entry := entryOf(f.info, f.path)
		if old, ok := known[id]; ok && old.ETag == entry.ETag {
			entry.BlobID = old.BlobID
		}
		if entry.BlobID == "" && blobs != nil {
			blobID, err := blobs.BlobID(ctx, spaceID, id)
			switch {
			case errors.Is(err, ErrNoMetadata):
				log.Warn("no metadata for the file", slog.String("path", f.path))
			case err != nil:
				return err
			case blobID != "":
				entry.BlobID = blobID
				report.FromMetadata++
			}
		}
		entries[id] = &entry
	}

	for id, entry := range entries {
		if entry.BlobID == "" {
			report.NoBlob = append(report.NoBlob, spaceID+entry.Path)
		}
		old, ok := known[id]
		if ok && old.Path == entry.Path && same(old, *entry) {
			report.Kept++
			continue
		}
		report.Rewritten++
		if dryRun {
			continue
		}
		if err := t.PutFile(ctx, *entry); err != nil {
			return err
		}
		if ok && old.Path != entry.Path {
			if err := t.DeleteFile(ctx, spaceID, old.Path); err != nil {
				return err
			}
		}
	}

	// What the tree holds and the platform does not is either in the trash
	// bin, where the record moves, or gone.
	items, err := platform.ListRecycle(ctx, space.Root)
	if err != nil {
		return err
	}
	trashed := map[string]cs3.RecycleItem{}
	for _, item := range items {
		trashed[item.Key] = item
	}
	for id, old := range known {
		if _, ok := live[id]; ok {
			continue
		}
		report.Stale++
		if dryRun {
			continue
		}
		if _, ok := trashed[id]; ok {
			if err := t.ApplyTrash(ctx, nil, []tree.Entry{old}); err != nil {
				return err
			}
			continue
		}
		if err := t.DeleteFile(ctx, spaceID, old.Path); err != nil {
			return err
		}
	}

	if dryRun {
		return nil
	}
	return t.PutSpace(ctx, tree.Space{ID: spaceID, Name: space.Name, Type: space.Type, Owner: space.Owner})
}

// walk lists a folder and descends, building the paths from the names: the
// paths the platform reports differ with the shape of the reference.
func walk(ctx context.Context, platform Platform, ref cs3.Ref, dir string, into map[string]found) error {
	infos, err := platform.ListContainer(ctx, ref)
	if err != nil {
		if errors.Is(err, cs3.ErrNotFound) {
			return nil
		}
		return err
	}
	for _, info := range infos {
		p := path.Join(dir, info.Name)
		if info.IsDir {
			if err := walk(ctx, platform, info.ID, p, into); err != nil {
				return err
			}
			continue
		}
		into[info.ID.OpaqueID] = found{path: p, info: info}
	}
	return nil
}

func entryOf(info cs3.ResourceInfo, p string) tree.Entry {
	return tree.Entry{
		SpaceID: info.ID.SpaceID,
		FileID:  info.ID.OpaqueID,
		Path:    tree.Clean(p),
		Size:    info.Size,
		Mime:    info.MimeType,
		ETag:    strings.Trim(info.ETag, `"`),
		MTime:   info.MTime,
		SHA1:    info.SHA1,
		MD5:     info.MD5,
	}
}

// same reports whether two entries of one file say the same.
func same(a, b tree.Entry) bool {
	return a.BlobID == b.BlobID && a.Size == b.Size && a.Mime == b.Mime && a.ETag == b.ETag &&
		a.MTime.Equal(b.MTime) && a.SHA1 == b.SHA1 && a.MD5 == b.MD5
}
