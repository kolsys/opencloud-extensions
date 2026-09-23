// Package errors holds the sentinel errors of the file-activity service.
package errors

import "errors"

// Errors the handlers branch on.
var (
	// ErrCursorTooOld means the client asked for events the stream no longer
	// holds. The client has to resync from scratch; starting from the first
	// available event silently would hide the gap.
	ErrCursorTooOld = errors.New("file-activity: cursor is older than the first available event")

	// ErrNotAllowed means the user is authenticated but not on the list of
	// users allowed to read the feed.
	ErrNotAllowed = errors.New("file-activity: user is not allowed to read the feed")

	// ErrGone means the resource of an event disappeared before it could be
	// looked up. The event is dropped and counted, not retried.
	ErrGone = errors.New("file-activity: resource is gone")
)
