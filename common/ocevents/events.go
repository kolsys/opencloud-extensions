package ocevents

import (
	"encoding/json"
	"time"
)

// The top level fields of an event carry their Go names: the structs of the
// platform have no json tags. Field names and sets are mirrored from
// reva v2 pkg/events, files.go, spaces.go and postprocessing.go.

// ContainerCreated is emitted when a directory has been created.
type ContainerCreated struct {
	SpaceOwner        *UserID
	Executant         *UserID
	Ref               *Reference
	ParentID          *ResourceID
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileUploaded is emitted when an upload is initiated, not when it finishes.
// The signal that a file is readable is UploadReady without Failed.
type FileUploaded struct {
	SpaceOwner        *UserID
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileTouched is emitted when an empty file has been created.
type FileTouched struct {
	SpaceOwner        *UserID
	Executant         *UserID
	Ref               *Reference
	ParentID          *ResourceID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileDownloaded is emitted on every download and is dropped by both services.
type FileDownloaded struct {
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileLocked is emitted when a file has been locked.
type FileLocked struct {
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileUnlocked is emitted when a file has been unlocked.
type FileUnlocked struct {
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// ItemTrashed is emitted when a file or a directory has been moved to the
// trash bin.
type ItemTrashed struct {
	SpaceOwner        *UserID
	Executant         *UserID
	ID                *ResourceID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// ItemMoved is emitted when a file or a directory has been moved or renamed.
// Ref is the new reference, OldReference the previous one.
type ItemMoved struct {
	SpaceOwner        *UserID
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	OldReference      *Reference
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// TrashbinPurged is emitted when a whole trash bin has been emptied. It
// carries no id of the items that were in it.
type TrashbinPurged struct {
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// ItemPurged is emitted when a single item has been deleted for good. The
// resource is gone by then and cannot be looked up any more.
type ItemPurged struct {
	Executant         *UserID
	ID                *ResourceID
	Ref               *Reference
	Owner             *UserID
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// ItemRestored is emitted when an item has been taken out of the trash bin.
type ItemRestored struct {
	SpaceOwner        *UserID
	Executant         *UserID
	ID                *ResourceID
	Ref               *Reference
	Owner             *UserID
	OldReference      *Reference
	Key               string
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// FileVersionRestored is emitted when an older version of a file has been
// made the current one. The etag of the file becomes the old one again.
type FileVersionRestored struct {
	SpaceOwner        *UserID
	Executant         *UserID
	Ref               *Reference
	Owner             *UserID
	Key               string
	Timestamp         *Timestamp
	ImpersonatingUser *User
}

// UploadReady is emitted when postprocessing of an upload has finished and
// the file is readable.
//
// It carries no id of the file: FileRef is the root of the space plus the
// path inside it, and ParentID is the id of the parent directory, nil when
// Failed. The fileid and the etag are obtained with a Stat on FileRef.
type UploadReady struct {
	UploadID          string
	Filename          string
	SpaceOwner        *UserID
	ExecutingUser     *User
	ImpersonatingUser *User
	FileRef           *Reference
	ParentID          *ResourceID
	Timestamp         *Timestamp
	Failed            bool
	IsVersion         bool
}

// SpaceCreated is emitted when a storage space has been created.
type SpaceCreated struct {
	Executant *UserID
	ID        *StorageSpaceID
	Owner     *UserID
	Root      *ResourceID
	Name      string
	Type      string
	Quota     json.RawMessage
	MTime     *Timestamp
}

// SpaceRenamed is emitted when a storage space has been renamed.
type SpaceRenamed struct {
	Executant *UserID
	ID        *StorageSpaceID
	Owner     *UserID
	Name      string
	Timestamp *Timestamp
}

// SpaceDeleted is emitted when a storage space has been deleted. Its
// Timestamp is a plain time, not the CS3 one the other events carry.
type SpaceDeleted struct {
	Executant    *UserID
	ID           *StorageSpaceID
	SpaceName    string
	FinalMembers map[string]json.RawMessage
	Timestamp    time.Time
}
