package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// whoamiPath is the OCS endpoint answering who the caller is.
const whoamiPath = "/ocs/v1.php/cloud/user?format=json"

// Identity is the caller as the platform knows it.
type Identity struct {
	// Username is the login name, the value FILE_ACTIVITY_ALLOWED_USERS lists.
	Username    string
	DisplayName string
	Email       string
}

// Whoami asks the platform who the caller is, passing on the credentials of
// the client. A rejected credential comes back as a StatusError.
func (c *Client) Whoami(ctx context.Context, header http.Header) (*Identity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+whoamiPath, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: whoami: %w", err)
	}
	request.Header.Set("Ocs-Apirequest", "true")
	for _, name := range ForwardedHeaders {
		if value := header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	// The URL is the internal address of the platform plus a fixed path.
	response, err := c.http.Do(request) //nolint:gosec // G704
	if err != nil {
		return nil, fmt.Errorf("httpx: whoami: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrain))
		return nil, StatusError{Code: response.StatusCode}
	}

	var answer struct {
		OCS struct {
			Data struct {
				ID          string `json:"id"`
				DisplayName string `json:"display-name"`
				Email       string `json:"email"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAnswer)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("httpx: whoami: %w", err)
	}
	if answer.OCS.Data.ID == "" {
		return nil, errors.New("httpx: whoami: the platform named no user")
	}

	return &Identity{
		Username:    answer.OCS.Data.ID,
		DisplayName: answer.OCS.Data.DisplayName,
		Email:       answer.OCS.Data.Email,
	}, nil
}
