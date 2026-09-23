// Package cs3 talks to the CS3 gateway of the platform under the service
// account: it resolves references with Stat and reads file contents through
// the data server.
//
// The events of the platform carry no file id and no etag, so almost every
// reaction starts with a Stat here.
package cs3

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Headers and values the platform expects.
const (
	// TokenHeader carries the reva token on an HTTP request to the data
	// server.
	TokenHeader = "X-Access-Token"

	// tokenMetadataKey is the same token as gRPC metadata, where keys are
	// lowercase.
	tokenMetadataKey = "x-access-token"

	// TransferHeader carries the token of one download on the data server.
	TransferHeader = "X-Reva-Transfer"

	// authType selects the service account authentication of the gateway.
	authType = "serviceaccounts"

	// spacesProtocol is the download protocol to prefer among the ones the
	// gateway offers.
	spacesProtocol = "spaces"
)

// tokenLifetime is how long a token is reused before it is asked for again.
// The gateway issues longer lived tokens; refreshing early costs one call and
// removes the window in which a token expires mid-request.
const tokenLifetime = 5 * time.Minute

// Errors returned by this package.
var (
	// ErrNotFound means the resource is gone: deleted or moved away while the
	// event was queued. Handlers acknowledge it and count it, they do not
	// retry.
	ErrNotFound = errors.New("cs3: resource not found")

	// ErrPermissionDenied means the service account may not see the resource.
	ErrPermissionDenied = errors.New("cs3: permission denied")

	ErrNoServiceAccount = errors.New("cs3: service account id and secret must be set")
)

// Config is how to reach the gateway of the platform.
type Config struct {
	GatewayAddr          string
	ServiceAccountID     string
	ServiceAccountSecret string

	// Insecure skips the TLS verification of the data server of the platform,
	// which runs on a self signed certificate on the stand.
	Insecure bool
}

// Client is a gateway client authenticated as the service account.
type Client struct {
	gateway gateway.GatewayAPIClient
	conn    *grpc.ClientConn
	http    *http.Client
	cfg     Config
	log     *slog.Logger

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New dials the gateway. The connection is lazy: no call is made until the
// first request.
func New(cfg Config, log *slog.Logger) (*Client, error) {
	if cfg.ServiceAccountID == "" || cfg.ServiceAccountSecret == "" {
		return nil, ErrNoServiceAccount
	}

	conn, err := grpc.NewClient(cfg.GatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cs3: dial %s: %w", cfg.GatewayAddr, err)
	}

	return &Client{
		gateway: gateway.NewGatewayAPIClient(conn),
		conn:    conn,
		http:    newHTTPClient(cfg.Insecure),
		cfg:     cfg,
		log:     log,
	}, nil
}

// Close releases the connection to the gateway.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Ping authenticates the service account. It backs the readiness probe.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.authenticate(ctx, true)
	return err
}

// authenticate returns a token for the service account, from the cache unless
// force is set.
func (c *Client) authenticate(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force && c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	res, err := c.gateway.Authenticate(ctx, &gateway.AuthenticateRequest{
		Type:         authType,
		ClientId:     c.cfg.ServiceAccountID,
		ClientSecret: c.cfg.ServiceAccountSecret,
	})
	if err != nil {
		return "", fmt.Errorf("cs3: authenticate: %w", err)
	}
	if code := res.GetStatus().GetCode(); code != rpc.Code_CODE_OK {
		return "", fmt.Errorf("cs3: authenticate: %s: %s", code, res.GetStatus().GetMessage())
	}

	c.token = res.GetToken()
	c.expires = time.Now().Add(tokenLifetime)
	return c.token, nil
}

// withToken returns a context carrying the token of the service account.
func (c *Client) withToken(ctx context.Context, force bool) (context.Context, string, error) {
	token, err := c.authenticate(ctx, force)
	if err != nil {
		return nil, "", err
	}
	return metadata.AppendToOutgoingContext(ctx, tokenMetadataKey, token), token, nil
}

// statusError turns a CS3 status into an error, mapping the codes the
// handlers branch on to sentinels.
func statusError(operation string, status *rpc.Status) error {
	switch status.GetCode() {
	case rpc.Code_CODE_OK:
		return nil
	case rpc.Code_CODE_NOT_FOUND:
		return fmt.Errorf("cs3: %s: %w", operation, ErrNotFound)
	case rpc.Code_CODE_PERMISSION_DENIED:
		return fmt.Errorf("cs3: %s: %w", operation, ErrPermissionDenied)
	default:
		return fmt.Errorf("cs3: %s: %s: %s", operation, status.GetCode(), status.GetMessage())
	}
}

// unauthenticated reports whether a status asks for a fresh token.
func unauthenticated(status *rpc.Status) bool {
	return status.GetCode() == rpc.Code_CODE_UNAUTHENTICATED
}
