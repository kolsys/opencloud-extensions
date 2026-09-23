package preview

import (
	"errors"
	"fmt"
	"image"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Defaults of the platform for a preview without dimensions.
const (
	defaultWidth  = 32
	defaultHeight = 32
)

// request is a preview request as the platform reads it.
type request struct {
	// davPath is the path of the file in any of the forms the webdav of the
	// platform accepts; the PROPFIND asks about the same path.
	davPath string
	// name is the last segment of the path, the file name.
	name      string
	box       image.Point
	processor string
}

// errNotPreview marks a request that is not a preview at all and goes to
// the platform as it is.
var errNotPreview = errors.New("preview: not a preview request")

// parse reads a request the way the platform does: x and y default to 32,
// zero or garbage is refused with the wording of the platform.
func parse(r *http.Request) (*request, error) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Query().Get("preview") != "1" {
		return nil, errNotPreview
	}

	query := r.URL.Query()
	width, err := dimension(query.Get("x"), "width", defaultWidth)
	if err != nil {
		return nil, err
	}
	height, err := dimension(query.Get("y"), "height", defaultHeight)
	if err != nil {
		return nil, err
	}

	davPath := path.Clean("/" + r.URL.Path)
	return &request{
		davPath:   davPath,
		name:      path.Base(davPath),
		box:       image.Pt(width, height),
		processor: query.Get("processor"),
	}, nil
}

func dimension(value, name string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 {
		// The wording is the one of the platform, kept for the clients that
		// match on it.
		return 0, fmt.Errorf("Cannot set %s of 0 or smaller!", name) //nolint:staticcheck,revive // the text of the platform
	}
	return int(parsed), nil
}

// etagOf is the ETag of a preview: the version of the file and what was
// asked of it. Requests that snap to the same variant may carry different
// ones, which costs nothing but a cache slot in the browser.
func (r *request) etagOf(fileETag string) string {
	return `"` + strings.Trim(fileETag, `"`) + "-" + processorOf(r.processor) + "-" +
		strconv.Itoa(r.box.X) + "x" + strconv.Itoa(r.box.Y) + `"`
}
