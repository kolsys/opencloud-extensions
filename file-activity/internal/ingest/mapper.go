// Package ingest turns the events of main-queue into entries of the feed
// and into changes of the tree.
package ingest

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// Reasons an event of main-queue is dropped instead of published.
const (
	ReasonIgnored   = "ignored"
	ReasonUnknown   = "unknown"
	ReasonFailed    = "failed"
	ReasonGone      = "gone"
	ReasonDenied    = "denied"
	ReasonMalformed = "malformed"
)

// SkipError says an event is dropped on purpose and why. It is acknowledged
// and counted, never retried.
type SkipError struct {
	Reason string
}

func (s *SkipError) Error() string {
	return "file-activity: event skipped: " + s.Reason
}

// Resolver looks a reference up. The gateway client implements it.
type Resolver interface {
	Stat(ctx context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error)
}

// Mapped is what one event of the platform becomes: the entries of the feed,
// and the change of the tree, applied once they are published. Publishing
// first keeps a replay honest: the entries are deduplicated by id, the change
// is idempotent.
type Mapped struct {
	Events []feed.Event
	Apply  func(ctx context.Context) error
}

// Tree is the copy of the tree of the platform the mapper keeps and asks:
// tree.Tree over a bucket, or tree.Disabled when none is configured.
type Tree interface {
	PutFile(ctx context.Context, e tree.Entry) error
	PlanMove(ctx context.Context, spaceID, oldPath, newPath string, isDir bool) ([]tree.Renamed, error)
	ApplyMove(ctx context.Context, renamed []tree.Renamed) error
	PlanTrash(ctx context.Context, spaceID, p string, isDir bool) ([]tree.Entry, error)
	ApplyTrash(ctx context.Context, folder *tree.Entry, files []tree.Entry) error
	PlanRestore(ctx context.Context, spaceID, fileID, newPath string) (*tree.Entry, []tree.Entry, error)
	ApplyRestore(ctx context.Context, item *tree.Entry, files []tree.Entry) error
	PlanPurge(ctx context.Context, spaceID, fileID string) (*tree.Entry, []tree.Entry, error)
	ApplyPurge(ctx context.Context, item *tree.Entry, files []tree.Entry) error
	PlanEmptyTrash(ctx context.Context, spaceID string) ([]tree.Entry, error)
	ApplyEmptyTrash(ctx context.Context, spaceID string) error
	PutSpace(ctx context.Context, s tree.Space) error
	Space(ctx context.Context, spaceID string) (*tree.Space, error)
	PlanDeleteSpace(ctx context.Context, spaceID string) ([]tree.Entry, error)
	ApplyDeleteSpace(ctx context.Context, spaceID string) error
}

// Mapper resolves the events of the platform into entries of the feed. The
// events carry no file id and no etag, so almost every one costs a Stat; what
// a Stat cannot tell, the blob of a file and the files a folder holds, comes
// from the tree.
type Mapper struct {
	cs3  Resolver
	tree Tree
	log  *slog.Logger
}

// NewMapper returns a mapper resolving references through the gateway and
// keeping the tree.
func NewMapper(resolver Resolver, t Tree, log *slog.Logger) *Mapper {
	return &Mapper{cs3: resolver, tree: t, log: log}
}

// Map returns the entries for an event, a Skip when the event is not one the
// feed carries, or another error when a lookup failed and a retry may help.
func (m *Mapper) Map(ctx context.Context, event ocevents.Event) (*Mapped, error) {
	if !event.Known() {
		return nil, &SkipError{Reason: ReasonUnknown}
	}

	switch payload := event.Payload.(type) {
	case *ocevents.UploadReady:
		return m.uploadReady(ctx, event, payload)
	case *ocevents.ContainerCreated:
		return m.containerCreated(ctx, event, payload)
	case *ocevents.ItemTrashed:
		return m.itemTrashed(ctx, event, payload)
	case *ocevents.ItemPurged:
		return m.itemPurged(ctx, event, payload)
	case *ocevents.ItemRestored:
		return m.itemRestored(ctx, event, payload)
	case *ocevents.ItemMoved:
		return m.itemMoved(ctx, event, payload)
	case *ocevents.FileVersionRestored:
		return m.fileVersionRestored(ctx, event, payload)
	case *ocevents.TrashbinPurged:
		return m.trashbinPurged(ctx, event, payload)
	case *ocevents.SpaceCreated:
		return m.spaceCreated(event, payload)
	case *ocevents.SpaceRenamed:
		return m.spaceRenamed(event, payload)
	case *ocevents.SpaceDeleted:
		return m.spaceDeleted(ctx, event, payload)
	default:
		return nil, &SkipError{Reason: ReasonIgnored}
	}
}

// uploadReady is the one event that names the blob: its upload id is the
// blob id the platform stores the bytes under.
func (m *Mapper) uploadReady(ctx context.Context, event ocevents.Event, upload *ocevents.UploadReady) (*Mapped, error) {
	if upload.Failed {
		return nil, &SkipError{Reason: ReasonFailed}
	}

	entry := base(event, upload.Timestamp, userActor(upload.ExecutingUser))
	entry.Type = feed.FileCreated
	if upload.IsVersion {
		entry.Type = feed.FileUpdated
	}
	info, err := m.resolve(ctx, refOf(upload.FileRef), entry)
	if err != nil {
		return nil, err
	}
	entry.BlobID = upload.UploadID

	mapped := &Mapped{Events: []feed.Event{*entry}}
	if !info.IsDir {
		file := entryOf(info, *entry.Path)
		file.BlobID = upload.UploadID
		mapped.Apply = func(ctx context.Context) error { return m.tree.PutFile(ctx, file) }
	}
	return mapped, nil
}

func (m *Mapper) containerCreated(ctx context.Context, event ocevents.Event, created *ocevents.ContainerCreated) (*Mapped, error) {
	entry := base(event, created.Timestamp, idActor(created.Executant))
	entry.Type = feed.FolderCreated
	if _, err := m.resolve(ctx, refOf(created.Ref), entry); err != nil {
		return nil, err
	}
	return &Mapped{Events: []feed.Event{*entry}}, nil
}

// itemTrashed cannot Stat: the item is in the trash bin by now. The ids come
// from ID and the last path from Ref; whether it was a file or a folder, and
// which files a folder took along, the tree tells.
func (m *Mapper) itemTrashed(ctx context.Context, event ocevents.Event, trashed *ocevents.ItemTrashed) (*Mapped, error) {
	entry := base(event, trashed.Timestamp, idActor(trashed.Executant))
	entry.Type = feed.Trashed
	setIDs(entry, trashed.ID)
	entry.Path = pathOf(trashed.Ref)
	if entry.Path == nil || entry.SpaceID == "" {
		return &Mapped{Events: []feed.Event{*entry}}, nil
	}

	files, err := m.tree.PlanTrash(ctx, entry.SpaceID, *entry.Path, false)
	if err != nil {
		return nil, err
	}
	if len(files) == 1 && files[0].FileID == entry.FileID {
		fill(entry, files[0])
		return &Mapped{
			Events: []feed.Event{*entry},
			Apply:  func(ctx context.Context) error { return m.tree.ApplyTrash(ctx, nil, files) },
		}, nil
	}

	// Not a file the tree knows at this path: a folder, whose files are the
	// ones under the path.
	files, err = m.tree.PlanTrash(ctx, entry.SpaceID, *entry.Path, true)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return &Mapped{Events: []feed.Event{*entry}}, nil
	}
	entry.IsDir = true
	folder := &tree.Entry{SpaceID: entry.SpaceID, FileID: entry.FileID, Path: *entry.Path, IsDir: true, MTime: entry.TS}
	return &Mapped{
		Events: expand(*entry, files, nil),
		Apply:  func(ctx context.Context) error { return m.tree.ApplyTrash(ctx, folder, files) },
	}, nil
}

// itemPurged leaves ID empty on the wire; the ids of the purged item travel
// in Ref. The path is gone with the item, the tree still knows it.
func (m *Mapper) itemPurged(ctx context.Context, event ocevents.Event, purged *ocevents.ItemPurged) (*Mapped, error) {
	entry := base(event, purged.Timestamp, idActor(purged.Executant))
	entry.Type = feed.Purged
	if purged.Ref != nil {
		setIDs(entry, purged.Ref.ResourceID)
	}
	if entry.FileID == "" {
		setIDs(entry, purged.ID)
	}
	if entry.FileID == "" {
		return &Mapped{Events: []feed.Event{*entry}}, nil
	}

	item, files, err := m.tree.PlanPurge(ctx, entry.SpaceID, entry.FileID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return &Mapped{Events: []feed.Event{*entry}}, nil
	}

	apply := func(ctx context.Context) error { return m.tree.ApplyPurge(ctx, item, files) }
	if !item.IsDir {
		fill(entry, *item)
		return &Mapped{Events: []feed.Event{*entry}, Apply: apply}, nil
	}
	entry.IsDir = true
	entry.Path = new(item.Path)
	return &Mapped{Events: expand(*entry, files, nil), Apply: apply}, nil
}

// itemRestored carries the new location in Ref and the id of the item in
// Key; ID is empty on the wire. The files a folder brings back come from the
// tree.
func (m *Mapper) itemRestored(ctx context.Context, event ocevents.Event, restored *ocevents.ItemRestored) (*Mapped, error) {
	entry := base(event, restored.Timestamp, idActor(restored.Executant))
	entry.Type = feed.Restored
	info, err := m.resolve(ctx, refOf(restored.Ref), entry)
	if err != nil {
		return nil, err
	}
	if entry.FileID == "" {
		entry.FileID = restored.Key
	}

	item, files, err := m.tree.PlanRestore(ctx, entry.SpaceID, entry.FileID, *entry.Path)
	if err != nil {
		return nil, err
	}
	switch {
	case item == nil && info.IsDir:
		// Trashed before the tree was kept: the folder comes back with nothing
		// known about its files.
		return &Mapped{Events: []feed.Event{*entry}}, nil
	case item == nil:
		file := entryOf(info, *entry.Path)
		return &Mapped{
			Events: []feed.Event{*entry},
			Apply:  func(ctx context.Context) error { return m.tree.PutFile(ctx, file) },
		}, nil
	}

	apply := func(ctx context.Context) error { return m.tree.ApplyRestore(ctx, item, files) }
	if !item.IsDir {
		// The platform refreshes the attributes on restore; the blob stays.
		file := entryOf(info, *entry.Path)
		file.BlobID = files[0].BlobID
		fill(entry, file)
		files[0] = file
		return &Mapped{Events: []feed.Event{*entry}, Apply: apply}, nil
	}
	return &Mapped{Events: expand(*entry, files, nil), Apply: apply}, nil
}

// itemMoved carries both locations. The files a folder takes along come
// from the tree.
func (m *Mapper) itemMoved(ctx context.Context, event ocevents.Event, moved *ocevents.ItemMoved) (*Mapped, error) {
	entry := base(event, moved.Timestamp, idActor(moved.Executant))
	entry.Type = feed.Moved
	info, err := m.resolve(ctx, refOf(moved.Ref), entry)
	if err != nil {
		return nil, err
	}
	entry.OldPath = pathOf(moved.OldReference)
	if entry.OldPath == nil {
		return &Mapped{Events: []feed.Event{*entry}}, nil
	}

	renamed, err := m.tree.PlanMove(ctx, entry.SpaceID, *entry.OldPath, *entry.Path, info.IsDir)
	if err != nil {
		return nil, err
	}
	switch {
	case len(renamed) == 0 && info.IsDir:
		return &Mapped{Events: []feed.Event{*entry}}, nil
	case len(renamed) == 0:
		// Never seen at the old path: recorded at the new one, blob unknown.
		file := entryOf(info, *entry.Path)
		return &Mapped{
			Events: []feed.Event{*entry},
			Apply:  func(ctx context.Context) error { return m.tree.PutFile(ctx, file) },
		}, nil
	}

	apply := func(ctx context.Context) error { return m.tree.ApplyMove(ctx, renamed) }
	if !info.IsDir {
		fill(entry, renamed[0].New)
		return &Mapped{Events: []feed.Event{*entry}, Apply: apply}, nil
	}

	files := make([]tree.Entry, 0, len(renamed))
	olds := make([]string, 0, len(renamed))
	for _, r := range renamed {
		files = append(files, r.New)
		olds = append(olds, r.Old.Path)
	}
	return &Mapped{Events: expand(*entry, files, olds), Apply: apply}, nil
}

// fileVersionRestored points at the file by id only; the path comes from the
// Stat. The blob is the one of an older version, which the tree does not
// know: it is marked unknown.
func (m *Mapper) fileVersionRestored(ctx context.Context, event ocevents.Event, restored *ocevents.FileVersionRestored) (*Mapped, error) {
	entry := base(event, restored.Timestamp, idActor(restored.Executant))
	entry.Type = feed.FileUpdated
	info, err := m.resolve(ctx, refOf(restored.Ref), entry)
	if err != nil {
		return nil, err
	}
	file := entryOf(info, *entry.Path)
	m.log.Warn("version restored, blob unknown to the tree", slog.String("path", file.Path), slog.String("file", file.FileID))
	return &Mapped{
		Events: []feed.Event{*entry},
		Apply:  func(ctx context.Context) error { return m.tree.PutFile(ctx, file) },
	}, nil
}

// trashbinPurged names no item: every file the tree holds in the trash bin
// of the space is gone for good.
func (m *Mapper) trashbinPurged(ctx context.Context, event ocevents.Event, purged *ocevents.TrashbinPurged) (*Mapped, error) {
	entry := base(event, purged.Timestamp, idActor(purged.Executant))
	entry.Type = feed.Purged
	if purged.Ref != nil && purged.Ref.ResourceID != nil {
		entry.SpaceID = purged.Ref.ResourceID.SpaceID
	}
	if entry.SpaceID == "" {
		return nil, &SkipError{Reason: ReasonMalformed}
	}

	files, err := m.tree.PlanEmptyTrash(ctx, entry.SpaceID)
	if err != nil {
		return nil, err
	}
	return &Mapped{
		Events: expand(*entry, files, nil)[1:],
		Apply:  func(ctx context.Context) error { return m.tree.ApplyEmptyTrash(ctx, entry.SpaceID) },
	}, nil
}

func (m *Mapper) spaceCreated(event ocevents.Event, created *ocevents.SpaceCreated) (*Mapped, error) {
	entry := base(event, created.MTime, idActor(created.Executant))
	entry.Type = feed.SpaceCreated
	entry.SpaceName = created.Name
	if created.Root != nil {
		entry.SpaceID = created.Root.SpaceID
	}
	if entry.SpaceID == "" {
		entry.SpaceID = spaceIDOf(created.ID)
	}
	entry.IsDir = true

	space := tree.Space{ID: entry.SpaceID, Name: created.Name, Type: created.Type}
	if created.Owner != nil {
		space.Owner = created.Owner.OpaqueID
	}
	return &Mapped{
		Events: []feed.Event{*entry},
		Apply:  func(ctx context.Context) error { return m.tree.PutSpace(ctx, space) },
	}, nil
}

func (m *Mapper) spaceRenamed(event ocevents.Event, renamed *ocevents.SpaceRenamed) (*Mapped, error) {
	entry := base(event, renamed.Timestamp, idActor(renamed.Executant))
	entry.Type = feed.SpaceRenamed
	entry.SpaceName = renamed.Name
	entry.SpaceID = spaceIDOf(renamed.ID)
	entry.IsDir = true

	return &Mapped{
		Events: []feed.Event{*entry},
		Apply: func(ctx context.Context) error {
			space, err := m.tree.Space(ctx, entry.SpaceID)
			if err != nil && !stderrors.Is(err, tree.ErrNotFound) {
				return err
			}
			if space == nil {
				space = &tree.Space{ID: entry.SpaceID}
			}
			space.Name = renamed.Name
			return m.tree.PutSpace(ctx, *space)
		},
	}, nil
}

// spaceDeleted carries a plain time instead of the CS3 timestamp. Every file
// of the space goes with it.
func (m *Mapper) spaceDeleted(ctx context.Context, event ocevents.Event, deleted *ocevents.SpaceDeleted) (*Mapped, error) {
	entry := base(event, nil, idActor(deleted.Executant))
	if !deleted.Timestamp.IsZero() {
		entry.TS = deleted.Timestamp.UTC()
	}
	entry.Type = feed.SpaceDeleted
	entry.SpaceName = deleted.SpaceName
	entry.SpaceID = spaceIDOf(deleted.ID)
	entry.IsDir = true

	files, err := m.tree.PlanDeleteSpace(ctx, entry.SpaceID)
	if err != nil {
		return nil, err
	}
	purged := *entry
	purged.Type = feed.Purged
	return &Mapped{
		Events: append([]feed.Event{*entry}, expand(purged, files, nil)[1:]...),
		Apply:  func(ctx context.Context) error { return m.tree.ApplyDeleteSpace(ctx, entry.SpaceID) },
	}, nil
}

// expand returns the entry of a folder operation followed by one entry per
// file it touched, each named after the event and the file. The old paths,
// when given, belong to a move.
func expand(folder feed.Event, files []tree.Entry, oldPaths []string) []feed.Event {
	events := make([]feed.Event, 0, len(files)+1)
	events = append(events, folder)
	for i, file := range files {
		e := folder
		e.ID = folder.ID + "/" + file.FileID
		e.SpaceName = ""
		e.OldPath = nil
		e.IsDir = false
		fill(&e, file)
		if oldPaths != nil {
			e.OldPath = new(oldPaths[i])
		}
		events = append(events, e)
	}
	return events
}

// fill copies what the tree knows about a file into an entry.
func fill(entry *feed.Event, file tree.Entry) {
	entry.SpaceID = file.SpaceID
	entry.FileID = file.FileID
	entry.ResourceID = resourceID(entry.ResourceID, file)
	entry.Path = new(file.Path)
	entry.IsDir = false
	entry.Size = file.Size
	entry.Mime = file.Mime
	entry.ETag = file.ETag
	entry.BlobID = file.BlobID
	entry.Checksum = checksumOf(file.SHA1)
	if !file.MTime.IsZero() {
		mtime := file.MTime
		entry.MTime = &mtime
	}
}

// resourceID keeps the composite id of the entry when it is about the same
// file, and rebuilds it from the storage of the known one otherwise.
func resourceID(known string, file tree.Entry) string {
	if strings.HasSuffix(known, "!"+file.FileID) {
		return known
	}
	storage, _, _ := strings.Cut(known, "$")
	if storage == "" || strings.Contains(storage, "!") {
		return file.SpaceID + "!" + file.FileID
	}
	return storage + "$" + file.SpaceID + "!" + file.FileID
}

// entryOf is the tree entry of a resource the platform described, at the
// path the event named. The blob is filled by the caller when it is known.
func entryOf(info *cs3.ResourceInfo, p string) tree.Entry {
	return tree.Entry{
		SpaceID: info.ID.SpaceID,
		FileID:  info.ID.OpaqueID,
		Path:    normalizePath(p),
		Size:    info.Size,
		Mime:    info.MimeType,
		ETag:    strings.Trim(info.ETag, `"`),
		MTime:   info.MTime,
		SHA1:    info.SHA1,
		MD5:     info.MD5,
	}
}

func checksumOf(sha1 string) string {
	if sha1 == "" {
		return ""
	}
	return "sha1:" + sha1
}

// resolve fills the ids, the path and the attributes of the resource a
// reference points at and returns what the platform said. A resource that
// is gone by now makes the event a skip.
//
// The path is taken from the reference when it is relative to the root of
// the space, which is how the events carry it: a Stat by such a reference
// reports only the name of the resource, while a Stat by id reports the full
// path. A reference without a path or relative to another resource costs
// that second Stat.
func (m *Mapper) resolve(ctx context.Context, ref cs3.Ref, entry *feed.Event) (*cs3.ResourceInfo, error) {
	info, err := m.stat(ctx, ref)
	if err != nil {
		return nil, err
	}

	switch {
	case ref.Path != "" && isSpaceRoot(ref):
		entry.Path = new(normalizePath(ref.Path))
	case strings.HasPrefix(info.Path, "/"):
		entry.Path = new(normalizePath(info.Path))
	default:
		byID, err := m.stat(ctx, info.ID)
		if err != nil {
			return nil, err
		}
		entry.Path = new(normalizePath(byID.Path))
	}

	entry.SpaceID = info.ID.SpaceID
	entry.FileID = info.ID.OpaqueID
	entry.ResourceID = info.ID.String()
	entry.IsDir = info.IsDir
	entry.Size = info.Size
	entry.Mime = info.MimeType
	entry.ETag = strings.Trim(info.ETag, `"`)
	entry.Checksum = checksumOf(info.SHA1)
	if !info.MTime.IsZero() {
		mtime := info.MTime
		entry.MTime = &mtime
	}
	return info, nil
}

func (m *Mapper) stat(ctx context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error) {
	info, err := m.cs3.Stat(ctx, ref)
	switch {
	case stderrors.Is(err, cs3.ErrNotFound):
		return nil, &SkipError{Reason: ReasonGone}
	case stderrors.Is(err, cs3.ErrPermissionDenied):
		return nil, &SkipError{Reason: ReasonDenied}
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errors.ErrGone, err)
	}
	return info, nil
}

// isSpaceRoot reports whether a reference is anchored at the root of its
// space, where the platform anchors the paths of its events.
func isSpaceRoot(ref cs3.Ref) bool {
	return ref.OpaqueID == "" || ref.OpaqueID == ref.SpaceID
}

// base starts an entry from the envelope: id, timestamp and actor.
func base(event ocevents.Event, stamp *ocevents.Timestamp, actor *feed.Actor) *feed.Event {
	entry := &feed.Event{ID: event.ID, TS: event.Queued.UTC(), Actor: actor}
	if when := stamp.Time(); !when.IsZero() {
		entry.TS = when
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	return entry
}

func idActor(id *ocevents.UserID) *feed.Actor {
	if id == nil || id.OpaqueID == "" {
		return nil
	}
	return &feed.Actor{ID: id.OpaqueID}
}

func userActor(user *ocevents.User) *feed.Actor {
	if user == nil || user.ID == nil || user.ID.OpaqueID == "" {
		return nil
	}
	return &feed.Actor{ID: user.ID.OpaqueID, Name: user.Username}
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

func setIDs(entry *feed.Event, id *ocevents.ResourceID) {
	if id.Empty() {
		return
	}
	entry.SpaceID = id.SpaceID
	entry.FileID = id.OpaqueID
	entry.ResourceID = id.String()
}

// pathOf normalises the path of a reference, nil when it has none.
func pathOf(ref *ocevents.Reference) *string {
	if ref == nil || ref.Path == "" {
		return nil
	}
	return new(normalizePath(ref.Path))
}

// normalizePath brings the shapes the platform uses to one: "./a/b" from an
// upload, "/a/b" from a move, "a/b" from a Stat by path and "." for the root
// all become the path inside the space, rooted at "/".
func normalizePath(p string) string {
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return "/"
	}
	return path.Clean("/" + p)
}

// spaceIDOf takes the space out of the composite id of a storage space,
// storageid$spaceid or storageid$spaceid!opaqueid.
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
