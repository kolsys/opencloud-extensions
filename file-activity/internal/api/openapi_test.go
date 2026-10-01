package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
)

// The description of the API is written by hand, so it is checked against
// the handlers here: a route or a field that changes without the description
// following fails the build rather than reaching a client.
const description = "../../api/openapi.yaml"

type openAPI struct {
	Paths map[string]map[string]struct {
		OperationID string `yaml:"operationId"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Type       any                  `yaml:"type"`
			Required   []string             `yaml:"required"`
			Enum       []string             `yaml:"enum"`
			Properties map[string]yaml.Node `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func load(t *testing.T) openAPI {
	t.Helper()

	body, err := os.ReadFile(description)
	if err != nil {
		t.Fatalf("read the description: %v", err)
	}
	var parsed openAPI
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse the description: %v", err)
	}
	return parsed
}

// keys are the JSON field names a value serialises to.
func keys(t *testing.T, value any) []string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal %T: %v", value, err)
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func properties(t *testing.T, spec openAPI, schema string) []string {
	t.Helper()

	described, ok := spec.Components.Schemas[schema]
	if !ok {
		t.Fatalf("the description has no schema %s", schema)
	}

	names := make([]string, 0, len(described.Properties))
	for name := range described.Properties {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Every path and method the description promises has to be served.
func TestDescribedRoutesAreServed(t *testing.T) {
	spec := load(t)
	mux := newHandler(t, &fakeFeed{firstAvailable: 1, last: 1}, nil, Access{})

	described := 0
	for path, methods := range spec.Paths {
		for method := range methods {
			described++
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if _, pattern := mux.Handler(request); pattern == "" {
				t.Errorf("%s %s is described but not served", method, path)
			}
		}
	}

	if described != 2 {
		t.Errorf("the description holds %d operations, the service serves 2", described)
	}
	for _, route := range []string{RouteFeed, RouteHead} {
		if _, ok := spec.Paths[route]; !ok {
			t.Errorf("%s is served but not described", route)
		}
	}
}

// The fields of an entry are the contract with the client: an added,
// renamed or dropped one has to reach the description.
func TestDescribedFieldsMatchTheStructs(t *testing.T) {
	spec := load(t)
	path, oldPath := "/a.mp4", "/b.mp4"

	// Every optional field is filled in, so that the comparison sees them all.
	now := time.Now().UTC()
	full := feed.Event{
		Seq: 1, ID: "id", TS: now, Type: feed.Moved,
		SpaceID: "space", SpaceName: "Space", FileID: "file", ResourceID: "storage$space!file",
		Path: &path, OldPath: &oldPath, IsDir: false, Size: 1, Mime: "video/mp4", ETag: "etag",
		BlobID: "blob", Checksum: "sha1:00", MTime: &now,
		Actor: &feed.Actor{ID: "user", Name: "alan"},
	}

	for _, tc := range []struct {
		schema string
		value  any
	}{
		{"Event", full},
		{"Actor", feed.Actor{ID: "user", Name: "alan"}},
		{"Page", feed.Page{Events: []feed.Event{}, Spaces: []feed.Space{{ID: "space", Name: "Space", Type: "project"}}}},
		{"Space", feed.Space{ID: "space", Name: "Space", Type: "project"}},
		{"Head", feed.Head{}},
	} {
		if got, want := keys(t, tc.value), properties(t, spec, tc.schema); !slices.Equal(got, want) {
			t.Errorf("%s: the struct has %v, the description has %v", tc.schema, got, want)
		}
	}
}

// A field without omitempty is always sent, so the description has to
// require exactly those.
func TestRequiredFieldsAreTheOnesAlwaysSent(t *testing.T) {
	spec := load(t)

	if got, want := keys(t, feed.Event{}), spec.Components.Schemas["Event"].Required; !slices.Equal(got, sorted(want)) {
		t.Errorf("Event: an empty entry sends %v, the description requires %v", got, want)
	}
	if got, want := keys(t, feed.Page{}), spec.Components.Schemas["Page"].Required; !slices.Equal(got, sorted(want)) {
		t.Errorf("Page: an empty page sends %v, the description requires %v", got, want)
	}
	if got, want := keys(t, feed.Head{}), spec.Components.Schemas["Head"].Required; !slices.Equal(got, sorted(want)) {
		t.Errorf("Head: %v against %v", got, want)
	}
	if got, want := keys(t, feed.Space{}), spec.Components.Schemas["Space"].Required; !slices.Equal(got, sorted(want)) {
		t.Errorf("Space: %v against %v", got, want)
	}
}

// The enum of the description is the set of types the mapper can produce.
func TestDescribedTypesAreTheOnesProduced(t *testing.T) {
	spec := load(t)

	produced := []string{
		string(feed.FileCreated), string(feed.FileUpdated), string(feed.FolderCreated),
		string(feed.Trashed), string(feed.Restored), string(feed.Purged), string(feed.Moved),
		string(feed.SpaceCreated), string(feed.SpaceDeleted), string(feed.SpaceRenamed),
	}
	if got, want := sorted(produced), sorted(spec.Components.Schemas["EventType"].Enum); !slices.Equal(got, want) {
		t.Errorf("the service produces %v, the description lists %v", got, want)
	}
}

// The status codes the handlers answer with have to be described.
func TestDescribedStatusCodes(t *testing.T) {
	body, err := os.ReadFile(description)
	if err != nil {
		t.Fatalf("read the description: %v", err)
	}

	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse the description: %v", err)
	}

	mux := newHandler(t, &fakeFeed{firstAvailable: 100, last: 200}, &fakePlatform{users: map[string]string{}}, Access{Allowed: []string{"nobody"}})
	for _, tc := range []struct {
		target, authorization string
		want                  int
	}{
		{RouteFeed + "?since=150", "Basic alan", http.StatusForbidden},
		{RouteFeed + "?since=0", "Basic alan", http.StatusGone},
		{RouteFeed + "?limit=0", "Basic alan", http.StatusForbidden},
	} {
		recorder, _ := get(t, mux, tc.target, tc.authorization)
		code := strconv.Itoa(recorder.Code)
		if _, ok := spec.Paths[RouteFeed]["get"].Responses[code]; !ok {
			t.Errorf("%s answered %s, which the description does not list", tc.target, code)
		}
	}

	// The ones the handlers can answer, listed here so that a new answer has
	// to be added to both places.
	for _, code := range []string{"200", "400", "401", "403", "410", "502"} {
		if _, ok := spec.Paths[RouteFeed]["get"].Responses[code]; !ok {
			t.Errorf("the description of %s does not list %s", RouteFeed, code)
		}
	}
}

func sorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}
