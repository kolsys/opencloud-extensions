package s3store

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
)

// Put writes an object. A size below zero streams the body with an unknown
// length. The metadata is stored as user metadata, where the etag of the
// platform lives.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string, meta map[string]string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: meta,
	})
	if err != nil {
		return fmt.Errorf("s3store: put %s: %w", key, err)
	}
	return nil
}

// Head reports what is stored under a key, ErrNotFound when nothing is.
func (s *Store) Head(ctx context.Context, key string) (*Object, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if notFound(err) {
			return nil, fmt.Errorf("s3store: head %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("s3store: head %s: %w", key, err)
	}

	return &Object{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
		Metadata:     metadataOf(info.UserMetadata),
	}, nil
}

// Get opens an object for reading. The caller closes the reader.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("s3store: get %s: %w", key, err)
	}

	// The client is lazy: nothing is requested until the first read or stat,
	// so a missing object shows up here rather than above.
	info, err := object.Stat()
	if err != nil {
		object.Close()
		if notFound(err) {
			return nil, nil, fmt.Errorf("s3store: get %s: %w", key, ErrNotFound)
		}
		return nil, nil, fmt.Errorf("s3store: get %s: %w", key, err)
	}

	return object, &Object{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
		Metadata:     metadataOf(info.UserMetadata),
	}, nil
}

// List returns the objects under a prefix. Garbage collection walks the whole
// bucket this way.
func (s *Store) List(ctx context.Context, prefix string) ([]Object, error) {
	objects := []Object{}
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if info.Err != nil {
			return nil, fmt.Errorf("s3store: list %s: %w", prefix, info.Err)
		}
		objects = append(objects, Object{
			Key:          info.Key,
			Size:         info.Size,
			ETag:         info.ETag,
			ContentType:  info.ContentType,
			LastModified: info.LastModified,
		})
	}
	return objects, nil
}

// Walk calls fn for every object under a prefix, in the order the bucket
// lists them, without holding the listing in memory. An error of fn stops
// the walk and is returned.
func (s *Store) Walk(ctx context.Context, prefix string, fn func(Object) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if info.Err != nil {
			return fmt.Errorf("s3store: list %s: %w", prefix, info.Err)
		}
		if err := fn(Object{
			Key:          info.Key,
			Size:         info.Size,
			ETag:         info.ETag,
			ContentType:  info.ContentType,
			LastModified: info.LastModified,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes one object. Removing what is not there is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3store: delete %s: %w", key, err)
	}
	return nil
}

// DeletePrefix removes everything under a prefix and returns how many objects
// were removed. A purged file and a deleted space are cleaned up this way.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}

	for _, object := range objects {
		if err := s.Delete(ctx, object.Key); err != nil {
			return 0, err
		}
	}
	return len(objects), nil
}
