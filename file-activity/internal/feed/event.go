// Package feed is the stream of changes the service publishes: the schema of
// an event, the JetStream stream it lives in, and how it is read back by
// sequence number.
package feed

import "time"

// Type is what happened to the resource.
type Type string

// Types of the feed. A new version of a file and a restored older version are
// both file_updated.
const (
	FileCreated   Type = "file_created"
	FileUpdated   Type = "file_updated"
	FolderCreated Type = "folder_created"
	Trashed       Type = "trashed"
	Restored      Type = "restored"
	Purged        Type = "purged"
	Moved         Type = "moved"
	SpaceCreated  Type = "space_created"
	SpaceDeleted  Type = "space_deleted"
	SpaceRenamed  Type = "space_renamed"
)

// Actor is who caused the event. Name is filled when the platform sends it.
type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Event is one entry of the feed.
//
// Seq is the sequence number of the entry in the stream and the cursor a
// client reads with; it is assigned on publish and filled in on read, so the
// stored body does not carry it. Path is null when the resource is gone for
// good and its last path is unknown, OldPath is set for a move or a rename.
// BlobID names the blob of the file in the bucket of the platform; a folder
// operation yields one entry per file it touched, each carrying its blob.
type Event struct {
	Seq        uint64     `json:"seq"`
	ID         string     `json:"id"`
	TS         time.Time  `json:"ts"`
	Type       Type       `json:"type"`
	SpaceID    string     `json:"space_id"`
	SpaceName  string     `json:"space_name,omitempty"`
	FileID     string     `json:"file_id,omitempty"`
	ResourceID string     `json:"resource_id,omitempty"`
	Path       *string    `json:"path"`
	OldPath    *string    `json:"old_path"`
	IsDir      bool       `json:"is_dir"`
	Size       uint64     `json:"size,omitempty"`
	Mime       string     `json:"mime,omitempty"`
	ETag       string     `json:"etag,omitempty"`
	BlobID     string     `json:"blob_id,omitempty"`
	Checksum   string     `json:"checksum,omitempty"`
	MTime      *time.Time `json:"mtime,omitempty"`
	Actor      *Actor     `json:"actor,omitempty"`
}

// Head is what a client compares its cursor against.
type Head struct {
	FirstAvailable uint64 `json:"first_available"`
	Last           uint64 `json:"last"`
}

// Page is the answer to a read.
type Page struct {
	Events         []Event `json:"events"`
	Next           uint64  `json:"next"`
	FirstAvailable uint64  `json:"first_available"`
	Last           uint64  `json:"last"`
}
