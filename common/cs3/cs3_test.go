package cs3

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// probeName is the file the upload check of the stand leaves in a personal
// space; the tests that need real bytes read it.
const probeName = "probe.bin"

// The tests run against the gateway of a stand. They are skipped when
// OC_GATEWAY_GRPC_ADDR is unset, so that go test stays hermetic.
func connect(t *testing.T) *Client {
	t.Helper()

	addr := os.Getenv("OC_GATEWAY_GRPC_ADDR")
	if addr == "" {
		t.Skip("OC_GATEWAY_GRPC_ADDR is unset, no stand to talk to")
	}

	client, err := New(Config{
		GatewayAddr:          addr,
		ServiceAccountID:     os.Getenv("OC_SERVICE_ACCOUNT_ID"),
		ServiceAccountSecret: os.Getenv("OC_SERVICE_ACCOUNT_SECRET"),
		Insecure:             true,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPingAuthenticatesTheServiceAccount(t *testing.T) {
	if err := connect(t).Ping(testContext(t)); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestListSpaces(t *testing.T) {
	spaces, err := connect(t).ListSpaces(testContext(t))
	if err != nil {
		t.Fatalf("ListSpaces: %v", err)
	}
	if len(spaces) == 0 {
		t.Fatal("the service account sees no space at all")
	}

	for _, space := range spaces {
		if space.Root.SpaceID == "" || space.Root.StorageID == "" {
			t.Errorf("space %q has an incomplete root: %+v", space.Name, space.Root)
		}
	}
}

// Every reaction to an event starts this way: the space root plus the path
// from the event, resolved into a fileid and an etag.
func TestStatResolvesAPathIntoAnID(t *testing.T) {
	client := connect(t)
	ctx := testContext(t)

	space := personalSpace(t, client)
	root, err := client.Stat(ctx, space.Root)
	if err != nil {
		t.Fatalf("Stat of the space root: %v", err)
	}
	if !root.IsDir {
		t.Error("the root of a space is not reported as a container")
	}
	if root.ID.OpaqueID == "" {
		t.Error("the root has no id")
	}

	_, info := probeFile(t, client)
	if info.ID.OpaqueID == "" {
		t.Error("the file has no id, a thumbnail cannot be keyed")
	}
	if info.ETag == "" {
		t.Error("the file has no etag, a thumbnail cannot be keyed")
	}
	if info.Size == 0 {
		t.Error("the file is reported as empty")
	}
	if info.MimeType == "" {
		t.Error("the file has no mime type, video cannot be told apart")
	}
	if info.IsDir {
		t.Error("a file is reported as a container")
	}
	if info.MTime.IsZero() {
		t.Error("the file has no mtime")
	}
}

// A file that is gone while its event waited in the queue is a skip, not a
// failure: the handlers branch on this sentinel.
func TestStatOfAMissingFileIsNotFound(t *testing.T) {
	client := connect(t)

	ref := personalSpace(t, client).Root
	ref.Path = "./there-is-no-such-file-here.bin"

	if _, err := client.Stat(testContext(t), ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat: %v, want ErrNotFound", err)
	}
}

func TestDownload(t *testing.T) {
	client := connect(t)
	ctx := testContext(t)

	ref, info := probeFile(t, client)

	body, ranged, err := client.Download(ctx, ref, 0, 0)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer body.Close()

	if ranged {
		t.Error("a request without a range was answered with 206")
	}
	read, err := io.Copy(io.Discard, body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if uint64(read) != info.Size {
		t.Errorf("read %d bytes, Stat reported %d", read, info.Size)
	}
}

// Whether the data server honours a range decides how the thumbnail worker
// reads a file: ffmpeg over HTTP, or a full copy into a temporary file first.
func TestDownloadRange(t *testing.T) {
	client := connect(t)
	ctx := testContext(t)

	ref, _ := probeFile(t, client)

	const want = 1024
	body, ranged, err := client.Download(ctx, ref, 0, want)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer body.Close()

	read, err := io.Copy(io.Discard, body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}

	t.Logf("range honoured: %t, %d bytes of the %d asked for", ranged, read, want)
	if ranged && read != want {
		t.Errorf("the data server answered 206 but sent %d bytes", read)
	}

	supported, err := client.SupportsRange(ctx, ref)
	if err != nil {
		t.Fatalf("SupportsRange: %v", err)
	}
	if supported != ranged {
		t.Errorf("SupportsRange says %t, the download said %t", supported, ranged)
	}
}

func TestNewNeedsAServiceAccount(t *testing.T) {
	if _, err := New(Config{GatewayAddr: "opencloud:9142"}, slog.Default()); !errors.Is(err, ErrNoServiceAccount) {
		t.Errorf("New without a service account: %v, want ErrNoServiceAccount", err)
	}
}

func TestTransient(t *testing.T) {
	refused := status.Error(codes.Unavailable, "connection error: connection refused")
	for _, err := range []error{refused, wrap("stat", Ref{}, refused), status.Error(codes.DeadlineExceeded, "slow")} {
		if !Transient(err) {
			t.Errorf("%v: not transient", err)
		}
	}
	for _, err := range []error{nil, ErrNotFound, errors.New("plain"), status.Error(codes.NotFound, "gone"), status.Error(codes.PermissionDenied, "no")} {
		if Transient(err) {
			t.Errorf("%v: transient", err)
		}
	}
}

// The stand has one personal space per demo user and ListSpaces does not
// promise an order, so a test that needs a particular file looks for the
// space that holds it.
func personalSpace(t *testing.T, client *Client) Space {
	t.Helper()

	spaces := personalSpaces(t, client)
	if len(spaces) == 0 {
		t.Skip("the stand has no personal space")
	}
	return spaces[0]
}

func personalSpaces(t *testing.T, client *Client) []Space {
	t.Helper()

	spaces, err := client.ListSpaces(testContext(t))
	if err != nil {
		t.Fatalf("ListSpaces: %v", err)
	}

	personal := make([]Space, 0, len(spaces))
	for _, space := range spaces {
		if space.Type == "personal" {
			personal = append(personal, space)
		}
	}
	slices.SortFunc(personal, func(a, b Space) int { return strings.Compare(a.Root.SpaceID, b.Root.SpaceID) })
	return personal
}

// probeFile returns a reference to the file the upload checks of the stand
// leave behind, in whichever personal space holds it.
func probeFile(t *testing.T, client *Client) (Ref, *ResourceInfo) {
	t.Helper()

	for _, space := range personalSpaces(t, client) {
		ref := space.Root
		ref.Path = "./" + probeName

		info, err := client.Stat(testContext(t), ref)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			t.Fatalf("Stat: %v", err)
		}
		return ref, info
	}

	t.Skipf("%s is on no personal space of this stand, upload it first", probeName)
	return Ref{}, nil
}
