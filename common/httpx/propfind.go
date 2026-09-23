package httpx

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// propfindTimeout bounds one authorisation round trip.
	propfindTimeout = 15 * time.Second

	// maxAnswer bounds the multistatus of a Depth: 0 answer.
	maxAnswer = 1 << 20

	// maxDrain is how much of a rejected answer is read before the
	// connection is given back to the pool.
	maxDrain = 4096
)

// ForwardedHeaders are the ones that carry the identity of the client. They
// are the only headers a PROPFIND of an extension passes on.
var ForwardedHeaders = []string{"Authorization", "Cookie", "public-token", "Ocs-Apirequest"}

// propfindBody asks for the properties the extensions need: the fileid and
// the etag key a thumbnail, the content type tells video from the rest.
const propfindBody = xml.Header + `<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:prop>
  <oc:fileid/>
  <d:getetag/>
  <d:getcontenttype/>
  <d:getcontentlength/>
  <d:resourcetype/>
 </d:prop>
</d:propfind>`

// StatusError is an answer of the platform that has to reach the client
// unchanged: 401, 403 and 404 of a PROPFIND are the verdict on the request,
// 425 says the file is still being processed after its upload.
type StatusError struct {
	Code int
}

func (e StatusError) Error() string {
	return fmt.Sprintf("httpx: the platform answered %d %s", e.Code, http.StatusText(e.Code))
}

// Status returns the code of err when it is a StatusError.
func Status(err error) (int, bool) {
	var status StatusError
	if errors.As(err, &status) {
		return status.Code, true
	}
	return 0, false
}

// Resource is what a PROPFIND reports about one file.
type Resource struct {
	// FileID is the composite id of the platform, storageid$spaceid!opaqueid.
	FileID string
	// StorageID, SpaceID and OpaqueID are the parts of FileID: a thumbnail
	// key is built from the last two, a CS3 reference needs all three.
	StorageID string
	SpaceID   string
	OpaqueID  string
	// ETag comes without the quotes the platform wraps it in.
	ETag        string
	ContentType string
	Size        int64
	IsDir       bool
}

// Client asks the platform about a resource with the credentials of the
// caller. It is both the authorisation check of every preview request and the
// source of the ids, so it runs on the internal URL of the proxy.
type Client struct {
	base *url.URL
	http *http.Client
}

// NewClient returns a client for the proxy of the platform.
func NewClient(baseURL string, insecure bool) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("httpx: the internal URL of the platform %q: must be an absolute URL", baseURL)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure, //nolint:gosec // the stand runs on a self signed certificate
	}

	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawPath = ""
	return &Client{
		base: base,
		http: &http.Client{Timeout: propfindTimeout, Transport: transport},
	}, nil
}

// PropFind asks about one resource with Depth: 0, passing on the credentials
// of the client. A 401, 403 or 404 comes back as a StatusError for the caller
// to forward as it is.
func (c *Client) PropFind(ctx context.Context, davPath string, header http.Header) (*Resource, error) {
	if !strings.HasPrefix(davPath, "/") {
		davPath = "/" + davPath
	}

	// The path arrives decoded; a name with a percent sign, a hash or a
	// question mark has to be escaped again on the way out.
	target := *c.base
	target.Path = c.base.Path + davPath
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", target.String(), strings.NewReader(propfindBody))
	if err != nil {
		return nil, fmt.Errorf("httpx: propfind %s: %w", davPath, err)
	}
	request.Header.Set("Depth", "0")
	request.Header.Set("Content-Type", contentTypeXML)
	for _, name := range ForwardedHeaders {
		if value := header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	// The URL is the internal address of the platform plus the path of the
	// request, not an address a client chose.
	response, err := c.http.Do(request) //nolint:gosec // G704
	if err != nil {
		return nil, fmt.Errorf("httpx: propfind %s: %w", davPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusMultiStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrain))
		return nil, StatusError{Code: response.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer))
	if err != nil {
		return nil, fmt.Errorf("httpx: propfind %s: %w", davPath, err)
	}
	return parsePropFind(body)
}

// multistatus is the answer of a PROPFIND, reduced to the first response.
type multistatus struct {
	Responses []struct {
		PropStats []struct {
			Status string `xml:"status"`
			Prop   struct {
				FileID       string `xml:"fileid"`
				ETag         string `xml:"getetag"`
				ContentType  string `xml:"getcontenttype"`
				Length       string `xml:"getcontentlength"`
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func parsePropFind(body []byte) (*Resource, error) {
	var parsed multistatus
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("httpx: propfind answer: %w", err)
	}
	if len(parsed.Responses) == 0 {
		return nil, errors.New("httpx: propfind answer holds no response")
	}

	resource := &Resource{}
	statuses := make([]string, 0, len(parsed.Responses[0].PropStats))
	for _, propstat := range parsed.Responses[0].PropStats {
		statuses = append(statuses, propstat.Status)
		// A file still in postprocessing after its upload is reported with
		// its properties under a 425 status; the client asks again later.
		if strings.Contains(propstat.Status, " 425 ") {
			return nil, StatusError{Code: http.StatusTooEarly}
		}
		// The platform splits the properties over one propstat per status and
		// reports the ones it could not answer as 404.
		if !strings.Contains(propstat.Status, " 200 ") {
			continue
		}

		prop := propstat.Prop
		if prop.FileID != "" {
			resource.FileID = prop.FileID
			resource.StorageID, resource.SpaceID, resource.OpaqueID = splitFileID(prop.FileID)
		}
		if prop.ETag != "" {
			resource.ETag = strings.Trim(prop.ETag, `"`)
		}
		if prop.ContentType != "" {
			resource.ContentType = prop.ContentType
		}
		if prop.Length != "" {
			resource.Size, _ = strconv.ParseInt(prop.Length, 10, 64)
		}
		if prop.ResourceType.Collection != nil {
			resource.IsDir = true
		}
	}

	if resource.FileID == "" {
		return nil, fmt.Errorf("httpx: propfind answer holds no fileid, statuses %q", statuses)
	}
	return resource, nil
}

// splitFileID takes storageid$spaceid!opaqueid apart. The storage id is
// optional in the ids the platform writes.
func splitFileID(fileID string) (storageID, spaceID, opaqueID string) {
	if head, rest, found := strings.Cut(fileID, "$"); found {
		storageID, fileID = head, rest
	}
	spaceID, opaqueID, _ = strings.Cut(fileID, "!")
	return storageID, spaceID, opaqueID
}
