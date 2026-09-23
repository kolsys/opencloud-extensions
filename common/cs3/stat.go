package cs3

import (
	"context"
	"math"
	"strings"
	"time"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	types "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
)

// Ref points at a resource: by id, by path inside a space, or by both. The
// events of the platform deliver the second shape, with the root of the space
// in the ids and the path relative to it.
type Ref struct {
	StorageID string
	SpaceID   string
	OpaqueID  string
	Path      string
}

// ResourceID returns the reference without its path.
func (r Ref) ResourceID() Ref {
	r.Path = ""
	return r
}

// String renders the reference the way the platform writes it.
func (r Ref) String() string {
	id := r.StorageID + "$" + r.SpaceID + "!" + r.OpaqueID
	if r.Path == "" {
		return id
	}
	return id + " " + r.Path
}

func (r Ref) proto() *provider.Reference {
	return &provider.Reference{
		ResourceId: &provider.ResourceId{
			StorageId: r.StorageID,
			SpaceId:   r.SpaceID,
			OpaqueId:  r.OpaqueID,
		},
		Path: r.Path,
	}
}

// ResourceInfo is the answer of a Stat, reduced to what the extensions use.
type ResourceInfo struct {
	// ID identifies the resource itself. Its OpaqueID is the fileid the
	// thumbnails are keyed by.
	ID Ref
	// Path is the canonical path the platform reports for the resource.
	Path string
	// Name is the last element of the path.
	Name string
	// IsDir tells a container from a file.
	IsDir bool
	Size  uint64
	// MimeType decides whether the file is a video.
	MimeType string
	// ETag changes with every new version of the file and is part of the key
	// a thumbnail is stored under.
	ETag  string
	MTime time.Time
	// ParentID is the container the resource sits in.
	ParentID Ref
	// SHA1 and MD5 are the checksums the platform computed at upload, hex
	// encoded, empty for a container.
	SHA1 string
	MD5  string
}

// PathRef returns a reference to the resource by the root of its space and
// its path inside it, the shape a download has to use: a download by id alone
// is routed to the simple data endpoint of the platform, which answers 500.
//
// The path is the full one only after a Stat by id; a Stat by path reports
// the name of the resource alone.
func (r *ResourceInfo) PathRef() Ref {
	return Ref{
		StorageID: r.ID.StorageID,
		SpaceID:   r.ID.SpaceID,
		OpaqueID:  r.ID.SpaceID,
		Path:      "." + strings.TrimPrefix(r.Path, "."),
	}
}

// Stat resolves a reference. A resource that is gone answers ErrNotFound,
// which handlers treat as a skip rather than a failure.
func (c *Client) Stat(ctx context.Context, ref Ref) (*ResourceInfo, error) {
	request := &provider.StatRequest{Ref: ref.proto()}

	authCtx, _, err := c.withToken(ctx, false)
	if err != nil {
		return nil, err
	}

	res, err := c.gateway.Stat(authCtx, request)
	if err != nil {
		return nil, wrap("stat", ref, err)
	}
	if unauthenticated(res.GetStatus()) {
		if authCtx, _, err = c.withToken(ctx, true); err != nil {
			return nil, err
		}
		if res, err = c.gateway.Stat(authCtx, request); err != nil {
			return nil, wrap("stat", ref, err)
		}
	}
	if err := statusError("stat "+ref.String(), res.GetStatus()); err != nil {
		return nil, err
	}

	return resourceInfo(res.GetInfo()), nil
}

// protoTime converts a CS3 timestamp to UTC, the zero time when it is
// missing or out of range.
func protoTime(stamp *types.Timestamp) time.Time {
	if stamp == nil {
		return time.Time{}
	}
	seconds := stamp.GetSeconds()
	if seconds > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), int64(stamp.GetNanos())).UTC()
}

func resourceInfo(info *provider.ResourceInfo) *ResourceInfo {
	if info == nil {
		return nil
	}

	return &ResourceInfo{
		ID: Ref{
			StorageID: info.GetId().GetStorageId(),
			SpaceID:   info.GetId().GetSpaceId(),
			OpaqueID:  info.GetId().GetOpaqueId(),
		},
		Path:     info.GetPath(),
		Name:     info.GetName(),
		IsDir:    info.GetType() == provider.ResourceType_RESOURCE_TYPE_CONTAINER,
		Size:     info.GetSize(),
		MimeType: info.GetMimeType(),
		ETag:     info.GetEtag(),
		MTime:    protoTime(info.GetMtime()),
		ParentID: Ref{
			StorageID: info.GetParentId().GetStorageId(),
			SpaceID:   info.GetParentId().GetSpaceId(),
			OpaqueID:  info.GetParentId().GetOpaqueId(),
		},
		SHA1: checksum(info, provider.ResourceChecksumType_RESOURCE_CHECKSUM_TYPE_SHA1, "sha1"),
		MD5:  checksum(info, provider.ResourceChecksumType_RESOURCE_CHECKSUM_TYPE_MD5, "md5"),
	}
}

// checksum reads one checksum of a resource: the platform reports one of
// them in the checksum field and the others as plain opaque entries.
func checksum(info *provider.ResourceInfo, kind provider.ResourceChecksumType, key string) string {
	if sum := info.GetChecksum(); sum.GetType() == kind && sum.GetSum() != "" {
		return sum.GetSum()
	}
	if entry, ok := info.GetOpaque().GetMap()[key]; ok && entry.GetDecoder() == "plain" {
		return string(entry.GetValue())
	}
	return ""
}
