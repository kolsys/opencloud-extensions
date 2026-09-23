// Package treetest holds an in-memory bucket for the tests of the tree and
// of what writes to it.
package treetest

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/kolsys/opencloud-extensions/common/s3store"
)

// Objects is a bucket in a map. It counts its writes and deletes.
type Objects struct {
	mu      sync.Mutex
	prefix  string
	objects map[string][]byte
	Puts    int
	Deletes int
}

// New returns an empty bucket whose keys start with prefix.
func New(prefix string) *Objects {
	return &Objects{prefix: prefix, objects: map[string][]byte{}}
}

// Key joins the parts under the prefix, like the S3 store does.
func (o *Objects) Key(parts ...string) string { return o.prefix + strings.Join(parts, "/") }

// Put stores the body.
func (o *Objects) Put(_ context.Context, key string, body io.Reader, _ int64, _ string, _ map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[key] = data
	o.Puts++
	return nil
}

// Get opens an object, s3store.ErrNotFound when there is none.
func (o *Objects) Get(_ context.Context, key string) (io.ReadCloser, *s3store.Object, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.objects[key]
	if !ok {
		return nil, nil, s3store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), &s3store.Object{Key: key, Size: int64(len(data))}, nil
}

// Delete removes an object; a missing one is not an error.
func (o *Objects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objects, key)
	o.Deletes++
	return nil
}

// Walk calls fn for every key under a prefix, in key order.
func (o *Objects) Walk(_ context.Context, prefix string, fn func(s3store.Object) error) error {
	for _, key := range o.Keys() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if err := fn(s3store.Object{Key: key}); err != nil {
			return err
		}
	}
	return nil
}

// Keys lists every key, sorted.
func (o *Objects) Keys() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	keys := make([]string, 0, len(o.objects))
	for key := range o.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
