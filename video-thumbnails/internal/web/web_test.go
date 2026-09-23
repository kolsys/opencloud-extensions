package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func built() fstest.MapFS {
	return fstest.MapFS{
		"dist/manifest.json":             {Data: []byte(`{"name":"video-thumbnails","version":"0.1.0","entrypoint":"js/remoteEntry-abc123.mjs"}`)},
		"dist/js/remoteEntry-abc123.mjs": {Data: []byte("export default 1")},
		"dist/js/chunk-def456.mjs":       {Data: []byte("export const x = 2")},
	}
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestServesTheStableEntryAndTheHashedFiles(t *testing.T) {
	h, err := newFrom(built(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	w := get(t, h, http.MethodGet, Route+"js/remoteEntry.mjs")
	if w.Code != http.StatusOK || w.Body.String() != "export default 1" {
		t.Fatalf("entry: %d %q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("entry Cache-Control %q", cc)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("entry Content-Type %q", ct)
	}

	w = get(t, h, http.MethodGet, Route+"js/chunk-def456.mjs")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("chunk: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}

	w = get(t, h, http.MethodGet, Route+"manifest.json")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("manifest: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}

	if w := get(t, h, http.MethodGet, Route+"js/missing.mjs"); w.Code != http.StatusNotFound {
		t.Errorf("missing file: %d", w.Code)
	}
	if w := get(t, h, http.MethodGet, Route+"../../etc/passwd"); w.Code < http.StatusBadRequest {
		t.Errorf("escape: %d", w.Code)
	}
	if w := get(t, h, http.MethodPost, Route+"js/remoteEntry.mjs"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestBinaryWithoutTheApp(t *testing.T) {
	h, err := newFrom(fstest.MapFS{"dist/.gitkeep": {}}, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrNotBuilt) || h == nil {
		t.Fatalf("newFrom = %v, %v", h, err)
	}
	if w := get(t, h, http.MethodGet, Route+"remoteEntry.mjs"); w.Code != http.StatusNotFound {
		t.Errorf("%d", w.Code)
	}
}

func TestBrokenManifest(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"malformed":       {"dist/manifest.json": {Data: []byte("{")}},
		"no entrypoint":   {"dist/manifest.json": {Data: []byte("{}")}},
		"entry not there": {"dist/manifest.json": {Data: []byte(`{"entrypoint":"js/x.mjs"}`)}},
	} {
		if _, err := newFrom(files, slog.New(slog.DiscardHandler)); err == nil || errors.Is(err, ErrNotBuilt) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestEntryPath(t *testing.T) {
	for in, want := range map[string]string{"remoteEntry-x.mjs": "remoteEntry.mjs", "js/remoteEntry-x.mjs": "js/remoteEntry.mjs"} {
		if got := EntryPath(in); got != want {
			t.Errorf("EntryPath(%q) = %q, want %q", in, got, want)
		}
	}
}
