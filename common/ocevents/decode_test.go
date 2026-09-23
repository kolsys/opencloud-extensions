package ocevents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The fixtures are envelopes taken off the main-queue of a stand, one file
// per event type. They are the only guard against the wire format of the
// platform drifting away from the structs of this package.
const fixtures = "testdata"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	message, err := os.ReadFile(filepath.Join(fixtures, name+".json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return message
}

func TestDecodeUploadReady(t *testing.T) {
	event, err := Decode(readFixture(t, "UploadReady"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.Type != "events.UploadReady" {
		t.Fatalf("Type = %q", event.Type)
	}
	if event.ID == "" {
		t.Error("ID is empty, the eventid of the envelope was lost")
	}

	ready, ok := event.Payload.(*UploadReady)
	if !ok {
		t.Fatalf("Payload is %T", event.Payload)
	}
	if ready.Failed {
		t.Error("Failed is set on an upload that succeeded")
	}
	if ready.Filename == "" {
		t.Error("Filename is empty")
	}
	// The event carries no file id: the path is relative to the root of the
	// space and the fileid has to come from a Stat on it.
	if ready.FileRef == nil || ready.FileRef.Path == "" {
		t.Fatal("FileRef is empty")
	}
	if ready.FileRef.SpaceID() == "" {
		t.Error("FileRef has no space id")
	}
	if ready.ParentID.Empty() {
		t.Error("ParentID is empty on a successful upload")
	}
	if ready.Timestamp.Time().IsZero() {
		t.Error("Timestamp did not decode")
	}
	if ready.ExecutingUser == nil || ready.ExecutingUser.Username == "" {
		t.Error("ExecutingUser has no user name")
	}
}

func TestDecodeSpaceCreated(t *testing.T) {
	event, err := Decode(readFixture(t, "SpaceCreated"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	created, ok := event.Payload.(*SpaceCreated)
	if !ok {
		t.Fatalf("Payload is %T", event.Payload)
	}
	if created.Name == "" || created.Type == "" {
		t.Errorf("Name = %q, Type = %q", created.Name, created.Type)
	}
	if created.Root.Empty() {
		t.Error("Root is empty")
	}
}

// ItemPurged leaves ID empty and carries the ids of the purged item in
// Ref.ResourceID instead. Taking them from ID would delete nothing.
func TestDecodeItemPurgedCarriesItsIDsInRef(t *testing.T) {
	event, err := Decode(readFixture(t, "ItemPurged"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	purged, ok := event.Payload.(*ItemPurged)
	if !ok {
		t.Fatalf("Payload is %T", event.Payload)
	}
	if !purged.ID.Empty() {
		t.Errorf("ID = %+v, the platform leaves it empty", purged.ID)
	}
	if purged.Ref == nil || purged.Ref.ResourceID.Empty() {
		t.Fatal("Ref carries no resource id")
	}
	if purged.Ref.ResourceID.OpaqueID == "" || purged.Ref.SpaceID() == "" {
		t.Errorf("Ref.ResourceID = %+v, want both the item and the space id", purged.Ref.ResourceID)
	}
}

// The path of a reference comes in two shapes: relative to the root of the
// space after an upload, rooted after a move. Whatever resolves a path has to
// take both.
func TestDecodeReferencePathShapes(t *testing.T) {
	ready, err := Decode(readFixture(t, "UploadReady"))
	if err != nil {
		t.Fatalf("Decode UploadReady: %v", err)
	}
	upload, ok := ready.Payload.(*UploadReady)
	if !ok {
		t.Fatalf("Payload is %T", ready.Payload)
	}
	if got := upload.FileRef.Path; got[0] != '.' {
		t.Errorf("UploadReady path = %q, want the relative form", got)
	}

	movedEvent, err := Decode(readFixture(t, "ItemMoved"))
	if err != nil {
		t.Fatalf("Decode ItemMoved: %v", err)
	}
	moved, ok := movedEvent.Payload.(*ItemMoved)
	if !ok {
		t.Fatalf("Payload is %T", movedEvent.Payload)
	}
	if moved.OldReference == nil || moved.OldReference.Path == "" {
		t.Error("OldReference has no path, a rename cannot be reported")
	}
	if moved.Ref == nil || moved.Ref.Path == "" {
		t.Fatal("Ref has no path")
	}
	if moved.Ref.Path == moved.OldReference.Path {
		t.Error("Ref and OldReference hold the same path")
	}
}

func TestDecodeFileUploaded(t *testing.T) {
	event, err := Decode(readFixture(t, "FileUploaded"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := event.Payload.(*FileUploaded); !ok {
		t.Fatalf("Payload is %T", event.Payload)
	}
}

// An event type the extensions do not handle is acknowledged and counted, not
// treated as a failure.
func TestDecodeUnknownTypeIsNotAnError(t *testing.T) {
	for _, name := range []string{"SendSSE", "BytesReceived", "PostprocessingFinished"} {
		event, err := Decode(readFixture(t, name))
		if err != nil {
			t.Errorf("Decode(%s): %v", name, err)
			continue
		}
		if event.Known() {
			t.Errorf("Decode(%s): payload is %T, want none", name, event.Payload)
		}
		if event.Type != "events."+name {
			t.Errorf("Decode(%s): Type = %q", name, event.Type)
		}
		if event.ID == "" {
			t.Errorf("Decode(%s): ID is empty", name)
		}
	}
}

func TestDecodeBadInput(t *testing.T) {
	if _, err := Decode([]byte("not json")); !errors.Is(err, ErrMalformedEnvelope) {
		t.Errorf("Decode of garbage: %v, want ErrMalformedEnvelope", err)
	}
	if _, err := Decode([]byte(`{"Metadata":{}}`)); !errors.Is(err, ErrMissingType) {
		t.Errorf("Decode without a type: %v, want ErrMissingType", err)
	}
}

// The structs of the platform have no json tags, so a renamed or added field
// shows up as a difference in the top level keys of the payload. Comparing
// them turns a silent misdecode into a failing test.
func TestFixturesHaveTheSameFieldsAsTheStructs(t *testing.T) {
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		message := readFixture(t, name[:len(name)-len(".json")])

		event, err := Decode(message)
		if err != nil || !event.Known() {
			continue
		}
		checked++

		var envelope Envelope
		if err := json.Unmarshal(message, &envelope); err != nil {
			t.Fatalf("%s: unmarshal envelope: %v", name, err)
		}

		want := topLevelKeys(t, name, envelope.Payload)
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("%s: marshal payload: %v", name, err)
		}
		got := topLevelKeys(t, name, encoded)

		if !slices.Equal(want, got) {
			t.Errorf("%s: fields of %T drifted\n platform: %v\n ours:     %v", name, event.Payload, want, got)
		}
	}

	if checked == 0 {
		t.Fatal("no fixture of a known type, the golden tests check nothing")
	}
}

func topLevelKeys(t *testing.T, fixture string, payload []byte) []string {
	t.Helper()

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("%s: unmarshal payload: %v", fixture, err)
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// The envelope wraps the event JSON in base64, inside a JSON string.
func TestEnvelopePayloadIsBase64(t *testing.T) {
	var envelope Envelope
	if err := json.Unmarshal(readFixture(t, "UploadReady"), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.Topic != MainQueue {
		t.Errorf("Topic = %q, want %q", envelope.Topic, MainQueue)
	}

	var raw struct {
		Payload string `json:"Payload"`
	}
	if err := json.Unmarshal(readFixture(t, "UploadReady"), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw.Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(decoded) != string(envelope.Payload) {
		t.Error("encoding/json decoded the payload differently than base64")
	}
}
