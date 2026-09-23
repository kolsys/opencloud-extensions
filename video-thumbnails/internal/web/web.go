// Package web serves the web app of the extension out of the binary. The web
// of the platform loads it by URL from its config, so no asset has to reach
// the disk of the platform.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Route is the prefix the proxy of the platform sends to the handler; the
// route is unprotected, the app is loaded before anyone logs in.
const Route = "/api/video-thumbnails/web/"

// EntryName is the stable name of the remote entry. The build gives the
// file a hash, the config of the platform needs a name that does not
// change.
const EntryName = "remoteEntry.mjs"

const (
	manifestName = "manifest.json"
	distDir      = "dist"

	// Hashed files never change, the entry and the manifest do with every
	// build.
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"

	contentTypeModule = "text/javascript; charset=utf-8"
)

//go:embed all:dist
var dist embed.FS

// ErrNotBuilt reports a binary built without the web app.
var ErrNotBuilt = errors.New("web: the web app is not built into the binary")

type manifest struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Entrypoint string `json:"entrypoint"`
}

// Handler serves the built app.
type Handler struct {
	files   fs.FS
	entry   string
	missing bool
}

// New reads the manifest of the built app. A binary built without it gets a
// handler that answers 404 and ErrNotBuilt to tell the operator.
func New(log *slog.Logger) (*Handler, error) {
	return newFrom(dist, log)
}

func newFrom(root fs.FS, log *slog.Logger) (*Handler, error) {
	files, err := fs.Sub(root, distDir)
	if err != nil {
		return nil, err
	}

	raw, err := fs.ReadFile(files, manifestName)
	if err != nil {
		return &Handler{files: files, missing: true}, ErrNotBuilt
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.Join(errors.New("web: manifest.json is malformed"), err)
	}
	if m.Entrypoint == "" {
		return nil, errors.New("web: manifest.json names no entrypoint")
	}
	if _, err := fs.Stat(files, m.Entrypoint); err != nil {
		return nil, errors.Join(errors.New("web: the entrypoint of manifest.json is missing"), err)
	}

	log.Info("web app embedded",
		slog.String("name", m.Name),
		slog.String("app_version", m.Version),
		slog.String("entry", Route+EntryPath(m.Entrypoint)))
	return &Handler{files: files, entry: m.Entrypoint}, nil
}

// EntryPath is where the stable entry is served relative to Route: next to
// the hashed one, so that the relative imports of the chunks resolve.
func EntryPath(entrypoint string) string {
	dir := path.Dir(entrypoint)
	if dir == "." {
		return EntryName
	}
	return dir + "/" + EntryName
}

// ServeHTTP serves the files of the app: the stable entry always revalidated,
// the hashed files for good.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if h.missing {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, Route)), "/")
	switch {
	case name == EntryPath(h.entry):
		w.Header().Set("Cache-Control", cacheRevalidate)
		w.Header().Set("Content-Type", contentTypeModule)
		http.ServeFileFS(w, r, h.files, h.entry)
	case name == manifestName:
		w.Header().Set("Cache-Control", cacheRevalidate)
		http.ServeFileFS(w, r, h.files, name)
	default:
		if strings.HasSuffix(name, ".mjs") {
			w.Header().Set("Content-Type", contentTypeModule)
		}
		w.Header().Set("Cache-Control", cacheImmutable)
		http.ServeFileFS(w, r, h.files, name)
	}
}
