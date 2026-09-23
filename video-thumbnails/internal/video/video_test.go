package video

import "testing"

func TestMatch(t *testing.T) {
	m := NewMatcher([]string{"mp4", ".MOV", " webm ", ""})

	cases := []struct {
		mime, name string
		want       bool
	}{
		{"video/mp4", "clip.mp4", true},
		{"video/x-matroska", "clip.mkv", true},
		{"VIDEO/quicktime", "clip.bin", true},
		{"application/octet-stream", "clip.mp4", true},
		{"", "CLIP.MOV", true},
		{"", "clip.webm", true},
		{"application/octet-stream", "clip.mkv", false},
		{"image/jpeg", "photo.jpg", false},
		{"", "", false},
		{"", "noext", false},
		{"", "dir.mp4/", false},
	}
	for _, c := range cases {
		if got := m.Match(c.mime, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.mime, c.name, got, c.want)
		}
	}
}
