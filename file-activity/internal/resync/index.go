package resync

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"hash/fnv"
	"hash/maphash"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// index is what the tree knows about the files of one space, held compactly
// enough for a space of a million files: ids and blobs packed as uuids, the
// rest as 64-bit hashes. It answers by file id, for a file the platform holds
// at another path, and by path, for a key of the tree.
type index struct {
	seed maphash.Seed
	// recs are the files in the order of the tree.
	recs []record
	// byID orders recs by id, byPath by the path each was indexed under.
	byID   []int
	byPath []pathRef
	// blobs holds the blobs that are not uuids, by record.
	blobs map[int]string
}

// record is one file of the tree.
type record struct {
	id   [16]byte
	blob [16]byte
	// etag and attrs are hashes. path is the hash of the path of the file
	// in the tree, and of its live path once the record is seen.
	etag  uint64
	attrs uint64
	path  uint64
	flags uint8
}

// pathRef points a record at the hash of the path it was indexed under.
type pathRef struct {
	hash uint64
	rec  int
}

// Flags of a record.
const (
	// flagSeen marks a file the platform holds, at the path the record
	// hashes.
	flagSeen uint8 = 1 << iota
	// flagBlob marks a record with a blob.
	flagBlob
	// flagBlobText marks a blob that is not a uuid and is kept in blobs.
	flagBlobText
)

// none is the answer of a lookup that found nothing.
const none = -1

// uuidText is the shape of a packed id: lowercase hex with dashes.
const uuidText = "00000000-0000-0000-0000-000000000000"

func newIndex() *index {
	return &index{seed: maphash.MakeSeed(), blobs: map[int]string{}}
}

// add records an entry of the tree. finish must run before any lookup.
func (x *index) add(e tree.Entry) {
	r := record{id: packID(e.FileID), etag: x.hash(e.ETag), attrs: x.attrs(e), path: x.hash(e.Path)}
	if e.BlobID != "" {
		r.flags |= flagBlob
		if packed, ok := packUUID(e.BlobID); ok {
			r.blob = packed
		} else {
			r.flags |= flagBlobText
			x.blobs[len(x.recs)] = e.BlobID
		}
	}
	x.recs = append(x.recs, r)
}

// finish sorts the lookups.
func (x *index) finish() {
	x.byID = make([]int, len(x.recs))
	x.byPath = make([]pathRef, len(x.recs))
	for i, r := range x.recs {
		x.byID[i] = i
		x.byPath[i] = pathRef{hash: r.path, rec: i}
	}
	slices.SortStableFunc(x.byID, func(a, b int) int { return bytes.Compare(x.recs[a].id[:], x.recs[b].id[:]) })
	slices.SortFunc(x.byPath, func(a, b pathRef) int { return cmp.Compare(a.hash, b.hash) })
}

// size returns how many files the tree had.
func (x *index) size() int {
	return len(x.recs)
}

// rec returns a record by its index.
func (x *index) rec(i int) *record {
	return &x.recs[i]
}

// lookup returns the record of a file id, preferring the one at the given
// path when the tree holds the file at more than one, which is what an
// interrupted move leaves behind; none when the tree does not know the file.
func (x *index) lookup(fileID string, pathHash uint64) int {
	found := none
	for _, i := range x.sameID(packID(fileID)) {
		if x.recs[i].path == pathHash {
			return i
		}
		if found == none {
			found = i
		}
	}
	return found
}

// seenElsewhere reports whether the file of a record is live under another
// record of the same id.
func (x *index) seenElsewhere(i int) bool {
	for _, j := range x.sameID(x.recs[i].id) {
		if j != i && x.recs[j].flags&flagSeen != 0 {
			return true
		}
	}
	return false
}

// sameID returns the records of an id, in the order of the tree.
func (x *index) sameID(id [16]byte) []int {
	start, _ := slices.BinarySearchFunc(x.byID, id, func(i int, id [16]byte) int { return bytes.Compare(x.recs[i].id[:], id[:]) })
	end := start
	for end < len(x.byID) && x.recs[x.byID[end]].id == id {
		end++
	}
	return x.byID[start:end]
}

// atPath returns the record indexed under a path, none when the tree had no
// file there when the index was built.
func (x *index) atPath(p string) int {
	i, ok := slices.BinarySearchFunc(x.byPath, x.hash(p), func(ref pathRef, h uint64) int { return cmp.Compare(ref.hash, h) })
	if !ok {
		return none
	}
	return x.byPath[i].rec
}

// see marks the file of a record live at a path.
func (x *index) see(i int, pathHash uint64) {
	x.recs[i].flags |= flagSeen
	x.recs[i].path = pathHash
}

// same reports whether a record says of a file what an entry at a path says.
func (x *index) same(i int, e tree.Entry, pathHash uint64) bool {
	r := &x.recs[i]
	return r.path == pathHash && r.attrs == x.attrs(e) && x.blobOf(i) == e.BlobID
}

// blobOf returns the blob of a record, empty when it has none.
func (x *index) blobOf(i int) string {
	r := &x.recs[i]
	switch {
	case r.flags&flagBlob == 0:
		return ""
	case r.flags&flagBlobText != 0:
		return x.blobs[i]
	default:
		return unpackUUID(r.blob)
	}
}

// hash hashes a string with the seed of the index.
func (x *index) hash(s string) uint64 {
	return maphash.String(x.seed, s)
}

// attrs hashes what tells one version of a file from another, besides the
// blob and the path.
func (x *index) attrs(e tree.Entry) uint64 {
	return x.hash(strings.Join([]string{
		strconv.FormatUint(e.Size, 10),
		e.MTime.UTC().Format(time.RFC3339Nano),
		e.Mime, e.ETag, e.SHA1, e.MD5,
	}, "\x00"))
}

// packID packs a file id: the uuid it is, or a hash of what it is instead.
func packID(id string) [16]byte {
	if packed, ok := packUUID(id); ok {
		return packed
	}
	h := fnv.New128a()
	h.Write([]byte(id))
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// packUUID packs a lowercase canonical uuid. Anything else is refused, so
// that unpackUUID gives back exactly what was packed.
func packUUID(s string) ([16]byte, bool) {
	var out [16]byte
	if len(s) != len(uuidText) {
		return out, false
	}
	var hexed [32]byte
	n := 0
	for i := range len(s) {
		c := s[i]
		switch {
		case uuidText[i] == '-':
			if c != '-' {
				return out, false
			}
		case ('0' <= c && c <= '9') || ('a' <= c && c <= 'f'):
			hexed[n] = c
			n++
		default:
			return out, false
		}
	}
	if _, err := hex.Decode(out[:], hexed[:]); err != nil {
		return out, false
	}
	return out, true
}

// unpackUUID lays packed bytes out as a canonical uuid.
func unpackUUID(b [16]byte) string {
	var hexed [32]byte
	hex.Encode(hexed[:], b[:])
	out := make([]byte, 0, len(uuidText))
	n := 0
	for i := range len(uuidText) {
		if uuidText[i] == '-' {
			out = append(out, '-')
			continue
		}
		out = append(out, hexed[n])
		n++
	}
	return string(out)
}
