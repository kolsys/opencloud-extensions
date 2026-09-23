package restore

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// Limits of the WebDAV sink. A PUT of a large file has no deadline of its
// own; the context of the run bounds it.
const (
	davHeaderTimeout = 5 * time.Minute
	maxErrorBody     = 1024
	maxPropfind      = 1 << 20
)

// Placeholders of a target template.
const (
	PlaceholderSpaceID   = "{space_id}"
	PlaceholderSpaceName = "{space_name}"
)

// Target is where a space lands: a URL template with placeholders for the
// id and the name of the space, and a map from the ids the tree knows to
// the ids the destination gave the spaces, for a destination that is an
// OpenCloud.
type Target struct {
	Template string
	SpaceMap map[string]string
}

// Resolve returns the base URL of a space, without a trailing slash.
func (t Target) Resolve(space spaceInfo) string {
	id := space.ID
	if mapped, ok := t.SpaceMap[id]; ok {
		id = mapped
	}
	base := strings.ReplaceAll(t.Template, PlaceholderSpaceID, url.PathEscape(id))
	base = strings.ReplaceAll(base, PlaceholderSpaceName, url.PathEscape(space.Name))
	return strings.TrimSuffix(base, "/")
}

// spaceInfo is what Resolve needs of a space.
type spaceInfo struct {
	ID   string
	Name string
}

// Credentials of the destination: a user with a token or password for
// Basic, or a bearer token.
type Credentials struct {
	User     string
	Password string //nolint:gosec // G117: a credential by design, never serialised
	Bearer   string
}

func (c Credentials) apply(r *http.Request) {
	switch {
	case c.Bearer != "":
		r.Header.Set("Authorization", "Bearer "+c.Bearer)
	case c.User != "":
		r.SetBasicAuth(c.User, c.Password)
	}
}

// WebDAV writes files to a WebDAV endpoint: any server that takes MKCOL and
// PUT. The headers OpenCloud and Nextcloud read are always sent: the
// modification time, and the checksum the server verifies the upload with.
type WebDAV struct {
	target Target
	creds  Credentials
	http   *http.Client

	mu      sync.Mutex
	created map[string]bool
}

// NewWebDAV returns a sink for a target.
func NewWebDAV(target Target, creds Credentials, insecure bool) (*WebDAV, error) {
	if !strings.HasPrefix(target.Template, "http://") && !strings.HasPrefix(target.Template, "https://") {
		return nil, fmt.Errorf("restore: target %q: must be an http or https URL", target.Template)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = davHeaderTimeout
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure, //nolint:gosec // a stand runs on a self signed certificate
	}

	return &WebDAV{
		target:  target,
		creds:   creds,
		http:    &http.Client{Transport: transport},
		created: map[string]bool{},
	}, nil
}

// Put writes one file, creating the folders above it first, the one of the
// space included.
func (w *WebDAV) Put(ctx context.Context, space tree.Space, e tree.Entry, body io.Reader) error {
	base := w.target.Resolve(spaceInfo{ID: space.ID, Name: space.Name})
	if err := w.ensureBase(ctx, base); err != nil {
		return err
	}
	if err := w.ensureDirs(ctx, base, path.Dir(e.Path)); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, base+escape(e.Path), body)
	if err != nil {
		return fmt.Errorf("restore: put %s: %w", e.Path, err)
	}
	request.ContentLength = int64(e.Size) //nolint:gosec // a file size fits
	if e.Mime != "" {
		request.Header.Set("Content-Type", e.Mime)
	}
	if !e.MTime.IsZero() {
		request.Header.Set("X-Oc-Mtime", strconv.FormatInt(e.MTime.Unix(), 10))
	}
	if e.SHA1 != "" {
		request.Header.Set("Oc-Checksum", "SHA1:"+e.SHA1)
	}
	w.creds.apply(request)

	response, err := w.http.Do(request) //nolint:gosec // G704: the URL is the target the operator named
	if err != nil {
		return fmt.Errorf("restore: put %s: %w", e.Path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("restore: put %s: %s", e.Path, reason(response))
	}
	return nil
}

// Exists reports whether the destination holds a file of the same size at
// the path.
func (w *WebDAV) Exists(ctx context.Context, space tree.Space, e tree.Entry) (bool, error) {
	base := w.target.Resolve(spaceInfo{ID: space.ID, Name: space.Name})
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", base+escape(e.Path), strings.NewReader(propfindSize))
	if err != nil {
		return false, fmt.Errorf("restore: propfind %s: %w", e.Path, err)
	}
	request.Header.Set("Depth", "0")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	w.creds.apply(request)

	response, err := w.http.Do(request) //nolint:gosec // G704: the URL is the target the operator named
	if err != nil {
		return false, fmt.Errorf("restore: propfind %s: %w", e.Path, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusMultiStatus:
	default:
		return false, fmt.Errorf("restore: propfind %s: %s", e.Path, reason(response))
	}

	var parsed struct {
		Length []string `xml:"response>propstat>prop>getcontentlength"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, maxPropfind)).Decode(&parsed); err != nil {
		return false, fmt.Errorf("restore: propfind %s: %w", e.Path, err)
	}
	for _, length := range parsed.Length {
		if size, err := strconv.ParseUint(strings.TrimSpace(length), 10, 64); err == nil && size == e.Size {
			return true, nil
		}
	}
	return false, nil
}

const propfindSize = xml.Header + `<d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/></d:prop></d:propfind>`

// ensureBase makes sure the folder a space lands in exists: the root of a
// space of the destination does, a folder named in the template may not.
// Its parent has to exist; a template is not a path to create top down.
func (w *WebDAV) ensureBase(ctx context.Context, base string) error {
	w.mu.Lock()
	done := w.created[base]
	w.mu.Unlock()
	if done {
		return nil
	}

	request, err := http.NewRequestWithContext(ctx, "PROPFIND", base, strings.NewReader(propfindSize))
	if err != nil {
		return fmt.Errorf("restore: propfind %s: %w", base, err)
	}
	request.Header.Set("Depth", "0")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	w.creds.apply(request)
	response, err := w.http.Do(request) //nolint:gosec // G704: the URL is the target the operator named
	if err != nil {
		return fmt.Errorf("restore: propfind %s: %w", base, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxPropfind))
	response.Body.Close()

	switch response.StatusCode {
	case http.StatusMultiStatus:
	case http.StatusNotFound:
		if err := w.mkcol(ctx, base); err != nil {
			return err
		}
	default:
		return fmt.Errorf("restore: propfind %s: %s", base, response.Status)
	}

	w.mu.Lock()
	w.created[base] = true
	w.mu.Unlock()
	return nil
}

// ensureDirs creates a folder and the ones above it, once per run.
func (w *WebDAV) ensureDirs(ctx context.Context, base, dir string) error {
	if dir == "/" || dir == "." || dir == "" {
		return nil
	}
	key := base + dir
	w.mu.Lock()
	done := w.created[key]
	w.mu.Unlock()
	if done {
		return nil
	}

	if err := w.ensureDirs(ctx, base, path.Dir(dir)); err != nil {
		return err
	}
	if err := w.mkcol(ctx, base+escape(dir)); err != nil {
		return err
	}

	w.mu.Lock()
	w.created[key] = true
	w.mu.Unlock()
	return nil
}

// mkcol creates one folder; one that exists already is fine.
func (w *WebDAV) mkcol(ctx context.Context, target string) error {
	request, err := http.NewRequestWithContext(ctx, "MKCOL", target, nil)
	if err != nil {
		return fmt.Errorf("restore: mkcol %s: %w", target, err)
	}
	w.creds.apply(request)
	response, err := w.http.Do(request) //nolint:gosec // G704: the URL is the target the operator named
	if err != nil {
		return fmt.Errorf("restore: mkcol %s: %w", target, err)
	}
	defer response.Body.Close()
	// 405 is a folder that exists already, which is the point.
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusMethodNotAllowed {
		return fmt.Errorf("restore: mkcol %s: %s", target, reason(response))
	}
	return nil
}

// escape makes a path of the tree a path of a URL, segment by segment.
func escape(p string) string {
	segments := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return "/" + strings.Join(segments, "/")
}

// reason is the status of a rejected request with the start of its body.
func reason(response *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	text := strings.TrimSpace(string(body))
	if text == "" {
		return response.Status
	}
	return response.Status + ": " + text
}

// ErrNoCredentials reports a destination without a way to authenticate.
var ErrNoCredentials = errors.New("restore: the destination needs a user with a token or a bearer token")
