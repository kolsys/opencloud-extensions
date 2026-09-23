package cs3

import (
	"context"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
)

// Space is a storage space of the platform: a personal space, a project
// space or a share.
type Space struct {
	// ID is the composite id the platform uses in its APIs.
	ID string
	// Root points at the top of the space and is the base of every path the
	// events of that space carry.
	Root Ref
	Name string
	// Type is "personal", "project", "share" or "mountpoint".
	Type string
	// Owner is the id of the user the space belongs to, empty for a project.
	Owner string
}

// ListSpaces returns the spaces the service account can see, which is all of
// them. Backfill and garbage collection walk them.
func (c *Client) ListSpaces(ctx context.Context) ([]Space, error) {
	request := &provider.ListStorageSpacesRequest{}

	authCtx, _, err := c.withToken(ctx, false)
	if err != nil {
		return nil, err
	}

	res, err := c.gateway.ListStorageSpaces(authCtx, request)
	if err != nil {
		return nil, wrap("list spaces", Ref{}, err)
	}
	if unauthenticated(res.GetStatus()) {
		if authCtx, _, err = c.withToken(ctx, true); err != nil {
			return nil, err
		}
		if res, err = c.gateway.ListStorageSpaces(authCtx, request); err != nil {
			return nil, wrap("list spaces", Ref{}, err)
		}
	}
	if err := statusError("list spaces", res.GetStatus()); err != nil {
		return nil, err
	}

	spaces := make([]Space, 0, len(res.GetStorageSpaces()))
	for _, space := range res.GetStorageSpaces() {
		spaces = append(spaces, Space{
			ID: space.GetId().GetOpaqueId(),
			Root: Ref{
				StorageID: space.GetRoot().GetStorageId(),
				SpaceID:   space.GetRoot().GetSpaceId(),
				OpaqueID:  space.GetRoot().GetOpaqueId(),
			},
			Name:  space.GetName(),
			Type:  space.GetSpaceType(),
			Owner: space.GetOwner().GetId().GetOpaqueId(),
		})
	}
	return spaces, nil
}
