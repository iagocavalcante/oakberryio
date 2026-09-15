// Package cli implements the oak CLI's HTTP client against oakd: the
// transport (unix socket vs tunneled HTTPS), the API calls, and small pure
// helpers like ParseKV.
package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Transport is oak's HTTP client to oakd. With OAK_API unset, it dials the
// unix socket at OAK_SOCKET (default /run/oak/oak.sock) -- trusted local access,
// no token needed. With OAK_API set, it speaks HTTPS to that base URL and
// attaches a bearer token from OAK_TOKEN or ~/.oak/token, matching the
// tunneled listener's AuthMiddleware (internal/daemon/api.go).
type Transport struct {
	Client  *http.Client
	BaseURL string
	Token   string

	// socket is the unix socket path Dial connects to; empty when this
	// Transport speaks HTTPS instead (OAK_API set).
	socket string
}

// NewTransport builds a Transport from the environment.
func NewTransport() (*Transport, error) {
	if api := os.Getenv("OAK_API"); api != "" {
		token, err := apiToken()
		if err != nil {
			return nil, err
		}
		return &Transport{Client: http.DefaultClient, BaseURL: strings.TrimRight(api, "/"), Token: token}, nil
	}

	socket := os.Getenv("OAK_SOCKET")
	if socket == "" {
		socket = "/run/oak/oak.sock"
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	return &Transport{Client: client, BaseURL: "http://unix", socket: socket}, nil
}

// Dial returns a raw connection to oakd, for endpoints (like `oak ssh`)
// that need direct duplex byte-stream access the std http.Client won't hand
// back. Over a unix socket transport this dials the socket directly; over
// an HTTPS transport this dials TLS to the configured host, matching how
// http.Transport would connect for a normal request.
func (t *Transport) Dial(ctx context.Context) (net.Conn, error) {
	if t.socket != "" {
		var d net.Dialer
		return d.DialContext(ctx, "unix", t.socket)
	}

	u, err := url.Parse(t.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url %q: %w", t.BaseURL, err)
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	var d net.Dialer
	return tls.DialWithDialer(&d, "tcp", host, nil)
}

// apiToken resolves OAK_TOKEN, falling back to ~/.oak/token.
func apiToken() (string, error) {
	if t := os.Getenv("OAK_TOKEN"); t != "" {
		return t, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("OAK_TOKEN not set and could not find home dir for ~/.oak/token: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".oak", "token"))
	if err != nil {
		return "", fmt.Errorf("OAK_TOKEN not set and could not read ~/.oak/token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// NewRequest builds a request against oakd, attaching the bearer token when
// one is configured.
func (t *Transport) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.BaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	return req, nil
}

// Do sends req.
func (t *Transport) Do(req *http.Request) (*http.Response, error) {
	return t.Client.Do(req)
}
