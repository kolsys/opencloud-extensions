package webhook

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"seq":1}`)
	signature := Sign("secret", body)

	if signature[:7] != "sha256=" || len(signature) != 7+64 {
		t.Errorf("signature = %q", signature)
	}
	if !Verify("secret", body, signature) {
		t.Error("a valid signature was rejected")
	}
	if Verify("other", body, signature) {
		t.Error("a signature under another secret was accepted")
	}
	if Verify("secret", []byte(`{"seq":2}`), signature) {
		t.Error("a signature of another body was accepted")
	}
}

func newPusher(url string, secret config.Secret) *Pusher {
	return New(nil, nil, url, secret, "test", "file-activity/test", metrics.New(prometheus.NewRegistry()), slog.New(slog.DiscardHandler))
}

func TestDeliverPostsASignedEntry(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer endpoint.Close()

	code, err := newPusher(endpoint.URL, "secret").deliver(t.Context(), feed.Event{Seq: 42, ID: "e", Type: feed.Moved, TS: time.Now()})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if code != http.StatusAccepted {
		t.Errorf("code = %d", code)
	}
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get(SeqHeader) != "42" {
		t.Errorf("headers = %v", gotHeader)
	}
	if !Verify("secret", gotBody, gotHeader.Get(SignatureHeader)) {
		t.Error("the signature does not match the body the endpoint received")
	}
	if len(gotBody) == 0 || gotBody[0] != '{' {
		t.Errorf("body = %q", gotBody)
	}
}

func TestDeliverWithoutASecretSendsNoSignature(t *testing.T) {
	var gotHeader http.Header
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
	}))
	defer endpoint.Close()

	if _, err := newPusher(endpoint.URL, "").deliver(t.Context(), feed.Event{Seq: 1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if gotHeader.Get(SignatureHeader) != "" {
		t.Error("a signature was sent without a secret")
	}
}

// Anything but a 2xx is a failure the consumer retries.
func TestDeliverReportsRejections(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()

	code, err := newPusher(endpoint.URL, "").deliver(t.Context(), feed.Event{Seq: 1})
	if err == nil {
		t.Fatal("a 500 was taken as a delivery")
	}
	if code != http.StatusInternalServerError || calls.Load() != 1 {
		t.Errorf("code = %d, calls = %d", code, calls.Load())
	}

	code, err = newPusher("http://127.0.0.1:1", "").deliver(t.Context(), feed.Event{Seq: 1})
	if err == nil || code != 0 {
		t.Errorf("unreachable endpoint: code = %d, err = %v", code, err)
	}
}
