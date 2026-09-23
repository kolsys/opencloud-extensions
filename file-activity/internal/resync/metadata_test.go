package resync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// The layout and the format are the ones of the platform: a node of space
// b1f74ec4-… with id ac09e55e-… lives in
// spaces/b1/f74ec4-…/nodes/ac/09/e5/5e/-8c8c-….mpk.
func TestMetadataReadsTheBlobOfANode(t *testing.T) {
	root := t.TempDir()
	space, node := "b1f74ec4-dd7e-11ef-a543-03775734d0f7", "ac09e55e-8c8c-4fa7-9b51-258afaf88d44"
	dir := filepath.Join(root, "spaces", "b1", "f74ec4-dd7e-11ef-a543-03775734d0f7", "nodes", "ac", "09", "e5", "5e")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	encoded, err := msgpack.Marshal(map[string][]byte{
		"user.oc.blobid":   []byte("f2a49437-0542-4fb4-af3a-0e568e00c1dd"),
		"user.oc.blobsize": []byte("2000000"),
		"user.oc.name":     []byte("clip.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "-8c8c-4fa7-9b51-258afaf88d44.mpk"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	folder, _ := msgpack.Marshal(map[string][]byte{"user.oc.name": []byte("movies")})
	if err := os.WriteFile(filepath.Join(dir, "-folder.mpk"), folder, 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := NewMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := m.BlobID(t.Context(), space, node)
	if err != nil || blobID != "f2a49437-0542-4fb4-af3a-0e568e00c1dd" {
		t.Errorf("BlobID = %q, %v", blobID, err)
	}
	if blobID, err := m.BlobID(t.Context(), space, "ac09e55e-folder"); err != nil || blobID != "" {
		t.Errorf("a folder has a blob: %q, %v", blobID, err)
	}
	if _, err := m.BlobID(t.Context(), space, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNoMetadata) {
		t.Errorf("unknown node: %v", err)
	}

	if _, err := NewMetadata(t.TempDir()); err == nil {
		t.Error("a directory without spaces/ was taken for a storage root")
	}
}

func TestPathify(t *testing.T) {
	if got := pathify("ac09e55e-8c8c-4fa7-9b51-258afaf88d44", 4); got != filepath.Join("ac", "09", "e5", "5e", "-8c8c-4fa7-9b51-258afaf88d44") {
		t.Errorf("node = %q", got)
	}
	if got := pathify("b1f74ec4-dd7e-11ef-a543-03775734d0f7", 1); got != filepath.Join("b1", "f74ec4-dd7e-11ef-a543-03775734d0f7") {
		t.Errorf("space = %q", got)
	}
}
