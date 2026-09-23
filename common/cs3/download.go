package cs3

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
)

// downloadTimeout bounds one transfer from the data server.
const downloadTimeout = 30 * time.Minute

func newHTTPClient(skipVerify bool) *http.Client {
	return &http.Client{
		Timeout: downloadTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: skipVerify, //nolint:gosec // the stand runs on a self signed certificate
			},
		},
	}
}

// Download opens the content of a resource.
//
// When length is above zero, the bytes from offset are requested with a Range
// header; ranged reports whether the data server honoured it. A data server
// that ignores the range answers the whole file, which the caller has to read
// from the start.
//
// The caller closes the reader.
func (c *Client) Download(ctx context.Context, ref Ref, offset, length int64) (body io.ReadCloser, ranged bool, err error) {
	endpoint, transfer, token, err := c.initiateDownload(ctx, ref)
	if err != nil {
		return nil, false, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("cs3: download %s: %w", ref, err)
	}
	request.Header.Set(TokenHeader, token)
	request.Header.Set(TransferHeader, transfer)
	if length > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}

	// The endpoint is the data server the gateway named, not user input.
	response, err := c.http.Do(request) //nolint:gosec // G704
	if err != nil {
		return nil, false, fmt.Errorf("cs3: download %s: %w", ref, err)
	}

	switch response.StatusCode {
	case http.StatusOK:
		return response.Body, false, nil
	case http.StatusPartialContent:
		return response.Body, true, nil
	case http.StatusNotFound:
		response.Body.Close()
		return nil, false, fmt.Errorf("cs3: download %s: %w", ref, ErrNotFound)
	case http.StatusForbidden, http.StatusUnauthorized:
		response.Body.Close()
		return nil, false, fmt.Errorf("cs3: download %s: %w", ref, ErrPermissionDenied)
	default:
		response.Body.Close()
		return nil, false, fmt.Errorf("cs3: download %s: data server answered %s", ref, response.Status)
	}
}

// probeLength is the size of the range the probe asks for. The data server
// of the platform answers a range of a single byte with 500, so the probe
// asks for a few kilobytes.
const probeLength = 4096

// SupportsRange reports whether the data server of the platform answers a
// range request with 206. Without ranges a thumbnail worker has to pull whole
// files into a temporary file first.
func (c *Client) SupportsRange(ctx context.Context, ref Ref) (bool, error) {
	body, ranged, err := c.Download(ctx, ref, 0, probeLength)
	if err != nil {
		return false, err
	}
	defer body.Close()

	if _, err := io.Copy(io.Discard, io.LimitReader(body, probeLength)); err != nil {
		return false, fmt.Errorf("cs3: probe range on %s: %w", ref, err)
	}
	return ranged, nil
}

// initiateDownload asks the gateway where the bytes can be read and with
// which transfer token.
func (c *Client) initiateDownload(ctx context.Context, ref Ref) (endpoint, transfer, token string, err error) {
	request := &provider.InitiateFileDownloadRequest{Ref: ref.proto()}

	authCtx, token, err := c.withToken(ctx, false)
	if err != nil {
		return "", "", "", err
	}

	res, err := c.gateway.InitiateFileDownload(authCtx, request)
	if err != nil {
		return "", "", "", wrap("initiate download", ref, err)
	}
	if unauthenticated(res.GetStatus()) {
		if authCtx, token, err = c.withToken(ctx, true); err != nil {
			return "", "", "", err
		}
		if res, err = c.gateway.InitiateFileDownload(authCtx, request); err != nil {
			return "", "", "", wrap("initiate download", ref, err)
		}
	}
	if err := statusError("initiate download "+ref.String(), res.GetStatus()); err != nil {
		return "", "", "", err
	}

	// The spaces protocol is the one the platform serves file contents on;
	// anything else is taken only when it is the single option offered.
	protocols := res.GetProtocols()
	for _, protocol := range protocols {
		if protocol.GetProtocol() == spacesProtocol {
			return protocol.GetDownloadEndpoint(), protocol.GetToken(), token, nil
		}
	}
	if len(protocols) > 0 {
		return protocols[0].GetDownloadEndpoint(), protocols[0].GetToken(), token, nil
	}

	return "", "", "", fmt.Errorf("cs3: download %s: the gateway offered no protocol", ref)
}

func wrap(operation string, ref Ref, err error) error {
	return fmt.Errorf("cs3: %s %s: %w", operation, ref, err)
}
