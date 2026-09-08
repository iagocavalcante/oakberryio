// Package cli implements the oak CLI's HTTP client against oakd: the
// transport (unix socket vs tunneled HTTPS), the API calls, and small pure
// helpers like ParseKV.
package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Transport is oak's HTTP client to oakd. With OAK_API unset, it dials the
// unix socket at OAK_SOCKET (default /run/oak.sock) -- trusted local access,
// no token needed. With OAK_API set, it speaks HTTPS to that base URL and
// attaches a bearer token from OAK_TOKEN or ~/.oak/token, matching the
// tunneled listener's AuthMiddleware (internal/daemon/api.go).
type Transport struct {
	Client  *http.Client
	BaseURL string
	Token   string
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
		socket = "/run/oak.sock"
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	return &Transport{Client: client, BaseURL: "http://unix"}, nil
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
