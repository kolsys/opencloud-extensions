// Package resync brings the tree back in line with the platform: what the
// platform holds is what the tree records, at the paths of today. Run on a
// platform the service is new to, or after it was down, it is the forced
// backup. The blob of a file whose upload the service never saw is the one
// thing the platform does not tell over its APIs; it is read from the
// metadata of the platform on disk when that is at hand, and left unknown
// and reported otherwise.
//
// A space takes three passes that hold about 100 bytes per file of the tree,
// whatever the size of the space: the tree is indexed, the platform is walked
// folder by folder with every file written as it is found, and the keys of
// the tree are swept for what the platform no longer holds.
package resync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// progressEvery is how many files go by between two progress lines.
const progressEvery = 10000

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
	// NoBlob is told every file recorded without a blob, as space/path, as
	// the run comes to it. Nil drops them.
	NoBlob func(file string)
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
	// NoBlob is how many files are recorded without a blob: their upload
	// was never seen and the metadata was not at hand or has none.
	NoBlob int
	// Failed lists the spaces the run gave up on, as id (name). The counts
	// of such a space are the ones up to the failure.
	Failed []string
}

// counts is what a run did in one space.
type counts struct {
	files, kept, rewritten, stale, fromMetadata, noBlob int
}

func (r *Report) add(c counts) {
	r.Files += c.files
	r.Kept += c.kept
	r.Rewritten += c.rewritten
	r.Stale += c.stale
	r.FromMetadata += c.fromMetadata
	r.NoBlob += c.noBlob
}

// Run resyncs the tree with the platform, one space after the other. A
// space that fails after the retries is listed in the report and the run
// goes on with the next one; only the end of the context stops it.
func Run(ctx context.Context, platform Platform, t *tree.Tree, blobs BlobIDs, opts Options, log *slog.Logger) (*Report, error) {
	var spaces []cs3.Space
	err := retry(ctx, log, "list spaces", func() error {
		var err error
		spaces, err = platform.ListSpaces(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	report := &Report{}
	for _, space := range spaces {
		if !wanted(space, opts.Spaces) {
			continue
		}
		report.Spaces++
		spaceLog := log.With(slog.String("space", space.Root.SpaceID), slog.String("name", space.Name))
		if err := syncSpace(ctx, platform, t, blobs, space, opts, report, spaceLog); err != nil {
			if ctx.Err() != nil {
				return report, fmt.Errorf("resync: space %s (%s): %w", space.Root.SpaceID, space.Name, err)
			}
			spaceLog.Error("space failed", slog.Any("error", err))
			report.Failed = append(report.Failed, space.Root.SpaceID+" ("+space.Name+")")
		}
	}
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

// spaceSync is one space being brought in line.
type spaceSync struct {
	platform Platform
	tree     *tree.Tree
	blobs    BlobIDs
	space    cs3.Space
	opts     Options
	index    *index
	counts   counts
	log      *slog.Logger
}

func syncSpace(ctx context.Context, platform Platform, t *tree.Tree, blobs BlobIDs, space cs3.Space, opts Options, report *Report, log *slog.Logger) error {
	s := &spaceSync{platform: platform, tree: t, blobs: blobs, space: space, opts: opts, index: newIndex(), log: log}
	defer func() { report.add(s.counts) }()

	started := time.Now()
	log.Info("space started")
	if err := s.indexTree(ctx); err != nil {
		return err
	}
	if err := s.walk(ctx, space.Root, "/"); err != nil {
		return err
	}
	if err := s.sweep(ctx); err != nil {
		return err
	}
	if !opts.DryRun {
		if err := s.retry(ctx, "put space", func() error {
			return t.PutSpace(ctx, tree.Space{ID: space.Root.SpaceID, Name: space.Name, Type: space.Type, Owner: space.Owner})
		}); err != nil {
			return err
		}
	}
	log.Info("space done", slog.Int("files", s.counts.files), slog.Int("kept", s.counts.kept), slog.Int("rewritten", s.counts.rewritten),
		slog.Int("stale", s.counts.stale), slog.Int("from_metadata", s.counts.fromMetadata), slog.Int("no_blob", s.counts.noBlob),
		slog.Duration("took", time.Since(started)))
	return nil
}

func (s *spaceSync) spaceID() string {
	return s.space.Root.SpaceID
}

func (s *spaceSync) retry(ctx context.Context, what string, fn func() error) error {
	return retry(ctx, s.log, what, fn)
}

// progress logs a count every progressEvery.
func (s *spaceSync) progress(what string, n int) {
	if n%progressEvery == 0 {
		s.log.Info(what, slog.Int("count", n))
	}
}

// indexTree reads what the tree has of the space into the index.
func (s *spaceSync) indexTree(ctx context.Context) error {
	n := 0
	err := s.tree.Walk(ctx, s.spaceID(), "/", func(e tree.Entry) error {
		s.index.add(e)
		n++
		s.progress("indexing the tree", n)
		return nil
	})
	if err != nil {
		return err
	}
	s.index.finish()
	s.log.Info("tree indexed", slog.Int("entries", s.index.size()))
	return nil
}

// subfolder is a folder to walk.
type subfolder struct {
	ref  cs3.Ref
	path string
}

// walk records the files of a folder and descends into its subfolders. The
// paths are built from the names: the paths the platform reports differ
// with the shape of the reference.
func (s *spaceSync) walk(ctx context.Context, ref cs3.Ref, dir string) error {
	folders, err := s.visit(ctx, ref, dir)
	if err != nil {
		return err
	}
	for _, f := range folders {
		if err := s.walk(ctx, f.ref, f.path); err != nil {
			return err
		}
	}
	return nil
}

// visit lists a folder, records its files and returns its subfolders. The
// listing is let go of before they are walked, so that what is held at a
// time is one folder and the way down to it.
func (s *spaceSync) visit(ctx context.Context, ref cs3.Ref, dir string) ([]subfolder, error) {
	var infos []cs3.ResourceInfo
	err := s.retry(ctx, "list "+dir, func() error {
		var err error
		infos, err = s.platform.ListContainer(ctx, ref)
		return err
	})
	if errors.Is(err, cs3.ErrNotFound) {
		// Gone since its parent was listed.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var folders []subfolder
	for _, info := range infos {
		p := path.Join(dir, info.Name)
		if info.IsDir {
			folders = append(folders, subfolder{ref: info.ID, path: p})
			continue
		}
		if err := s.file(ctx, info, p); err != nil {
			return nil, err
		}
	}
	return folders, nil
}

// file brings the record of one file in line: the blob stays known while
// the version does, otherwise the metadata of the platform names it, when
// it is at hand; the entry is written unless the tree says the same already.
func (s *spaceSync) file(ctx context.Context, info cs3.ResourceInfo, p string) error {
	s.counts.files++
	s.progress("walking the platform", s.counts.files)

	entry := entryOf(info, p)
	pathHash := s.index.hash(entry.Path)
	known := s.index.lookup(entry.FileID, pathHash)
	if known != none && s.index.rec(known).etag == s.index.hash(entry.ETag) {
		entry.BlobID = s.index.blobOf(known)
	}
	if entry.BlobID == "" && s.blobs != nil {
		blobID, err := s.blobs.BlobID(ctx, s.spaceID(), entry.FileID)
		switch {
		case errors.Is(err, ErrNoMetadata):
			s.log.Warn("no metadata for the file", slog.String("path", entry.Path))
		case err != nil:
			return err
		case blobID != "":
			entry.BlobID = blobID
			s.counts.fromMetadata++
		}
	}
	if entry.BlobID == "" {
		s.counts.noBlob++
		if s.opts.NoBlob != nil {
			s.opts.NoBlob(s.spaceID() + entry.Path)
		}
	}

	if known != none && s.index.same(known, entry, pathHash) {
		s.index.see(known, pathHash)
		s.counts.kept++
		return nil
	}
	if known != none {
		s.index.see(known, pathHash)
	}
	s.counts.rewritten++
	if s.opts.DryRun {
		return nil
	}
	return s.retry(ctx, "put "+entry.Path, func() error { return s.tree.PutFile(ctx, entry) })
}

// sweep walks the keys of the tree and takes out what the platform no
// longer holds at that path: the old path of a moved file, or a file that is
// gone, whose record moves into the trash bin of the tree when the platform
// has it in its own.
func (s *spaceSync) sweep(ctx context.Context) error {
	var items []cs3.RecycleItem
	err := s.retry(ctx, "list recycle", func() error {
		var err error
		items, err = s.platform.ListRecycle(ctx, s.space.Root)
		return err
	})
	if err != nil {
		return err
	}
	trashed := make(map[[16]byte]bool, len(items))
	for _, item := range items {
		trashed[packID(item.Key)] = true
	}

	n := 0
	return s.tree.Paths(ctx, s.spaceID(), "/", func(p string) error {
		n++
		s.progress("sweeping the tree", n)
		i := s.index.atPath(p)
		if i == none {
			// Written during the walk, or by the service meanwhile.
			return nil
		}
		r := s.index.rec(i)
		switch {
		case r.flags&flagSeen != 0 && r.path == s.index.hash(p):
			return nil
		case r.flags&flagSeen != 0, s.index.seenElsewhere(i):
			// The old path of a move, or what an interrupted one left behind.
			return s.remove(ctx, p)
		}
		s.counts.stale++
		if trashed[r.id] {
			return s.trash(ctx, p)
		}
		return s.remove(ctx, p)
	})
}

// remove forgets the file at a path.
func (s *spaceSync) remove(ctx context.Context, p string) error {
	if s.opts.DryRun {
		return nil
	}
	return s.retry(ctx, "delete "+p, func() error { return s.tree.DeleteFile(ctx, s.spaceID(), p) })
}

// trash moves the record of the file at a path into the trash bin of the
// tree.
func (s *spaceSync) trash(ctx context.Context, p string) error {
	if s.opts.DryRun {
		return nil
	}
	var old *tree.Entry
	err := s.retry(ctx, "read "+p, func() error {
		var err error
		old, err = s.tree.File(ctx, s.spaceID(), p)
		return err
	})
	if errors.Is(err, tree.ErrNotFound) {
		// Removed meanwhile.
		return nil
	}
	if err != nil {
		return err
	}
	return s.retry(ctx, "trash "+p, func() error { return s.tree.ApplyTrash(ctx, nil, []tree.Entry{*old}) })
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
