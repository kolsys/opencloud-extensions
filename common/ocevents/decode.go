// Package ocevents decodes the events OpenCloud publishes to the main-queue
// stream of its NATS.
//
// A message is a go-micro envelope whose Payload holds the JSON of one event,
// and whose Metadata names the type and the id of that event. The structs of
// the platform are mirrored here rather than imported: importing reva would
// pull half of the platform in for a dozen structs, and the wire format would
// still have to be checked against fixtures.
package ocevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Keys of the envelope metadata the platform fills in.
const (
	MetadataKeyEventType   = "eventtype"
	MetadataKeyEventID     = "eventid"
	MetadataKeyTraceParent = "traceparent"
	MetadataKeyInitiatorID = "initiatorid"
)

// MainQueue is the stream and the subject the platform publishes to.
const MainQueue = "main-queue"

// Errors returned while decoding a message.
var (
	ErrMalformedEnvelope = errors.New("ocevents: malformed envelope")
	ErrMissingType       = errors.New("ocevents: envelope without an event type")
)

// Envelope is the go-micro message wrapping every event.
type Envelope struct {
	Timestamp time.Time         `json:"Timestamp"`
	Metadata  map[string]string `json:"Metadata"`
	ID        string            `json:"ID"`
	Topic     string            `json:"Topic"`
	Payload   []byte            `json:"Payload"`
}

// Event is one decoded event of the platform.
type Event struct {
	// ID is the eventid of the platform, unique per event and used as the
	// deduplication key when the event is republished.
	ID string
	// Type is the Go type name the platform put in the metadata, like
	// "events.UploadReady".
	Type string
	// Queued is the timestamp of the envelope.
	Queued time.Time
	// Payload is a pointer to one of the event structs of this package, or
	// nil when the type is not one we handle.
	Payload any
}

// Known reports whether the event is one this package decodes.
func (e Event) Known() bool {
	return e.Payload != nil
}

// payloads maps the type name of the platform to a fresh value to decode into.
var payloads = map[string]func() any{
	"events.ContainerCreated":    func() any { return &ContainerCreated{} },
	"events.FileUploaded":        func() any { return &FileUploaded{} },
	"events.FileTouched":         func() any { return &FileTouched{} },
	"events.FileDownloaded":      func() any { return &FileDownloaded{} },
	"events.FileLocked":          func() any { return &FileLocked{} },
	"events.FileUnlocked":        func() any { return &FileUnlocked{} },
	"events.ItemTrashed":         func() any { return &ItemTrashed{} },
	"events.ItemMoved":           func() any { return &ItemMoved{} },
	"events.TrashbinPurged":      func() any { return &TrashbinPurged{} },
	"events.ItemPurged":          func() any { return &ItemPurged{} },
	"events.ItemRestored":        func() any { return &ItemRestored{} },
	"events.FileVersionRestored": func() any { return &FileVersionRestored{} },
	"events.UploadReady":         func() any { return &UploadReady{} },
	"events.SpaceCreated":        func() any { return &SpaceCreated{} },
	"events.SpaceRenamed":        func() any { return &SpaceRenamed{} },
	"events.SpaceDeleted":        func() any { return &SpaceDeleted{} },
}

// Decode decodes one message of main-queue.
//
// An event of a type this package does not handle is not an error: the Type
// is reported, Payload stays nil, and the caller acknowledges the message and
// counts it as skipped.
func Decode(message []byte) (Event, error) {
	var envelope Envelope
	if err := json.Unmarshal(message, &envelope); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrMalformedEnvelope, err)
	}

	event := Event{
		ID:     envelope.Metadata[MetadataKeyEventID],
		Type:   envelope.Metadata[MetadataKeyEventType],
		Queued: envelope.Timestamp,
	}
	if event.Type == "" {
		return event, ErrMissingType
	}

	newPayload, ok := payloads[event.Type]
	if !ok {
		return event, nil
	}

	payload := newPayload()
	if err := json.Unmarshal(envelope.Payload, payload); err != nil {
		return event, fmt.Errorf("ocevents: %s: %w", event.Type, err)
	}
	event.Payload = payload

	return event, nil
}

// Types returns the type names this package decodes, for the readiness output
// and the tests.
func Types() []string {
	types := make([]string, 0, len(payloads))
	for name := range payloads {
		types = append(types, name)
	}
	return types
}
