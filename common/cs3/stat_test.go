package cs3

import "testing"

func TestPathRef(t *testing.T) {
	info := &ResourceInfo{ID: Ref{StorageID: "storage", SpaceID: "space", OpaqueID: "file"}, Path: "/videos/clip.mp4"}
	ref := info.PathRef()
	if ref.OpaqueID != "space" || ref.SpaceID != "space" || ref.StorageID != "storage" {
		t.Errorf("PathRef is not anchored at the space root: %+v", ref)
	}
	if ref.Path != "./videos/clip.mp4" {
		t.Errorf("Path = %q, want the relative form the events use", ref.Path)
	}

	root := &ResourceInfo{ID: Ref{SpaceID: "space", OpaqueID: "space"}, Path: "."}
	if got := root.PathRef().Path; got != "." {
		t.Errorf("PathRef of the root = %q", got)
	}
}
