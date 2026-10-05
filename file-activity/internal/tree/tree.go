// Package tree keeps in a bucket what the platform keeps in its metadata and
// nowhere else: which blob a file is, at which path, in which space. It is
// written from the events of main-queue and read back by restore, when the
// metadata of the platform is gone.
//
//	{prefix}tree/{space_id}{path}       one object per file, the entry as JSON
//	{prefix}trash/{space_id}/{file_id}  the entry while the file is in the trash bin
//	{prefix}spaces/{space_id}           the space
//
// Folders are paths, not objects; a folder exists in the trash bin only,
// where its entry lists the files it took along.
package tree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/kolsys/opencloud-extensions/common/s3store"
)

// Prefixes inside the bucket.
const (
	treeDir   = "tree"
	trashDir  = "trash"
	spacesDir = "spaces"

	contentTypeJSON = "application/json"

	// maxEntry bounds one entry; a folder in the trash bin lists its files.
	maxEntry = 64 << 20
)

// ErrNotFound reports that the tree knows nothing about the file or space.
var ErrNotFound = errors.New("tree: not found")

// Entry is what the tree knows about one file.
type Entry struct {
	SpaceID string `json:"space_id"`
	FileID  string `json:"file_id"`
	// Path is rooted at the space, with a leading slash.
	Path string `json:"path"`
	// IsDir is set on the entry of a folder in the trash bin.
	IsDir bool `json:"is_dir,omitempty"`
	// BlobID names the blob in the bucket of the platform; empty when the
	// upload was not seen, which restore reports.
	BlobID string    `json:"blob_id,omitempty"`
	Size   uint64    `json:"size"`
	Mime   string    `json:"mime,omitempty"`
	ETag   string    `json:"etag,omitempty"`
	MTime  time.Time `json:"mtime"`
	SHA1   string    `json:"sha1,omitempty"`
	MD5    string    `json:"md5,omitempty"`
	// Children are the files a folder took into the trash bin, by id.
	Children []string `json:"children,omitempty"`
}

// BlobKey is the key of the blob of the entry in the bucket of the platform.
func (e Entry) BlobKey() string {
	return BlobKey(e.SpaceID, e.BlobID)
}

// BlobKey lays a blob id out the way the decomposeds3 driver of the platform
// does: under the space, the first four pairs of characters as directories,
// the rest as the name.
//
//	b1f74ec4-…/ac/09/e5/5e/-8c8c-4fa7-9b51-258afaf88d44
func BlobKey(spaceID, blobID string) string {
	const pairs, width = 4, 2
	// The space, the pairs and the rest.
	parts := make([]string, 0, pairs+2) //nolint:mnd // the two ends of the key
	parts = append(parts, spaceID)
	rest := blobID
	for i := 0; i < pairs && len(rest) > width; i++ {
		parts = append(parts, rest[:width])
		rest = rest[width:]
	}
	return strings.Join(append(parts, rest), "/")
}

// Space is what the tree knows about a space.
type Space struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// Renamed is a file before and after a move.
type Renamed struct {
	Old Entry
	New Entry
}

// Objects is the bucket side the tree needs.
type Objects interface {
	Key(parts ...string) string
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string, meta map[string]string) error
	Get(ctx context.Context, key string) (io.ReadCloser, *s3store.Object, error)
	Delete(ctx context.Context, key string) error
	Walk(ctx context.Context, prefix string, fn func(s3store.Object) error) error
}

// Tree is the tree of the platform, kept in a bucket.
type Tree struct {
	objects Objects
}

// New returns the tree kept in the given bucket.
func New(objects Objects) *Tree {
	return &Tree{objects: objects}
}

// PutFile records a file at its path, replacing whatever was there.
func (t *Tree) PutFile(ctx context.Context, e Entry) error {
	e.Path = Clean(e.Path)
	e.IsDir, e.Children = false, nil
	return t.put(ctx, t.treeKey(e.SpaceID, e.Path), e)
}

// File returns the entry at a path, ErrNotFound when there is none.
func (t *Tree) File(ctx context.Context, spaceID, p string) (*Entry, error) {
	var e Entry
	if err := t.get(ctx, t.treeKey(spaceID, Clean(p)), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// DeleteFile forgets a file at a path. Forgetting what is not there is not
// an error.
func (t *Tree) DeleteFile(ctx context.Context, spaceID, p string) error {
	return t.objects.Delete(ctx, t.treeKey(spaceID, Clean(p)))
}

// DeleteTrashed forgets a file or folder in the trash bin.
func (t *Tree) DeleteTrashed(ctx context.Context, spaceID, fileID string) error {
	return t.objects.Delete(ctx, t.trashKey(spaceID, fileID))
}

// SpaceIDs returns the spaces that have files in the tree, recorded as a
// space or not: the personal spaces that predate the service have files
// and no record.
func (t *Tree) SpaceIDs(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	prefix := t.objects.Key(treeDir) + "/"
	err := t.objects.Walk(ctx, prefix, func(object s3store.Object) error {
		id, _, _ := strings.Cut(strings.TrimPrefix(object.Key, prefix), "/")
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// Trashed returns the entry of a file or folder in the trash bin.
func (t *Tree) Trashed(ctx context.Context, spaceID, fileID string) (*Entry, error) {
	var e Entry
	if err := t.get(ctx, t.trashKey(spaceID, fileID), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Walk calls fn for every file under a folder of a space, the root included.
func (t *Tree) Walk(ctx context.Context, spaceID, dir string, fn func(Entry) error) error {
	return t.objects.Walk(ctx, t.treePrefix(spaceID, dir), func(object s3store.Object) error {
		var e Entry
		if err := t.get(ctx, object.Key, &e); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Removed between the listing and the read.
				return nil
			}
			return err
		}
		return fn(e)
	})
}

// Paths calls fn with the path of every file under a folder of a space, the
// root included, without reading the entries: a key is a path.
func (t *Tree) Paths(ctx context.Context, spaceID, dir string, fn func(path string) error) error {
	base := t.objects.Key(treeDir, spaceID)
	return t.objects.Walk(ctx, t.treePrefix(spaceID, dir), func(object s3store.Object) error {
		return fn(strings.TrimPrefix(object.Key, base))
	})
}

// PlanMove returns what a move of a file or a folder changes: every file
// with its old and its new path. Nothing is written.
func (t *Tree) PlanMove(ctx context.Context, spaceID, oldPath, newPath string, isDir bool) ([]Renamed, error) {
	oldPath, newPath = Clean(oldPath), Clean(newPath)
	if !isDir {
		e, err := t.File(ctx, spaceID, oldPath)
		if errors.Is(err, ErrNotFound) {
			// Moved already, or never seen.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []Renamed{{Old: *e, New: relocated(*e, newPath)}}, nil
	}

	var renamed []Renamed
	err := t.Walk(ctx, spaceID, oldPath, func(e Entry) error {
		renamed = append(renamed, Renamed{Old: e, New: relocated(e, newPath+strings.TrimPrefix(e.Path, oldPath))})
		return nil
	})
	return renamed, err
}

// ApplyMove writes a planned move. Running it twice is harmless.
func (t *Tree) ApplyMove(ctx context.Context, renamed []Renamed) error {
	for _, r := range renamed {
		if err := t.PutFile(ctx, r.New); err != nil {
			return err
		}
		if err := t.objects.Delete(ctx, t.treeKey(r.Old.SpaceID, r.Old.Path)); err != nil {
			return err
		}
	}
	return nil
}

// PlanTrash returns the files a trashed file or folder takes into the trash
// bin. Nothing is written.
func (t *Tree) PlanTrash(ctx context.Context, spaceID, p string, isDir bool) ([]Entry, error) {
	p = Clean(p)
	if !isDir {
		e, err := t.File(ctx, spaceID, p)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []Entry{*e}, nil
	}

	var files []Entry
	err := t.Walk(ctx, spaceID, p, func(e Entry) error {
		files = append(files, e)
		return nil
	})
	return files, err
}

// ApplyTrash moves the planned files into the trash bin. A folder gets an
// entry of its own that lists them, merged with what an earlier, interrupted
// run already recorded.
func (t *Tree) ApplyTrash(ctx context.Context, folder *Entry, files []Entry) error {
	if folder != nil {
		known, err := t.Trashed(ctx, folder.SpaceID, folder.FileID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		entry := Entry{SpaceID: folder.SpaceID, FileID: folder.FileID, Path: Clean(folder.Path), IsDir: true, MTime: folder.MTime}
		if known != nil {
			entry.Children = known.Children
		}
		for _, f := range files {
			entry.Children = appendUnique(entry.Children, f.FileID)
		}
		if err := t.put(ctx, t.trashKey(entry.SpaceID, entry.FileID), entry); err != nil {
			return err
		}
	}

	for _, f := range files {
		f.Path = Clean(f.Path)
		if err := t.put(ctx, t.trashKey(f.SpaceID, f.FileID), f); err != nil {
			return err
		}
		if err := t.objects.Delete(ctx, t.treeKey(f.SpaceID, f.Path)); err != nil {
			return err
		}
	}
	return nil
}

// PlanRestore returns the files a restore brings back, at their new paths.
// A folder comes back with the files it took along, under the new path of
// the folder. Nothing is written.
func (t *Tree) PlanRestore(ctx context.Context, spaceID, fileID, newPath string) (*Entry, []Entry, error) {
	newPath = Clean(newPath)
	item, err := t.Trashed(ctx, spaceID, fileID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !item.IsDir {
		return item, []Entry{relocated(*item, newPath)}, nil
	}

	files := make([]Entry, 0, len(item.Children))
	for _, id := range item.Children {
		child, err := t.Trashed(ctx, spaceID, id)
		if errors.Is(err, ErrNotFound) {
			// Restored or purged on its own already.
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files = append(files, relocated(*child, newPath+strings.TrimPrefix(child.Path, item.Path)))
	}
	return item, files, nil
}

// ApplyRestore writes a planned restore: the files back into the tree, the
// entries out of the trash bin, the folder last.
func (t *Tree) ApplyRestore(ctx context.Context, item *Entry, files []Entry) error {
	if item == nil {
		return nil
	}
	for _, f := range files {
		if err := t.PutFile(ctx, f); err != nil {
			return err
		}
		if err := t.objects.Delete(ctx, t.trashKey(f.SpaceID, f.FileID)); err != nil {
			return err
		}
	}
	if item.IsDir {
		return t.objects.Delete(ctx, t.trashKey(item.SpaceID, item.FileID))
	}
	return nil
}

// PlanPurge returns the files a purge from the trash bin removes for good:
// the file itself, or the files a folder took along. Nothing is written.
func (t *Tree) PlanPurge(ctx context.Context, spaceID, fileID string) (*Entry, []Entry, error) {
	item, err := t.Trashed(ctx, spaceID, fileID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !item.IsDir {
		return item, []Entry{*item}, nil
	}

	files := make([]Entry, 0, len(item.Children))
	for _, id := range item.Children {
		child, err := t.Trashed(ctx, spaceID, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files = append(files, *child)
	}
	return item, files, nil
}

// ApplyPurge removes the planned entries from the trash bin, the folder last.
func (t *Tree) ApplyPurge(ctx context.Context, item *Entry, files []Entry) error {
	if item == nil {
		return nil
	}
	for _, f := range files {
		if err := t.objects.Delete(ctx, t.trashKey(f.SpaceID, f.FileID)); err != nil {
			return err
		}
	}
	if item.IsDir {
		return t.objects.Delete(ctx, t.trashKey(item.SpaceID, item.FileID))
	}
	return nil
}

// PlanEmptyTrash returns every file in the trash bin of a space. Nothing is
// written.
func (t *Tree) PlanEmptyTrash(ctx context.Context, spaceID string) ([]Entry, error) {
	var files []Entry
	err := t.objects.Walk(ctx, t.objects.Key(trashDir, spaceID)+"/", func(object s3store.Object) error {
		var e Entry
		if err := t.get(ctx, object.Key, &e); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if !e.IsDir {
			files = append(files, e)
		}
		return nil
	})
	return files, err
}

// ApplyEmptyTrash drops the trash bin of a space, folders included.
func (t *Tree) ApplyEmptyTrash(ctx context.Context, spaceID string) error {
	return t.deletePrefix(ctx, t.objects.Key(trashDir, spaceID)+"/")
}

// PutSpace records a space.
func (t *Tree) PutSpace(ctx context.Context, s Space) error {
	return t.put(ctx, t.objects.Key(spacesDir, s.ID), s)
}

// Space returns a recorded space, ErrNotFound when there is none.
func (t *Tree) Space(ctx context.Context, spaceID string) (*Space, error) {
	var s Space
	if err := t.get(ctx, t.objects.Key(spacesDir, spaceID), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// WalkSpaces calls fn for every recorded space.
func (t *Tree) WalkSpaces(ctx context.Context, fn func(Space) error) error {
	return t.objects.Walk(ctx, t.objects.Key(spacesDir)+"/", func(object s3store.Object) error {
		var s Space
		if err := t.get(ctx, object.Key, &s); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		return fn(s)
	})
}

// PlanDeleteSpace returns every file of a space, tree and trash bin.
// Nothing is written.
func (t *Tree) PlanDeleteSpace(ctx context.Context, spaceID string) ([]Entry, error) {
	var files []Entry
	if err := t.Walk(ctx, spaceID, "/", func(e Entry) error {
		files = append(files, e)
		return nil
	}); err != nil {
		return nil, err
	}
	trashed, err := t.PlanEmptyTrash(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return append(files, trashed...), nil
}

// ApplyDeleteSpace forgets a space: its tree, its trash bin and itself.
func (t *Tree) ApplyDeleteSpace(ctx context.Context, spaceID string) error {
	if err := t.deletePrefix(ctx, t.treePrefix(spaceID, "/")); err != nil {
		return err
	}
	if err := t.ApplyEmptyTrash(ctx, spaceID); err != nil {
		return err
	}
	return t.objects.Delete(ctx, t.objects.Key(spacesDir, spaceID))
}

func (t *Tree) treeKey(spaceID, p string) string {
	return t.objects.Key(treeDir, spaceID) + Clean(p)
}

// treePrefix is the key prefix of the files under a folder: the root of the
// space is every file, a folder is what its path followed by a slash covers.
func (t *Tree) treePrefix(spaceID, dir string) string {
	dir = Clean(dir)
	if dir == "/" {
		return t.objects.Key(treeDir, spaceID) + "/"
	}
	return t.objects.Key(treeDir, spaceID) + dir + "/"
}

func (t *Tree) trashKey(spaceID, fileID string) string {
	return t.objects.Key(trashDir, spaceID, fileID)
}

func (t *Tree) put(ctx context.Context, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("tree: encode %s: %w", key, err)
	}
	return t.objects.Put(ctx, key, bytes.NewReader(body), int64(len(body)), contentTypeJSON, nil)
}

func (t *Tree) get(ctx context.Context, key string, value any) error {
	body, _, err := t.objects.Get(ctx, key)
	if errors.Is(err, s3store.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return err
	}
	defer body.Close()

	if err := json.NewDecoder(io.LimitReader(body, maxEntry)).Decode(value); err != nil {
		return fmt.Errorf("tree: decode %s: %w", key, err)
	}
	return nil
}

func (t *Tree) deletePrefix(ctx context.Context, prefix string) error {
	var keys []string
	if err := t.objects.Walk(ctx, prefix, func(object s3store.Object) error {
		keys = append(keys, object.Key)
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := t.objects.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// Clean brings a path to the shape the keys use: rooted at the space with a
// leading slash, no trailing slash, no dot segments.
func Clean(p string) string {
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return "/"
	}
	return path.Clean("/" + p)
}

// relocated is the entry at another path.
func relocated(e Entry, newPath string) Entry {
	e.Path = Clean(newPath)
	e.IsDir, e.Children = false, nil
	return e
}

func appendUnique(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}
