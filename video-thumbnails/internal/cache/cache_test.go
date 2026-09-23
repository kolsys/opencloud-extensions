package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutGet(t *testing.T) {
	c, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := c.Get("a"); ok {
		t.Fatal("hit on an empty cache")
	}

	path, err := c.Put("a", []byte("aaa"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get("a")
	if !ok || got != path {
		t.Fatalf("Get = %q, %v, want %q", got, ok, path)
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, []byte("aaa")) {
		t.Errorf("stored %q", data)
	}
	if c.Size() != 3 || c.Len() != 1 {
		t.Errorf("size %d, len %d", c.Size(), c.Len())
	}

	// A key written twice counts once.
	if _, err := c.Put("a", []byte("aaaaa")); err != nil {
		t.Fatal(err)
	}
	if c.Size() != 5 || c.Len() != 1 {
		t.Errorf("after rewrite: size %d, len %d", c.Size(), c.Len())
	}

	// A file removed behind the back of the cache is a miss, once.
	os.Remove(path)
	if _, ok := c.Get("a"); ok {
		t.Error("hit on a removed file")
	}
	if c.Len() != 0 || c.Size() != 0 {
		t.Errorf("after loss: size %d, len %d", c.Size(), c.Len())
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c, err := Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"a", "b", "c"} {
		if _, err := c.Put(key, bytes.Repeat([]byte(key), 4)); err != nil {
			t.Fatal(err)
		}
	}
	// a is gone (12 > 10), b and c stay.
	if _, ok := c.Get("a"); ok {
		t.Error("a survived")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b evicted")
	}

	// b was just used, so d pushes c out.
	if _, err := c.Put("d", []byte("dddd")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("c"); ok {
		t.Error("c survived although b was used later")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b evicted before c")
	}

	// One file above the limit is kept alone.
	if _, err := c.Put("big", bytes.Repeat([]byte("x"), 20)); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 || c.Size() != 20 {
		t.Errorf("after the big one: size %d, len %d", c.Size(), c.Len())
	}
}

func TestReopenKeepsOrderAndDropsTemp(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	older, _ := c.Put("old", []byte("1"))
	newer, _ := c.Put("new", []byte("22"))
	// The mtime is the order a reopened cache sees.
	past := time.Now().Add(-time.Hour)
	os.Chtimes(older, past, past)
	os.Chtimes(newer, past.Add(time.Minute), past.Add(time.Minute))

	temp := filepath.Join(dir, "ab", "abandoned"+tempSuffix)
	os.MkdirAll(filepath.Dir(temp), dirPerm)
	os.WriteFile(temp, []byte("half"), filePerm)

	// Reopened under a limit that fits one file: the older one goes.
	reopened, err := Open(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 1 || reopened.Size() != 2 {
		t.Errorf("reopened: size %d, len %d", reopened.Size(), reopened.Len())
	}
	if _, ok := reopened.Get("new"); !ok {
		t.Error("the newer file did not survive the reopen")
	}
	if _, ok := reopened.Get("old"); ok {
		t.Error("the older file survived the reopen")
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Error("the temporary file survived the reopen")
	}
}

func TestOpenRejectsBadArguments(t *testing.T) {
	if _, err := Open("", 1); err == nil {
		t.Error("empty dir accepted")
	}
	if _, err := Open(t.TempDir(), 0); err == nil {
		t.Error("zero limit accepted")
	}
}
