package resync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// The layout of the metadata of the decomposed storage of the platform: one
// messagepack file per node, the ids split into directories of two
// characters, the space one level deep and the node four.
//
//	{root}/spaces/{sp[0:2]}/{sp[2:]}/nodes/{id[0:2]}/{id[2:4]}/{id[4:6]}/{id[6:8]}/{id[8:]}.mpk
const (
	spacesDir  = "spaces"
	nodesDir   = "nodes"
	nodeSuffix = ".mpk"

	// blobIDAttr is the attribute that names the blob of a file.
	blobIDAttr = "user.oc.blobid"

	spaceDepth, nodeDepth, width = 1, 4, 2

	// maxNode bounds one metadata file; they run to a few kilobytes.
	maxNode = 16 << 20
)

// ErrNoMetadata reports a node the metadata has no file for.
var ErrNoMetadata = errors.New("resync: no metadata for the node")

// Metadata reads the blob ids out of the metadata of the platform on disk,
// the one place they are kept: the same files the platform reads, mounted
// read-only for the run.
type Metadata struct {
	root string
}

// NewMetadata returns a reader for the root of the storage of the platform,
// the directory that holds spaces/.
func NewMetadata(root string) (*Metadata, error) {
	info, err := os.Stat(filepath.Join(root, spacesDir))
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("resync: %s is not the root of a decomposed storage: no %s directory", root, spacesDir)
	}
	return &Metadata{root: root}, nil
}

// BlobID returns the blob a node holds, empty for a node without one, and
// ErrNoMetadata for a node the storage has no file for.
func (m *Metadata) BlobID(_ context.Context, spaceID, nodeID string) (string, error) {
	path := m.nodePath(spaceID, nodeID)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoMetadata, path)
		}
		return "", fmt.Errorf("resync: read %s: %w", path, err)
	}
	if len(raw) > maxNode {
		return "", fmt.Errorf("resync: %s: %d bytes, not a node", path, len(raw))
	}

	attributes := map[string][]byte{}
	if err := msgpack.Unmarshal(raw, &attributes); err != nil {
		return "", fmt.Errorf("resync: decode %s: %w", path, err)
	}
	return string(attributes[blobIDAttr]), nil
}

func (m *Metadata) nodePath(spaceID, nodeID string) string {
	return filepath.Join(m.root, spacesDir, pathify(spaceID, spaceDepth), nodesDir, pathify(nodeID, nodeDepth)+nodeSuffix)
}

// pathify splits an id into directories of two characters, depth times, the
// way the platform lays its metadata out.
func pathify(id string, depth int) string {
	var parts []string
	for range depth {
		if len(id) <= width {
			break
		}
		parts = append(parts, id[:width])
		id = id[width:]
	}
	return strings.Join(append(parts, id), string(filepath.Separator))
}
