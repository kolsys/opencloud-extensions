// Package store is the layout of the thumbnails in the bucket:
//
//	{prefix}{spaceid}/{fileid}/master.jpg   x-amz-meta-etag, -mime, -source-size
//	{prefix}{spaceid}/{fileid}/failed.json  the generation was given up on
//
// There is no index: the master and its metadata are the state of a file.
// The variants are rendered from the master on request and live in the disk
// cache of the service only.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/kolsys/opencloud-extensions/common/s3store"
)

// Names inside the directory of a file.
const (
	MasterName = "master.jpg"
	FailedName = "failed.json"

	contentTypeJPEG = "image/jpeg"
	contentTypeJSON = "application/json"
)

// Keys of the user metadata of a master.
const (
	MetaETag       = "etag"
	MetaMime       = "mime"
	MetaSourceSize = "source-size"
)

// Store lays thumbnails out in a bucket.
type Store struct {
	s3 *s3store.Store
}

// New wraps a bucket.
func New(s3 *s3store.Store) *Store {
	return &Store{s3: s3}
}

// Ping backs the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.s3.Ping(ctx)
}

// Dir is the key prefix of one file.
func (s *Store) Dir(spaceID, fileID string) string {
	return s.s3.Key(spaceID, fileID) + "/"
}

// SpaceDir is the key prefix of one space.
func (s *Store) SpaceDir(spaceID string) string {
	return s.s3.Key(spaceID) + "/"
}

// Master is what is known about the stored master frame of a file.
type Master struct {
	// ETag is the version of the file the master was rendered from.
	ETag       string
	Mime       string
	SourceSize int64
	Size       int64
	Stored     time.Time
	// ObjectETag is the ETag of the master object itself, which changes
	// whenever the master is replaced, an import included.
	ObjectETag string
}

// Current reports whether the master was rendered from the given version.
func (m *Master) Current(etag string) bool {
	return m != nil && m.ETag == etag
}

// HeadMaster reports the stored master of a file, s3store.ErrNotFound when
// there is none.
func (s *Store) HeadMaster(ctx context.Context, spaceID, fileID string) (*Master, error) {
	object, err := s.s3.Head(ctx, s.Dir(spaceID, fileID)+MasterName)
	if err != nil {
		return nil, err
	}
	return masterOf(object), nil
}

func masterOf(object *s3store.Object) *Master {
	master := &Master{
		ETag:       object.Metadata[MetaETag],
		Mime:       object.Metadata[MetaMime],
		Size:       object.Size,
		Stored:     object.LastModified,
		ObjectETag: strings.Trim(object.ETag, `"`),
	}
	master.SourceSize, _ = strconv.ParseInt(object.Metadata[MetaSourceSize], 10, 64)
	return master
}

// GetMaster opens the master frame of a file. The caller closes the reader.
func (s *Store) GetMaster(ctx context.Context, spaceID, fileID string) (io.ReadCloser, *Master, error) {
	body, object, err := s.s3.Get(ctx, s.Dir(spaceID, fileID)+MasterName)
	if err != nil {
		return nil, nil, err
	}
	return body, masterOf(object), nil
}

// PutMaster stores a freshly rendered master and drops everything else under
// the file: a failure marker belongs to the previous version.
func (s *Store) PutMaster(ctx context.Context, spaceID, fileID string, jpeg []byte, master Master) error {
	dir := s.Dir(spaceID, fileID)
	meta := map[string]string{
		MetaETag:       master.ETag,
		MetaMime:       master.Mime,
		MetaSourceSize: strconv.FormatInt(master.SourceSize, 10),
	}
	if err := s.s3.Put(ctx, dir+MasterName, bytes.NewReader(jpeg), int64(len(jpeg)), contentTypeJPEG, meta); err != nil {
		return err
	}

	objects, err := s.s3.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if object.Key == dir+MasterName {
			continue
		}
		if err := s.s3.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	return nil
}

// Failure is the marker stored when a generation is given up on.
type Failure struct {
	ETag     string    `json:"etag"`
	Error    string    `json:"error"`
	Attempts int       `json:"attempts"`
	FailedAt time.Time `json:"failed_at"`
}

// PutFailure stores the marker. The previews answer 404 for the file until a
// new version replaces it.
func (s *Store) PutFailure(ctx context.Context, spaceID, fileID string, failure Failure) error {
	body, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("store: encode failure: %w", err)
	}
	return s.s3.Put(ctx, s.Dir(spaceID, fileID)+FailedName, bytes.NewReader(body), int64(len(body)), contentTypeJSON, nil)
}

// Failure reports the marker of a file, s3store.ErrNotFound when there is
// none.
func (s *Store) Failure(ctx context.Context, spaceID, fileID string) (*Failure, error) {
	body, _, err := s.s3.Get(ctx, s.Dir(spaceID, fileID)+FailedName)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var failure Failure
	if err := json.NewDecoder(body).Decode(&failure); err != nil {
		return nil, fmt.Errorf("store: decode failure: %w", err)
	}
	return &failure, nil
}

// WalkMasters calls fn with the space and the file of every master in the
// bucket, in the order the bucket lists them. Nothing but the keys is read.
func (s *Store) WalkMasters(ctx context.Context, fn func(spaceID, fileID string) error) error {
	prefix := s.s3.Key()
	return s.s3.Walk(ctx, prefix, func(object s3store.Object) error {
		parts := strings.Split(strings.TrimPrefix(object.Key, prefix), "/")
		if len(parts) != 3 || parts[2] != MasterName {
			return nil
		}
		return fn(parts[0], parts[1])
	})
}

// DeleteFile removes everything stored for a file.
func (s *Store) DeleteFile(ctx context.Context, spaceID, fileID string) (int, error) {
	return s.s3.DeletePrefix(ctx, s.Dir(spaceID, fileID))
}

// DeleteSpace removes everything stored for a space.
func (s *Store) DeleteSpace(ctx context.Context, spaceID string) (int, error) {
	return s.s3.DeletePrefix(ctx, s.SpaceDir(spaceID))
}
