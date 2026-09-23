package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// proxyTimeouts of the transport to a service of the platform.
const (
	proxyDialTimeout     = 5 * time.Second
	proxyResponseTimeout = 60 * time.Second
)

// NewProxy returns a reverse proxy to one service of the platform. It changes
// nothing: the path, the query, the headers and the body travel as they came,
// and the answer is passed back untouched.
//
// Previews of files that are not video are served this way, by the webdav
// service of the platform, as if the extension were not in the path at all.
func NewProxy(upstream string, log *slog.Logger) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("httpx: upstream %q: must be an absolute URL", upstream)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = proxyResponseTimeout

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = target.Scheme
			r.Out.URL.Host = target.Host
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("proxy to the platform failed",
				slog.String("upstream", upstream),
				slog.String("path", r.URL.Path),
				slog.Any("error", err))
			WriteDAVError(w, http.StatusBadGateway, "the platform did not answer")
		},
	}, nil
}
