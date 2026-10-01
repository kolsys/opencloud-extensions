package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The answer has to be the one the platform would send, down to the
// namespaces: the web matches on them.
func TestWriteNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteNotFound(recorder, "clip.mp4")

	if recorder.Code != http.StatusNotFound {
		t.Errorf("code = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`<d:error`,
		`xmlns:d="DAV"`,
		`xmlns:s="http://sabredav.org/ns"`,
		`<s:exception>Sabre\DAV\Exception\NotFound</s:exception>`,
		`<s:message>File with name clip.mp4 could not be located</s:message>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %s:\n%s", want, body)
		}
	}
}

// The platform names no exception for 425, so the element stays empty, and
// the Retry-After is the addition of the extension.
func TestWriteTooEarly(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteTooEarly(recorder, 5)

	if recorder.Code != http.StatusTooEarly {
		t.Errorf("code = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "<s:exception></s:exception>") {
		t.Errorf("exception is not empty:\n%s", body)
	}

	recorder = httptest.NewRecorder()
	WriteTooEarly(recorder, 0)
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After of a zero wait = %q, want 1", got)
	}
}

func TestSplitFileID(t *testing.T) {
	storage, space, opaque := splitFileID("904763f4-a03c$b1f74ec4-dd7e!fd142f17-47bc")
	if storage != "904763f4-a03c" || space != "b1f74ec4-dd7e" || opaque != "fd142f17-47bc" {
		t.Errorf("split = %q, %q, %q", storage, space, opaque)
	}

	// A bare id without a storage part still yields the two halves.
	storage, space, opaque = splitFileID("b1f74ec4-dd7e!fd142f17-47bc")
	if storage != "" || space != "b1f74ec4-dd7e" || opaque != "fd142f17-47bc" {
		t.Errorf("split without a storage = %q, %q, %q", storage, space, opaque)
	}
}

func TestParsePropFind(t *testing.T) {
	answer := []byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response>
  <d:href>/remote.php/webdav/clip.mp4</d:href>
  <d:propstat>
   <d:prop>
    <oc:fileid>904763f4$b1f74ec4!fd142f17</oc:fileid>
    <d:getetag>"22f357f48a44d564"</d:getetag>
    <d:getcontenttype>video/mp4</d:getcontenttype>
    <d:getcontentlength>2000000</d:getcontentlength>
    <d:resourcetype></d:resourcetype>
   </d:prop>
   <d:status>HTTP/1.1 200 OK</d:status>
  </d:propstat>
  <d:propstat>
   <d:prop><oc:favorite/></d:prop>
   <d:status>HTTP/1.1 404 Not Found</d:status>
  </d:propstat>
 </d:response>
</d:multistatus>`)

	resource, err := parsePropFind(answer)
	if err != nil {
		t.Fatalf("parsePropFind: %v", err)
	}
	if resource.ETag != "22f357f48a44d564" {
		t.Errorf("ETag = %q, the quotes were not stripped", resource.ETag)
	}
	if resource.ContentType != "video/mp4" || resource.Size != 2000000 {
		t.Errorf("resource = %+v", resource)
	}
	if resource.SpaceID != "b1f74ec4" || resource.OpaqueID != "fd142f17" {
		t.Errorf("ids = %q, %q", resource.SpaceID, resource.OpaqueID)
	}
	if resource.IsDir {
		t.Error("a file is reported as a collection")
	}

	if _, err := parsePropFind([]byte(`<d:multistatus xmlns:d="DAV:"></d:multistatus>`)); err == nil {
		t.Error("an empty multistatus was accepted")
	}
}

// A file in postprocessing carries its properties under a 425 status, the
// way reva reports it; the client has to ask again later.
func TestParsePropFindOfAFileInProcessing(t *testing.T) {
	answer := []byte(`<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response>
  <d:href>/dav/spaces/904763f4$b1f74ec4/clip.mp4</d:href>
  <d:propstat>
   <d:prop>
    <oc:fileid>904763f4$b1f74ec4!fd142f17</oc:fileid>
    <d:getetag>"22f357f48a44d564"</d:getetag>
    <d:getcontenttype>video/mp4</d:getcontenttype>
   </d:prop>
   <d:status>HTTP/1.1 425 TOO EARLY</d:status>
  </d:propstat>
 </d:response>
</d:multistatus>`)

	_, err := parsePropFind(answer)
	if code, ok := Status(err); !ok || code != http.StatusTooEarly {
		t.Errorf("parsePropFind = %v, want a 425 status error", err)
	}
}

// The path of the request arrives decoded and must reach the platform
// escaped again, or a name with a percent sign or a hash is cut short.
func TestPropFindEscapesThePath(t *testing.T) {
	var seen string
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:propstat><d:prop><oc:fileid>s$sp!f</oc:fileid></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
	}))
	defer platform.Close()

	client, err := NewClient(platform.URL+"/", false)
	if err != nil {
		t.Fatal(err)
	}
	name := "/dav/spaces/s$sp/100% done #1, (final)?.mp4"
	if _, err := client.PropFind(context.Background(), name, http.Header{}); err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	if seen != name {
		t.Errorf("the platform saw %q, want %q", seen, name)
	}
}

// The listing comes with the credentials of the client, and the id of a
// space is cut down to the part the events carry.
func TestDrives(t *testing.T) {
	var seen *http.Request
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		if r.Header.Get("Authorization") != "Basic alan" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"value":[
			{"id":"storage$personal-space","name":"Alan Turing","driveType":"personal","quota":{"total":1}},
			{"id":"storage$project-space","name":"Fixtures","driveType":"project"},
			{"id":"shares$share!mount","name":"clip.mp4","driveType":"mountpoint"}
		]}`))
	}))
	defer platform.Close()

	client, err := NewClient(platform.URL, false)
	if err != nil {
		t.Fatal(err)
	}

	header := http.Header{}
	header.Set("Authorization", "Basic alan")
	header.Set("X-Other", "not forwarded")
	drives, err := client.Drives(context.Background(), header)
	if err != nil {
		t.Fatalf("Drives: %v", err)
	}
	if seen.URL.Path != drivesPath || seen.Header.Get("X-Other") != "" {
		t.Errorf("the platform saw %s with %v", seen.URL.Path, seen.Header)
	}
	want := []Drive{
		{ID: "storage$personal-space", SpaceID: "personal-space", Name: "Alan Turing", Type: DrivePersonal},
		{ID: "storage$project-space", SpaceID: "project-space", Name: "Fixtures", Type: DriveProject},
		{ID: "shares$share!mount", SpaceID: "share", Name: "clip.mp4", Type: "mountpoint"},
	}
	if len(drives) != len(want) {
		t.Fatalf("drives = %+v", drives)
	}
	for i := range want {
		if drives[i] != want[i] {
			t.Errorf("drive %d = %+v, want %+v", i, drives[i], want[i])
		}
	}

	if _, err := client.Drives(context.Background(), http.Header{}); err != nil {
		if code, ok := Status(err); !ok || code != http.StatusUnauthorized {
			t.Errorf("Drives without a credential: %v", err)
		}
	} else {
		t.Error("Drives without a credential succeeded")
	}
}

// A caller without spaces gets null from the platform, not an error.
func TestDrivesOfNobody(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`null`))
	}))
	defer platform.Close()

	client, err := NewClient(platform.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	drives, err := client.Drives(context.Background(), http.Header{})
	if err != nil || len(drives) != 0 {
		t.Errorf("Drives = %v, %v", drives, err)
	}
}

// Everything the client sent has to reach the platform unchanged.
func TestProxyPassesTheRequestThrough(t *testing.T) {
	var got *http.Request
	var body string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		buffer := make([]byte, 64)
		n, _ := r.Body.Read(buffer)
		body = string(buffer[:n])
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("jpeg bytes"))
	}))
	defer upstream.Close()

	proxy, err := NewProxy(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/dav/spaces/space!file?preview=1&x=32&y=32", strings.NewReader("hello"))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)

	if got == nil {
		t.Fatal("the upstream was not called")
	}
	if got.URL.Path != "/dav/spaces/space!file" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if got.URL.RawQuery != "preview=1&x=32&y=32" {
		t.Errorf("query = %q", got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer token" {
		t.Error("the authorization header was dropped")
	}
	if body != "hello" {
		t.Errorf("body = %q", body)
	}
	if recorder.Header().Get("ETag") != `"abc"` || recorder.Body.String() != "jpeg bytes" {
		t.Errorf("the answer was not passed back: %v %q", recorder.Header(), recorder.Body.String())
	}
}

func TestProxyReportsAnUnreachablePlatform(t *testing.T) {
	proxy, err := NewProxy("http://127.0.0.1:1", discardLogger())
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dav/x", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "<d:error") {
		t.Errorf("the error is not in the format of the platform: %s", recorder.Body.String())
	}
}

func TestNewProxyRejectsABadUpstream(t *testing.T) {
	if _, err := NewProxy("opencloud:9115", discardLogger()); err == nil {
		t.Error("an upstream without a scheme was accepted")
	}
}

// Against a stand: the PROPFIND is the authorisation of every preview, so a
// wrong credential has to come back as a status the caller forwards.
func TestPropFindAgainstTheStand(t *testing.T) {
	baseURL, user, token := os.Getenv("PLATFORM_INTERNAL_URL"), os.Getenv("STAND_USER"), os.Getenv("STAND_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("PLATFORM_INTERNAL_URL and STAND_TOKEN are unset, no stand to talk to")
	}

	client, err := NewClient(baseURL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	authorized := http.Header{}
	authorized.Set("Authorization", basic(user, token))

	resource, err := client.PropFind(ctx, "/remote.php/webdav/probe.bin", authorized)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	if resource.FileID == "" || resource.ETag == "" {
		t.Errorf("resource = %+v", resource)
	}
	if resource.SpaceID == "" || resource.OpaqueID == "" {
		t.Errorf("the fileid did not split: %+v", resource)
	}

	if _, err := client.PropFind(ctx, "/remote.php/webdav/no-such-file.bin", authorized); err != nil {
		if code, ok := Status(err); !ok || code != http.StatusNotFound {
			t.Errorf("PropFind of a missing file: %v", err)
		}
	} else {
		t.Error("PropFind of a missing file succeeded")
	}

	wrong := http.Header{}
	wrong.Set("Authorization", basic(user, "definitely not the token"))
	if _, err := client.PropFind(ctx, "/remote.php/webdav/probe.bin", wrong); err != nil {
		code, ok := Status(err)
		if !ok || (code != http.StatusUnauthorized && code != http.StatusForbidden) {
			t.Errorf("PropFind with a wrong token: %v", err)
		}
	} else {
		t.Error("PropFind with a wrong token succeeded")
	}
}

// A HEAD signed for a public link answers what the PROPFIND does, from the
// headers; only the signature and the expiration of the query travel.
func TestHeadReadsTheResourceFromTheHeaders(t *testing.T) {
	var seen *http.Request
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		switch r.URL.Path {
		case "/remote.php/dav/public-files/tok3n/clip.mp4":
			w.Header().Set("Oc-Fileid", "904763f4$b1f74ec4!fd142f17")
			w.Header().Set("Etag", `"22f357f48a44d564"`)
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", "10990")
			w.WriteHeader(http.StatusOK)
		case "/remote.php/dav/public-files/tok3n/folder":
			w.Header().Set("Oc-Fileid", "904763f4$b1f74ec4!d1")
			w.Header().Set("Content-Type", "httpd/unix-directory")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer platform.Close()

	client, err := NewClient(platform.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"signature": {"abc"}, "expiration": {"2030-01-01T00:00:00Z"}, "preview": {"1"}, "x": {"64"}}
	resource, err := client.Head(context.Background(), "/remote.php/dav/public-files/tok3n/clip.mp4", query, http.Header{"Cookie": {"c=1"}})
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if resource.OpaqueID != "fd142f17" || resource.SpaceID != "b1f74ec4" || resource.ETag != "22f357f48a44d564" || resource.ContentType != "video/mp4" || resource.Size != 10990 || resource.IsDir {
		t.Errorf("resource = %+v", resource)
	}
	if seen.Method != http.MethodHead || seen.URL.Query().Get("signature") != "abc" || seen.URL.Query().Get("expiration") == "" || seen.URL.Query().Get("preview") != "" {
		t.Errorf("the platform saw %s %s", seen.Method, seen.URL.String())
	}
	if seen.Header.Get("Cookie") != "c=1" {
		t.Error("the credentials of the client were not forwarded")
	}

	folder, err := client.Head(context.Background(), "/remote.php/dav/public-files/tok3n/folder", query, nil)
	if err != nil || !folder.IsDir {
		t.Errorf("folder = %+v, %v", folder, err)
	}

	_, err = client.Head(context.Background(), "/remote.php/dav/public-files/tok3n/nope", query, nil)
	if code, ok := Status(err); !ok || code != http.StatusUnauthorized {
		t.Errorf("rejected head = %v, want a 401 status error", err)
	}
	if !Signed(query) || Signed(url.Values{"signature": {"abc"}}) {
		t.Error("Signed does not want both the signature and the expiration")
	}
}
