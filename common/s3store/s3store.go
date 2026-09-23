// Package s3store keeps the thumbnails as plain objects. There is no index
// and no table: whether a thumbnail exists and whether it is current is
// answered by a HEAD on the object and its user metadata.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/kolsys/opencloud-extensions/common/config"
)

// Errors returned by this package.
var (
	// ErrNotFound means the object is not in the bucket.
	ErrNotFound = errors.New("s3store: object not found")

	ErrNoBucket = errors.New("s3store: a bucket name is required")
)

// Config is the bucket the thumbnails live in.
type Config struct {
	// Endpoint is the URL of the S3 service, with the scheme deciding TLS.
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string //nolint:gosec // an identifier, the secret of the pair is SecretKey
	SecretKey config.Secret
}

// Store is a bucket with a key prefix.
type Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// New builds a client. Addressing is path style, which both minio and Yandex
// Object Storage serve.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, ErrNoBucket
	}

	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("s3store: endpoint %q: must be an absolute URL", cfg.Endpoint)
	}

	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey.Reveal(), ""),
		Secure:       endpoint.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("s3store: client for %s: %w", cfg.Endpoint, err)
	}

	return &Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// Ping reports whether the bucket is reachable. It backs the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("s3store: bucket %s: %w", s.bucket, err)
	}
	if !exists {
		return fmt.Errorf("s3store: bucket %s does not exist", s.bucket)
	}
	return nil
}

// Key joins the parts into an object key under the prefix of the store.
func (s *Store) Key(parts ...string) string {
	return s.prefix + strings.Join(parts, "/")
}

// notFound reports whether an error of the client means the object or the
// bucket is simply not there.
func notFound(err error) bool {
	var response minio.ErrorResponse
	if !errors.As(err, &response) {
		return false
	}
	return response.StatusCode == http.StatusNotFound ||
		response.Code == "NoSuchKey" || response.Code == "NoSuchBucket"
}

// Object is what a HEAD or a listing reports about a stored object.
type Object struct {
	Key  string
	Size int64
	// ETag is the one S3 computed, unrelated to the etag of the platform,
	// which travels in the user metadata.
	ETag         string
	ContentType  string
	LastModified time.Time
	// Metadata holds the user metadata with the x-amz-meta- prefix stripped
	// and the keys lowercased.
	Metadata map[string]string
}

// metadata lowercases the keys the client hands back, which are canonicalised
// like HTTP headers.
func metadataOf(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}

	lowered := make(map[string]string, len(raw))
	for key, value := range raw {
		lowered[strings.ToLower(strings.TrimPrefix(key, "X-Amz-Meta-"))] = value
	}
	return lowered
}
