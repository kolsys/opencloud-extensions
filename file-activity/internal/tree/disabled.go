package tree

import "context"

// Disabled is the tree of a service configured without a bucket: it knows
// nothing and records nothing, so the feed carries what the events say and
// no more.
type Disabled struct{}

// PutFile records nothing.
func (Disabled) PutFile(context.Context, Entry) error { return nil }

// PlanMove knows no file.
func (Disabled) PlanMove(context.Context, string, string, string, bool) ([]Renamed, error) {
	return nil, nil
}

// ApplyMove changes nothing.
func (Disabled) ApplyMove(context.Context, []Renamed) error { return nil }

// PlanTrash knows no file.
func (Disabled) PlanTrash(context.Context, string, string, bool) ([]Entry, error) { return nil, nil }

// ApplyTrash changes nothing.
func (Disabled) ApplyTrash(context.Context, *Entry, []Entry) error { return nil }

// PlanRestore knows no file.
func (Disabled) PlanRestore(context.Context, string, string, string) (*Entry, []Entry, error) {
	return nil, nil, nil
}

// ApplyRestore changes nothing.
func (Disabled) ApplyRestore(context.Context, *Entry, []Entry) error { return nil }

// PlanPurge knows no file.
func (Disabled) PlanPurge(context.Context, string, string) (*Entry, []Entry, error) {
	return nil, nil, nil
}

// ApplyPurge changes nothing.
func (Disabled) ApplyPurge(context.Context, *Entry, []Entry) error { return nil }

// PlanEmptyTrash knows no file.
func (Disabled) PlanEmptyTrash(context.Context, string) ([]Entry, error) { return nil, nil }

// ApplyEmptyTrash changes nothing.
func (Disabled) ApplyEmptyTrash(context.Context, string) error { return nil }

// PutSpace records nothing.
func (Disabled) PutSpace(context.Context, Space) error { return nil }

// Space knows no space.
func (Disabled) Space(context.Context, string) (*Space, error) { return nil, ErrNotFound }

// PlanDeleteSpace knows no file.
func (Disabled) PlanDeleteSpace(context.Context, string) ([]Entry, error) { return nil, nil }

// ApplyDeleteSpace changes nothing.
func (Disabled) ApplyDeleteSpace(context.Context, string) error { return nil }
