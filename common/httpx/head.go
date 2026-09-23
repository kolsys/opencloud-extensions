package httpx

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Query parameters that authenticate a request to a public link: the web
// asks for the previews of a link with a password this way, without any
// header. The platform accepts them on GET and HEAD only.
const (
	ParamSignature  = "signature"
	ParamExpiration = "expiration"
)

// Headers a HEAD of the platform answers with.
const (
	headerFileID      = "Oc-Fileid"
	contentTypeFolder = "httpd/unix-directory"
)

// Signed reports whether a query authenticates by the signature of a public
// link.
func Signed(query url.Values) bool {
	return query.Get(ParamSignature) != "" && query.Get(ParamExpiration) != ""
}

// Head asks about one resource with a HEAD, the one request besides GET the
// platform authenticates by the signature of a public link. The signature
// and the expiration of the query travel with it, the credentials of the
// client too. It answers what the PROPFIND does, from the headers.
func (c *Client) Head(ctx context.Context, davPath string, query url.Values, header http.Header) (*Resource, error) {
	if !strings.HasPrefix(davPath, "/") {
		davPath = "/" + davPath
	}

	target := *c.base
	target.Path = c.base.Path + davPath
	signed := url.Values{}
	for _, name := range []string{ParamSignature, ParamExpiration} {
		if value := query.Get(name); value != "" {
			signed.Set(name, value)
		}
	}
	target.RawQuery = signed.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: head %s: %w", davPath, err)
	}
	for _, name := range ForwardedHeaders {
		if value := header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	response, err := c.http.Do(request) //nolint:gosec // G704: the internal address of the platform plus the path of the request
	if err != nil {
		return nil, fmt.Errorf("httpx: head %s: %w", davPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, StatusError{Code: response.StatusCode}
	}
	return parseHead(response.Header)
}

// parseHead reads a resource out of the headers of a HEAD.
func parseHead(header http.Header) (*Resource, error) {
	resource := &Resource{
		FileID: header.Get(headerFileID),
		ETag:   strings.Trim(header.Get("Etag"), `"`),
	}
	if resource.FileID == "" {
		return nil, fmt.Errorf("httpx: head answer holds no %s", headerFileID)
	}
	resource.StorageID, resource.SpaceID, resource.OpaqueID = splitFileID(resource.FileID)

	if contentType, _, err := mime.ParseMediaType(header.Get("Content-Type")); err == nil {
		resource.ContentType = contentType
	}
	resource.IsDir = resource.ContentType == contentTypeFolder
	if length := header.Get("Content-Length"); length != "" {
		resource.Size, _ = strconv.ParseInt(length, 10, 64)
	}
	return resource, nil
}
