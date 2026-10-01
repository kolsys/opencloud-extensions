package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	// drivesPath is the graph endpoint listing the spaces of the caller.
	drivesPath = "/graph/v1.0/me/drives"

	// maxDrives bounds that listing: every space comes with its root, its
	// quota and its special items.
	maxDrives = 16 << 20
)

// Types of the drives that are spaces of their own. The graph API lists the
// shares a caller received too, as mountpoint and virtual drives.
const (
	DrivePersonal = "personal"
	DriveProject  = "project"
)

// Drive is a space the caller has access to.
type Drive struct {
	// ID is the composite id of the space, storageid$spaceid.
	ID string
	// SpaceID is the space part of ID, the one the events of the platform
	// carry.
	SpaceID string
	Name    string
	Type    string
}

// Drives lists the spaces the caller is a member of, passing on the
// credentials of the client. A rejected credential comes back as a
// StatusError.
func (c *Client) Drives(ctx context.Context, header http.Header) ([]Drive, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+drivesPath, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: drives: %w", err)
	}
	for _, name := range ForwardedHeaders {
		if value := header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	// The URL is the internal address of the platform plus a fixed path.
	response, err := c.http.Do(request) //nolint:gosec // G704
	if err != nil {
		return nil, fmt.Errorf("httpx: drives: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrain))
		return nil, StatusError{Code: response.StatusCode}
	}

	// A caller without any space gets null rather than an empty list.
	var answer struct {
		Value []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			DriveType string `json:"driveType"`
		} `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxDrives)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("httpx: drives: %w", err)
	}

	drives := make([]Drive, 0, len(answer.Value))
	for _, value := range answer.Value {
		_, spaceID, _ := splitFileID(value.ID)
		drives = append(drives, Drive{ID: value.ID, SpaceID: spaceID, Name: value.Name, Type: value.DriveType})
	}
	return drives, nil
}
