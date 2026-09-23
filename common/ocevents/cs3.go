package ocevents

import (
	"math"
	"time"
)

// The CS3 types nested in the events are protobuf messages: they serialise
// under the snake_case names of their protobuf json tags, unlike the top
// level fields of an event, which carry their Go names.

// ResourceID identifies a resource inside a storage provider.
type ResourceID struct {
	StorageID string `json:"storage_id,omitempty"`
	OpaqueID  string `json:"opaque_id,omitempty"`
	SpaceID   string `json:"space_id,omitempty"`
}

// Empty reports whether the id points at nothing.
func (r *ResourceID) Empty() bool {
	return r == nil || (r.StorageID == "" && r.OpaqueID == "" && r.SpaceID == "")
}

// String renders the id the way the platform writes it in URLs and APIs.
func (r *ResourceID) String() string {
	if r == nil {
		return ""
	}
	return r.StorageID + "$" + r.SpaceID + "!" + r.OpaqueID
}

// Reference points at a resource, by id, by path, or by both. An event of an
// upload carries the root of the space plus the path inside it.
type Reference struct {
	ResourceID *ResourceID `json:"resource_id,omitempty"`
	Path       string      `json:"path,omitempty"`
}

// SpaceID returns the id of the space the reference lives in.
func (r *Reference) SpaceID() string {
	if r == nil || r.ResourceID == nil {
		return ""
	}
	return r.ResourceID.SpaceID
}

// StorageSpaceID identifies a storage space.
type StorageSpaceID struct {
	OpaqueID string `json:"opaque_id,omitempty"`
}

// UserID identifies a user at an identity provider.
type UserID struct {
	Idp      string `json:"idp,omitempty"`
	OpaqueID string `json:"opaque_id,omitempty"`
	Type     int    `json:"type,omitempty"`
}

// User is a user of the platform as the events carry it.
type User struct {
	ID          *UserID `json:"id,omitempty"`
	Username    string  `json:"username,omitempty"`
	Mail        string  `json:"mail,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
}

// Timestamp is the protobuf timestamp of the CS3 API.
type Timestamp struct {
	Seconds uint64 `json:"seconds,omitempty"`
	Nanos   uint32 `json:"nanos,omitempty"`
}

// Time converts the timestamp to UTC. A missing or unrepresentable timestamp
// is the zero time.
func (t *Timestamp) Time() time.Time {
	if t == nil || t.Seconds > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(int64(t.Seconds), int64(t.Nanos)).UTC()
}
