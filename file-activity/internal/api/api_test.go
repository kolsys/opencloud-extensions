package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/httpx"
	feederrors "github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

// fakeFeed holds a window of the feed: entries first..last, of which only
// those from firstAvailable on are still readable.
type fakeFeed struct {
	firstAvailable, last uint64
}

func (f *fakeFeed) Head(context.Context) (feed.Head, error) {
	return feed.Head{FirstAvailable: f.firstAvailable, Last: f.last}, nil
}

func (f *fakeFeed) Read(_ context.Context, since uint64, limit int) (feed.Page, error) {
	page := feed.Page{Events: []feed.Event{}, Next: since, FirstAvailable: f.firstAvailable, Last: f.last}
	if since+1 < f.firstAvailable {
		return page, feederrors.ErrCursorTooOld
	}
	for seq := since + 1; seq <= f.last && len(page.Events) < limit; seq++ {
		page.Events = append(page.Events, feed.Event{Seq: seq, ID: "event", Type: feed.FileCreated})
		page.Next = seq
	}
	return page, nil
}

// fakeIdentifier maps a credential to a user name, and counts the calls.
type fakeIdentifier struct {
	users map[string]string
	calls int
}

func (f *fakeIdentifier) Whoami(_ context.Context, header http.Header) (*httpx.Identity, error) {
	f.calls++
	user, ok := f.users[header.Get("Authorization")]
	if !ok {
		return nil, httpx.StatusError{Code: http.StatusUnauthorized}
	}
	return &httpx.Identity{Username: user}, nil
}

func newHandler(t *testing.T, store Feed, identifier Identifier, allowed []string) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	m := metrics.New(prometheus.NewRegistry())
	New(store, identifier, allowed, m, slog.New(slog.DiscardHandler)).Register(mux)
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

func TestFeedPages(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 1, last: 5}, nil, nil)

	recorder, body := get(t, mux, RouteFeed+"?since=0&limit=2", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", recorder.Code, recorder.Body.String())
	}
	events, _ := body["events"].([]any)
	if len(events) != 2 || body["next"] != float64(2) || body["last"] != float64(5) || body["first_available"] != float64(1) {
		t.Errorf("page = %v", body)
	}

	// Reading again with the same cursor gives the same page.
	again, bodyAgain := get(t, mux, RouteFeed+"?since=0&limit=2", "")
	if again.Body.String() != recorder.Body.String() {
		t.Errorf("the same cursor gave a different page:\n%v\n%v", body, bodyAgain)
	}

	// A caught up client gets an empty page and keeps its cursor.
	_, tail := get(t, mux, RouteFeed+"?since=5", "")
	if events, _ := tail["events"].([]any); len(events) != 0 || tail["next"] != float64(5) {
		t.Errorf("tail = %v", tail)
	}
}

// Silently starting at the first available event would hide the gap: the
// client is told to resync instead.
func TestFeedCursorTooOldIsGone(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 100, last: 200}, nil, nil)

	recorder, body := get(t, mux, RouteFeed+"?since=3", "")
	if recorder.Code != http.StatusGone {
		t.Fatalf("code = %d, want 410", recorder.Code)
	}
	if body["first_available"] != float64(100) {
		t.Errorf("body = %v, want first_available", body)
	}

	// since = first_available - 1 is the last cursor that still works.
	edge, _ := get(t, mux, RouteFeed+"?since=99", "")
	if edge.Code != http.StatusOK {
		t.Errorf("since one before the first available: code = %d", edge.Code)
	}
}

func TestFeedRejectsBadParameters(t *testing.T) {
	mux := newHandler(t, &fakeFeed{last: 1}, nil, nil)
	for _, query := range []string{"?since=abc", "?since=-1", "?limit=0", "?limit=1001", "?limit=x"} {
		if recorder, _ := get(t, mux, RouteFeed+query, ""); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", query, recorder.Code)
		}
	}
}

func TestHead(t *testing.T) {
	mux := newHandler(t, &fakeFeed{firstAvailable: 7, last: 42}, nil, nil)
	recorder, body := get(t, mux, RouteHead, "")
	if recorder.Code != http.StatusOK || body["first_available"] != float64(7) || body["last"] != float64(42) {
		t.Errorf("head: %d %v", recorder.Code, body)
	}
}

func TestAllowedUsers(t *testing.T) {
	identifier := &fakeIdentifier{users: map[string]string{"Basic alan": "alan", "Basic mary": "mary"}}
	mux := newHandler(t, &fakeFeed{last: 1}, identifier, []string{"alan"})

	if recorder, _ := get(t, mux, RouteHead, "Basic alan"); recorder.Code != http.StatusOK {
		t.Errorf("allowed user: code = %d", recorder.Code)
	}
	if recorder, _ := get(t, mux, RouteHead, "Basic mary"); recorder.Code != http.StatusForbidden {
		t.Errorf("user not on the list: code = %d, want 403", recorder.Code)
	}
	if recorder, _ := get(t, mux, RouteHead, "Basic nobody"); recorder.Code != http.StatusUnauthorized {
		t.Errorf("unknown credential: code = %d, want the answer of the platform", recorder.Code)
	}

	// The platform is asked once per credential, not once per request.
	calls := identifier.calls
	for range 5 {
		get(t, mux, RouteHead, "Basic alan")
	}
	if identifier.calls != calls {
		t.Errorf("the platform was asked %d more times for a cached credential", identifier.calls-calls)
	}
}

func TestEmptyAllowedListMeansEveryone(t *testing.T) {
	identifier := &fakeIdentifier{users: map[string]string{}}
	mux := newHandler(t, &fakeFeed{last: 1}, identifier, nil)
	if recorder, _ := get(t, mux, RouteHead, "Basic whoever"); recorder.Code != http.StatusOK {
		t.Errorf("code = %d", recorder.Code)
	}
	if identifier.calls != 0 {
		t.Error("the platform was asked although every user is allowed")
	}
}

type brokenFeed struct{}

func (brokenFeed) Head(context.Context) (feed.Head, error) {
	return feed.Head{}, errors.New("nats down")
}
func (brokenFeed) Read(context.Context, uint64, int) (feed.Page, error) {
	return feed.Page{}, errors.New("nats down")
}

func TestFeedUnavailable(t *testing.T) {
	mux := newHandler(t, brokenFeed{}, nil, nil)
	if recorder, _ := get(t, mux, RouteFeed, ""); recorder.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", recorder.Code)
	}
}
