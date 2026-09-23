package cs3

import (
	"context"
	"time"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
)

// ListContainer returns what a folder holds, one level deep. The paths of
// the answers are not to be trusted across the shapes of references; the
// caller builds them from the names.
func (c *Client) ListContainer(ctx context.Context, ref Ref) ([]ResourceInfo, error) {
	request := &provider.ListContainerRequest{Ref: ref.proto()}

	authCtx, _, err := c.withToken(ctx, false)
	if err != nil {
		return nil, err
	}

	res, err := c.gateway.ListContainer(authCtx, request)
	if err != nil {
		return nil, wrap("list container", ref, err)
	}
	if unauthenticated(res.GetStatus()) {
		if authCtx, _, err = c.withToken(ctx, true); err != nil {
			return nil, err
		}
		if res, err = c.gateway.ListContainer(authCtx, request); err != nil {
			return nil, wrap("list container", ref, err)
		}
	}
	if err := statusError("list container "+ref.String(), res.GetStatus()); err != nil {
		return nil, err
	}

	infos := make([]ResourceInfo, 0, len(res.GetInfos()))
	for _, info := range res.GetInfos() {
		infos = append(infos, *resourceInfo(info))
	}
	return infos, nil
}

// RecycleItem is one item of a trash bin as the platform lists it: the top
// of what was trashed, files and folders alike.
type RecycleItem struct {
	// Key is the id of the item, the one its events carry.
	Key string
	// Path is the path the item had before it was trashed.
	Path      string
	IsDir     bool
	Size      uint64
	DeletedAt time.Time
}

// ListRecycle returns the items in the trash bin of a space, given the root
// of the space.
func (c *Client) ListRecycle(ctx context.Context, root Ref) ([]RecycleItem, error) {
	request := &provider.ListRecycleRequest{Ref: root.ResourceID().proto()}

	authCtx, _, err := c.withToken(ctx, false)
	if err != nil {
		return nil, err
	}

	res, err := c.gateway.ListRecycle(authCtx, request)
	if err != nil {
		return nil, wrap("list recycle", root, err)
	}
	if unauthenticated(res.GetStatus()) {
		if authCtx, _, err = c.withToken(ctx, true); err != nil {
			return nil, err
		}
		if res, err = c.gateway.ListRecycle(authCtx, request); err != nil {
			return nil, wrap("list recycle", root, err)
		}
	}
	if err := statusError("list recycle "+root.String(), res.GetStatus()); err != nil {
		return nil, err
	}

	items := make([]RecycleItem, 0, len(res.GetRecycleItems()))
	for _, item := range res.GetRecycleItems() {
		items = append(items, RecycleItem{
			Key:       item.GetKey(),
			Path:      item.GetRef().GetPath(),
			IsDir:     item.GetType() == provider.ResourceType_RESOURCE_TYPE_CONTAINER,
			Size:      item.GetSize(),
			DeletedAt: protoTime(item.GetDeletionTime()),
		})
	}
	return items, nil
}
