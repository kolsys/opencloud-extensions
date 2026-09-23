// Package cache is the disk cache of the previews: the masters read from the
// bucket and the variants rendered from them, under a size limit, the least
// recently used out first. It is rebuilt from the directory at start, and
// losing the directory costs nothing but the reads and the renders again.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	dirPerm  = 0o750
	filePerm = 0o640

	// fanOut is how many leading characters of a name pick its directory.
	fanOut = 2

	tempSuffix = ".tmp"
)

// Cache is a directory of files under a size limit.
type Cache struct {
	dir   string
	limit int64

	mu    sync.Mutex
	size  int64
	order *list.List // the front is the most recently used
	items map[string]*list.Element
}

type entry struct {
	name string
	size int64
}

// Open prepares the directory and indexes what it already holds, oldest
// first, so that the order of use survives a restart.
func Open(dir string, limit int64) (*Cache, error) {
	if dir == "" || limit < 1 {
		return nil, errors.New("cache: a directory and a positive limit are required")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}

	c := &Cache{dir: dir, limit: limit, order: list.New(), items: map[string]*list.Element{}}
	if err := c.index(); err != nil {
		return nil, err
	}
	return c, nil
}

// Get returns the path of a cached key and marks it as used. A file that
// vanished under the cache is forgotten and reported as a miss.
func (c *Cache) Get(key string) (string, bool) {
	name := nameOf(key)

	c.mu.Lock()
	element, ok := c.items[name]
	if ok {
		c.order.MoveToFront(element)
	}
	c.mu.Unlock()
	if !ok {
		return "", false
	}

	path := c.pathOf(name)
	if _, err := os.Stat(path); err != nil {
		c.mu.Lock()
		c.remove(name)
		c.mu.Unlock()
		return "", false
	}
	return path, true
}

// Put stores the bytes of a key and returns their path. The write goes to a
// temporary file first, so a reader never sees a partial one. Whatever is
// least recently used goes when the limit is exceeded.
func (c *Cache) Put(key string, data []byte) (string, error) {
	name := nameOf(key)
	path := c.pathOf(name)

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return "", fmt.Errorf("cache: %w", err)
	}
	temp := path + tempSuffix
	if err := os.WriteFile(temp, data, filePerm); err != nil {
		return "", fmt.Errorf("cache: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("cache: %w", err)
	}
	c.insert(name, int64(len(data)))
	c.evict()
	return path, nil
}

// Size is how many bytes the cache holds.
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// Len is how many files the cache holds.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

type found struct {
	name    string
	size    int64
	modTime time.Time
}

func (c *Cache) index() error {
	var files []found
	err := filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(path) == tempSuffix {
			// Left by a write that did not finish.
			return os.Remove(path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, found{name: d.Name(), size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: index %s: %w", c.dir, err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range files {
		c.insert(f.name, f.size)
	}
	c.evict()
	return nil
}

// insert records a file as the most recently used; a file already known is
// replaced. The caller holds the lock.
func (c *Cache) insert(name string, size int64) {
	if element, ok := c.items[name]; ok {
		c.size -= element.Value.(*entry).size
		c.order.Remove(element)
	}
	c.items[name] = c.order.PushFront(&entry{name: name, size: size})
	c.size += size
}

// evict drops the least recently used files until the cache fits its limit.
// The most recent one stays even when it alone exceeds it. The caller holds
// the lock.
func (c *Cache) evict() {
	for c.size > c.limit && c.order.Len() > 1 {
		oldest := c.order.Back().Value.(*entry)
		_ = os.Remove(c.pathOf(oldest.name))
		c.remove(oldest.name)
	}
}

// remove forgets a file. The caller holds the lock.
func (c *Cache) remove(name string) {
	element, ok := c.items[name]
	if !ok {
		return
	}
	c.size -= element.Value.(*entry).size
	c.order.Remove(element)
	delete(c.items, name)
}

func (c *Cache) pathOf(name string) string {
	return filepath.Join(c.dir, name[:fanOut], name)
}

// nameOf hashes a key into a file name: nothing of the key reaches the
// file system.
func nameOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
