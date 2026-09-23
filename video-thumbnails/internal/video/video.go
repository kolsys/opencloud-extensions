// Package video tells a video from the rest of the files, the one decision
// the reactor, the previews and the CLI have to make the same way.
package video

import (
	"path"
	"strings"
)

// Matcher recognises videos by the mime type the platform reports and by the
// extension for the types it does not know.
type Matcher struct {
	extensions map[string]struct{}
}

// NewMatcher returns a matcher for the given extensions, with or without the
// leading dot, in any case.
func NewMatcher(extensions []string) *Matcher {
	known := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		known["."+strings.TrimPrefix(ext, ".")] = struct{}{}
	}
	return &Matcher{extensions: known}
}

// Match reports whether a file is a video. The mime type wins when it says
// video, the extension decides otherwise; either may be empty.
func (m *Matcher) Match(mime, name string) bool {
	if strings.HasPrefix(strings.ToLower(mime), "video/") {
		return true
	}
	return m.MatchName(name)
}

// MatchName reports whether the extension of a file name is a video one.
func (m *Matcher) MatchName(name string) bool {
	_, ok := m.extensions[strings.ToLower(path.Ext(name))]
	return ok
}
