package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/httpx"
	feederrors "github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

// fakeFeed holds a window of the feed: entries first..last, of which only
// those from firstAvailable on are still readable. Entry n is in space
// spaces[n-1], or in "s" when no spaces are given.
type fakeFeed struct {
	firstAvailable, last uint64
	spaces               []string
	scopes               []feed.Scope
}

func (f *fakeFeed) Head(context.Context) (feed.Head, error) {
	return feed.Head{FirstAvailable: f.firstAvailable, Last: f.last}, nil
}

func (f *fakeFeed) Read(_ context.Context, since uint64, limit int, scope feed.Scope) (feed.Page, error) {
	f.scopes = append(f.scopes, scope)

	page := feed.Page{Events: []feed.Event{}, Next: since, FirstAvailable: f.firstAvailable, Last: f.last}
	if since+1 < f.firstAvailable {
		return page, feederrors.ErrCursorTooOld
	}
	for seq := since + 1; seq <= f.last && len(page.Events) < limit; seq++ {
		space := "s"
		if f.spaces != nil {
			space = f.spaces[seq-1]
		}
		if !scope.All() && !slices.Contains(scope.Spaces(), space) {
			continue
		}
		page.Events = append(page.Events, feed.Event{Seq: seq, ID: "event", Type: feed.FileCreated, SpaceID: space})
		page.Next = seq
	}
	return page, nil
}

// fakePlatform maps a credential to a user and a user to its drives, and
// counts the questions.
type fakePlatform struct {
	users          map[string]string
	drives         map[string][]httpx.Drive
	whoami, listed int
}

// alan is a member of space s, the only one entries of a fakeFeed without
// spaces are in.
func alan() *fakePlatform {
	return &fakePlatform{
		users:  map[string]string{"Basic alan": "alan"},
		drives: map[string][]httpx.Drive{"alan": {{ID: "st$s", SpaceID: "s", Name: "Alan Turing", Type: httpx.DrivePersonal}}},
	}
}

func (f *fakePlatform) Whoami(_ context.Context, header http.Header) (*httpx.Identity, error) {
	f.whoami++
	user, ok := f.users[header.Get("Authorization")]
	if !ok {
		return nil, httpx.StatusError{Code: http.StatusUnauthorized}
	}
	return &httpx.Identity{Username: user}, nil
}

func (f *fakePlatform) Drives(_ context.Context, header http.Header) ([]httpx.Drive, error) {
	f.listed++
	user, ok := f.users[header.Get("Authorization")]
	if !ok {
		return nil, httpx.StatusError{Code: http.StatusUnauthorized}
	}
	return f.drives[user], nil
}

func newHandler(t *testing.T, store Feed, platform Platform, access Access) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	m := metrics.New(prometheus.NewRegistry())
	New(store, platform, access, m, slog.New(slog.DiscardHandler)).Register(mux)
	return mux
}

func get(t *testing.T, mux *http.ServeMux, target, authorization string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	body := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body is not JSON: %s", target, recorder.Body.String())
		}
	}
	return recorder, body
}

func spacesOfEvents(body map[string]any) []string {
	events, _ := body["events"].([]any)
	out := make([]string, 0, len(events))
	for _, event := range events {
		entry, _ := event.(map[string]any)
		space, _ := entry["space_id"].(string)
		out = append(out, space)
	}
	return out
}

func TestFeedPages(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 1, last: 5}, alan(), Access{})

	recorder, body := get(t, mux, RouteFeed+"?since=0&limit=2", "Basic alan")
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	events, _ := body["events"].([]any)
	if len(events) != 2 || body["next"] != float64(2) || body["last"] != float64(5) || body["first_available"] != float64(1) {
		t.Errorf("page = %v", body)
	}

	// Reading again with the same cursor gives the same page.
	again, bodyAgain := get(t, mux, RouteFeed+"?since=0&limit=2", "Basic alan")
	if again.Body.String() != recorder.Body.String() {
		t.Errorf("the same cursor gave a different page:\n%v\n%v", body, bodyAgain)
	}

	// A caught up client gets an empty page.
	_, tail := get(t, mux, RouteFeed+"?since=5", "Basic alan")
	if events, _ := tail["events"].([]any); len(events) != 0 || tail["next"] != float64(5) {
		t.Errorf("tail = %v", tail)
	}
}

// Silently starting at the first available event would hide the gap: the
// client is told to resync instead.
func TestFeedCursorTooOldIsGone(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 100, last: 200}, alan(), Access{})

	recorder, body := get(t, mux, RouteFeed+"?since=3", "Basic alan")
	if recorder.Code != http.StatusGone {
		t.Fatalf("code = %d, want 410", recorder.Code)
	}
	if body["first_available"] != float64(100) {
		t.Errorf("body = %v, want first_available", body)
	}

	// since = first_available - 1 is the last cursor that still works.
	edge, _ := get(t, mux, RouteFeed+"?since=99", "Basic alan")
	if edge.Code != http.StatusOK {
		t.Errorf("since one before the first available: code = %d", edge.Code)
	}
}

func TestFeedRejectsBadParameters(t *testing.T) {
	mux := newHandler(t, &fakeFeed{last: 1}, alan(), Access{})
	for _, query := range []string{"?since=abc", "?since=-1", "?limit=0", "?limit=1001", "?limit=x"} {
		if recorder, _ := get(t, mux, RouteFeed+query, "Basic alan"); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", query, recorder.Code)
		}
	}
}

// A member reads the spaces it is a member of and is told which ones they
// are; a share it received is not one of them.
func TestFeedOfTheSpacesOfTheCaller(t *testing.T) {
	store := &fakeFeed{firstAvailable: 1, last: 5, spaces: []string{"home", "other", "project", "shared", "home"}}
	platform := &fakePlatform{
		users: map[string]string{"Basic alan": "alan"},
		drives: map[string][]httpx.Drive{"alan": {
			{ID: "st$home", SpaceID: "home", Name: "Alan Turing", Type: httpx.DrivePersonal},
			{ID: "st$project", SpaceID: "project", Name: "Fixtures", Type: httpx.DriveProject},
			{ID: "shares$shared!mount", SpaceID: "shared", Name: "clip.mp4", Type: "mountpoint"},
		}},
	}
	mux := newHandler(t, store, platform, Access{})

	recorder, body := get(t, mux, RouteFeed, "Basic alan")
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := spacesOfEvents(body); !slices.Equal(got, []string{"home", "project", "home"}) {
		t.Errorf("events are in %v", got)
	}

	spaces, _ := body["spaces"].([]any)
	want := []map[string]any{
		{"id": "home", "name": "Alan Turing", "type": "personal"},
		{"id": "project", "name": "Fixtures", "type": "project"},
	}
	if len(spaces) != len(want) {
		t.Fatalf("spaces = %v", body["spaces"])
	}
	for i := range want {
		got, _ := spaces[i].(map[string]any)
		if len(got) != len(want[i]) || got["id"] != want[i]["id"] || got["name"] != want[i]["name"] || got["type"] != want[i]["type"] {
			t.Errorf("space %d = %v, want %v", i, got, want[i])
		}
	}
	if platform.whoami != 0 {
		t.Error("the platform was asked who the caller is although no list needs it")
	}
}

// A member of no space reads nothing, and is told so with an empty list.
func TestFeedOfAMemberOfNoSpace(t *testing.T) {
	platform := &fakePlatform{
		users:  map[string]string{"Basic guest": "guest"},
		drives: map[string][]httpx.Drive{"guest": {{ID: "shares$s!mount", SpaceID: "s", Type: "mountpoint"}}},
	}
	mux := newHandler(t, &fakeFeed{firstAvailable: 1, last: 3}, platform, Access{})

	recorder, body := get(t, mux, RouteFeed, "Basic guest")
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d", recorder.Code)
	}
	spaces, ok := body["spaces"].([]any)
	if events, _ := body["events"].([]any); len(events) != 0 || !ok || len(spaces) != 0 {
		t.Errorf("page = %v, want no events and an empty list of spaces", body)
	}
}

// A user on the full list reads every space, allowed or not, without the
// platform being asked for its spaces.
func TestFullFeedUsers(t *testing.T) {
	store := &fakeFeed{firstAvailable: 1, last: 3, spaces: []string{"a", "b", "c"}}
	platform := alan()
	platform.users["Basic service"] = "service"
	mux := newHandler(t, store, platform, Access{Allowed: []string{"alan"}, FullFeed: []string{"service"}})

	recorder, body := get(t, mux, RouteFeed, "Basic service")
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := spacesOfEvents(body); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("events are in %v", got)
	}
	if _, ok := body["spaces"]; ok {
		t.Errorf("a reader of the whole feed got spaces %v", body["spaces"])
	}
	if platform.listed != 0 {
		t.Error("the platform was asked for the spaces of a reader of the whole feed")
	}
	if !store.scopes[0].All() {
		t.Errorf("scope = %+v, want the whole feed", store.scopes[0])
	}

	// Everyone else still reads their own spaces only.
	if _, body := get(t, mux, RouteFeed, "Basic alan"); len(spacesOfEvents(body)) != 0 {
		t.Errorf("alan read %v", spacesOfEvents(body))
	}
}

// The platform is asked once per credential, and the spaces are fetched for
// a read even when the credential was seen on the head before.
func TestCallerIsCached(t *testing.T) {
	platform := alan()
	mux := newHandler(t, &fakeFeed{firstAvailable: 1, last: 1}, platform, Access{Allowed: []string{"alan"}})

	get(t, mux, RouteHead, "Basic alan")
	if platform.whoami != 1 || platform.listed != 0 {
		t.Errorf("head: whoami %d, drives %d", platform.whoami, platform.listed)
	}
	for range 5 {
		if recorder, _ := get(t, mux, RouteFeed, "Basic alan"); recorder.Code != http.StatusOK {
			t.Fatalf("code = %d", recorder.Code)
		}
	}
	if platform.listed != 1 {
		t.Errorf("the spaces were listed %d times", platform.listed)
	}
	whoami := platform.whoami
	get(t, mux, RouteHead, "Basic alan")
	if platform.whoami != whoami {
		t.Error("a head after a read asked the platform again")
	}
}

func TestHead(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 7, last: 42}, nil, Access{})
	recorder, body := get(t, mux, RouteHead, "")
	if recorder.Code != http.StatusOK || body["first_available"] != float64(7) || body["last"] != float64(42) {
		t.Errorf("head: %d %v", recorder.Code, body)
	}
}

func TestAllowedUsers(t *testing.T) {
	platform := &fakePlatform{users: map[string]string{"Basic alan": "alan", "Basic mary": "mary"}}
	mux := newHandler(t, &fakeFeed{last: 1}, platform, Access{Allowed: []string{"alan"}})

	if recorder, _ := get(t, mux, RouteHead, "Basic alan"); recorder.Code != http.StatusOK {
		t.Errorf("allowed user: code = %d", recorder.Code)
	}
	if recorder, _ := get(t, mux, RouteHead, "Basic mary"); recorder.Code != http.StatusForbidden {
		t.Errorf("user not on the list: code = %d, want 403", recorder.Code)
	}
	if recorder, _ := get(t, mux, RouteFeed, "Basic mary"); recorder.Code != http.StatusForbidden {
		t.Errorf("user not on the list reading: code = %d, want 403", recorder.Code)
	}
	if platform.listed != 0 {
		t.Error("the spaces of a user not on the list were asked for")
	}
	if recorder, _ := get(t, mux, RouteHead, "Basic nobody"); recorder.Code != http.StatusUnauthorized {
		t.Errorf("unknown credential: code = %d, want the answer of the platform", recorder.Code)
	}

	// The platform is asked once per credential, not once per request.
	calls := platform.whoami
	for range 5 {
		get(t, mux, RouteHead, "Basic alan")
	}
	if platform.whoami != calls {
		t.Errorf("the platform was asked %d more times for a cached credential", platform.whoami-calls)
	}
}

// Without lists the head trusts the proxy, while a read still needs the
// spaces of the caller and so its credential.
func TestEmptyAllowedListMeansEveryone(t *testing.T) {
	platform := alan()
	mux := newHandler(t, &fakeFeed{last: 1}, platform, Access{})
	if recorder, _ := get(t, mux, RouteHead, "Basic whoever"); recorder.Code != http.StatusOK {
		t.Errorf("code = %d", recorder.Code)
	}
	if platform.whoami != 0 || platform.listed != 0 {
		t.Error("the platform was asked although every user is allowed")
	}
	if recorder, _ := get(t, mux, RouteFeed, "Basic whoever"); recorder.Code != http.StatusUnauthorized {
		t.Errorf("read with a credential the platform rejects: code = %d, want 401", recorder.Code)
	}
}

type brokenFeed struct{}

func (brokenFeed) Head(context.Context) (feed.Head, error) {
	return feed.Head{}, errors.New("nats down")
}
func (brokenFeed) Read(context.Context, uint64, int, feed.Scope) (feed.Page, error) {
	return feed.Page{}, errors.New("nats down")
}

func TestFeedUnavailable(t *testing.T) {
	mux := newHandler(t, brokenFeed{}, alan(), Access{})
	if recorder, _ := get(t, mux, RouteFeed, "Basic alan"); recorder.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", recorder.Code)
	}
}

type brokenPlatform struct{ fakePlatform }

func (brokenPlatform) Drives(context.Context, http.Header) ([]httpx.Drive, error) {
	return nil, errors.New("connection refused")
}

func TestPlatformUnavailable(t *testing.T) {
	mux := newHandler(t, &fakeFeed{last: 1}, &brokenPlatform{}, Access{})
	if recorder, _ := get(t, mux, RouteFeed, "Basic alan"); recorder.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", recorder.Code)
	}
}
