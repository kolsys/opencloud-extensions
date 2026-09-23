package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/common/config"
)

// The tests run against the S3 of a stand and are skipped when
// VIDEO_THUMBNAILS_S3_ENDPOINT is unset, so that go test stays hermetic.
func store(t *testing.T) *Store {
	t.Helper()

	endpoint := os.Getenv("VIDEO_THUMBNAILS_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("VIDEO_THUMBNAILS_S3_ENDPOINT is unset, no stand to talk to")
	}

	s, err := New(Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("VIDEO_THUMBNAILS_S3_REGION"),
		Bucket:    os.Getenv("VIDEO_THUMBNAILS_S3_BUCKET"),
		Prefix:    "test/",
		AccessKey: os.Getenv("VIDEO_THUMBNAILS_S3_ACCESS_KEY"),
		SecretKey: config.Secret(os.Getenv("VIDEO_THUMBNAILS_S3_SECRET_KEY")),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPing(t *testing.T) {
	if err := store(t).Ping(testContext(t)); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// One thumbnail, written and read back the way the worker and the HTTP side
// handle it: the etag of the platform travels in the user metadata, and a
// HEAD is what tells whether the object is current.
func TestObjectRoundTrip(t *testing.T) {
	s := store(t)
	ctx := testContext(t)

	key := s.Key("space-1", "file-1", "master.jpg")
	t.Cleanup(func() { _ = s.Delete(context.Background(), key) })

	content := []byte("not really a jpeg")
	meta := map[string]string{"etag": `"abc123"`, "mime": "video/mp4", "source-size": "734003200"}

	if err := s.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "image/jpeg", meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	head, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", head.Size, len(content))
	}
	if head.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q", head.ContentType)
	}
	for key, want := range meta {
		if got := head.Metadata[key]; got != want {
			t.Errorf("metadata %q = %q, want %q", key, got, want)
		}
	}

	body, object, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()

	read, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(read, content) {
		t.Errorf("read %q, want %q", read, content)
	}
	if object.Metadata["etag"] != meta["etag"] {
		t.Errorf("Get lost the metadata: %v", object.Metadata)
	}
}

func TestHeadOfAMissingObject(t *testing.T) {
	s := store(t)
	if _, err := s.Head(testContext(t), s.Key("space-1", "nothing", "master.jpg")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head: %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(testContext(t), s.Key("space-1", "nothing", "master.jpg")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: %v, want ErrNotFound", err)
	}
}

// A purged file and a deleted space are cleaned up by prefix.
func TestListAndDeletePrefix(t *testing.T) {
	s := store(t)
	ctx := testContext(t)

	prefix := s.Key("space-2", "file-2") + "/"
	t.Cleanup(func() { _, _ = s.DeletePrefix(context.Background(), prefix) })

	for _, name := range []string{"master.jpg", "32x32.jpg", "500x280.jpg"} {
		if err := s.Put(ctx, prefix+name, strings.NewReader("x"), 1, "image/jpeg", nil); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	objects, err := s.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 3 {
		t.Fatalf("List returned %d objects, want 3", len(objects))
	}

	removed, err := s.DeletePrefix(ctx, prefix)
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if removed != 3 {
		t.Errorf("DeletePrefix removed %d, want 3", removed)
	}

	left, err := s.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d objects survived the prefix delete", len(left))
	}
}

func TestNewNeedsABucket(t *testing.T) {
	if _, err := New(Config{Endpoint: "http://minio:9000"}); !errors.Is(err, ErrNoBucket) {
		t.Errorf("New without a bucket: %v, want ErrNoBucket", err)
	}
	if _, err := New(Config{Endpoint: "minio:9000", Bucket: "b"}); err == nil {
		t.Error("New accepted an endpoint without a scheme")
	}
}
