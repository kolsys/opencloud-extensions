// Package errors holds the sentinel errors of the video-thumbnails service.
package errors

import "errors"

// Errors the handlers branch on.
var (
	// ErrNotVideo means the resource is not a video and has no business in
	// the queue.
	ErrNotVideo = errors.New("video-thumbnails: not a video")

	// ErrGone means the file disappeared or changed before its job ran; a
	// newer job or nothing at all takes its place.
	ErrGone = errors.New("video-thumbnails: file is gone or changed")

	// ErrNoFrame means ffmpeg produced no frame at all.
	ErrNoFrame = errors.New("video-thumbnails: ffmpeg produced no frame")

	// ErrFailed means the generation was given up on and the failure marker
	// is stored: the previews answer 404 until a new version arrives.
	ErrFailed = errors.New("video-thumbnails: generation failed for good")
)
