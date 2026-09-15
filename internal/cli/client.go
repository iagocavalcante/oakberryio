package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Machine mirrors internal/store.Machine's JSON encoding (that struct has no
// json tags, so its field names are the wire format). Kept as a separate
// type so the CLI doesn't need to import internal/store.
type Machine struct {
	ID        string
	App       string
	ReleaseID int64
	NodeID    string
	IP        string
	Tap       string
	State     string
	PID       int64
	CreatedAt string
}

// Client wraps a Transport with oak's specific API calls against oakd; see
// internal/daemon/api.go for the wire contract.
type Client struct {
	*Transport
}

// NewClient builds a Client from the environment (see NewTransport).
func NewClient() (*Client, error) {
	t, err := NewTransport()
	if err != nil {
		return nil, err
	}
	return &Client{Transport: t}, nil
}

// httpError formats a non-2xx response as an error, including the body.
func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
}

// Deploy posts a deploy request for app and streams the response's progress
// lines to out as they arrive. Per the API's contract (internal/daemon/
// api.go, handleDeploy) the HTTP status is always 200 once streaming starts;
// the real outcome is the final line, which reads "ok <machine-id>" on
// success or "error: <message>" on failure -- Deploy returns an error in the
// latter case.
func (c *Client) Deploy(ctx context.Context, app, image, tomlText string, out io.Writer) error {
	body, err := json.Marshal(struct {
		Image  string `json:"image"`
		Config string `json:"config"`
	}{Image: image, Config: tomlText})
	if err != nil {
		return fmt.Errorf("marshal deploy request: %w", err)
	}

	req, err := c.NewRequest(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/deploy", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("deploy request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deploy: %w", httpError(resp))
	}

	var lastLine string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(out, line)
		lastLine = line
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read deploy stream: %w", err)
	}
	if strings.HasPrefix(lastLine, "error:") {
		return fmt.Errorf("%s", lastLine)
	}
	return nil
}

// BuildRemote posts contextTar (a tar of the build context) to oakd's
// POST /apps/{name}/build, the `oak deploy --remote` path -- see
// docs/plans/2026-09-15-remote-build-design.md and internal/daemon/api.go's
// handleBuild for the wire contract. dockerfile and buildArgs travel as the
// X-Oak-Dockerfile and X-Oak-Build-Args headers.
//
// Like Deploy, the HTTP status is always 200 once streaming starts; output
// is streamed to out as it arrives, and the real outcome is the final line:
// "image <ref>" on success (its ref is BuildRemote's return value) or
// "error: <message>" on failure, in which case BuildRemote returns an
// error.
func (c *Client) BuildRemote(ctx context.Context, app, dockerfile string, buildArgs map[string]string, contextTar io.Reader, out io.Writer) (string, error) {
	req, err := c.NewRequest(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/build", contextTar)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Oak-Dockerfile", dockerfile)
	if len(buildArgs) > 0 {
		argsJSON, err := json.Marshal(buildArgs)
		if err != nil {
			return "", fmt.Errorf("marshal build args: %w", err)
		}
		req.Header.Set("X-Oak-Build-Args", string(argsJSON))
	}

	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("build %s: %w", app, httpError(resp))
	}

	var lastLine string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(out, line)
		lastLine = line
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read build stream: %w", err)
	}
	if strings.HasPrefix(lastLine, "error:") {
		return "", fmt.Errorf("%s", lastLine)
	}
	image, ok := strings.CutPrefix(lastLine, "image ")
	if !ok {
		return "", fmt.Errorf("build %s: unexpected final line %q", app, lastLine)
	}
	return image, nil
}

// Restart reboots app from its latest release with no rebuild, streaming
// progress to out exactly like Deploy (see its doc comment for the wire
// contract).
func (c *Client) Restart(ctx context.Context, app string, out io.Writer) error {
	req, err := c.NewRequest(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/restart", nil)
	if err != nil {
		return err
	}
	return c.streamProgress(req, "restart", app, out)
}

// Scale resizes app's VM (memory and/or CPU count) and reboots it, streaming
// progress to out exactly like Deploy. A zero memoryMB or cpus leaves that
// field unchanged.
func (c *Client) Scale(ctx context.Context, app string, memoryMB, cpus int, out io.Writer) error {
	body, err := json.Marshal(struct {
		MemoryMB int `json:"memory_mb"`
		CPUs     int `json:"cpus"`
	}{MemoryMB: memoryMB, CPUs: cpus})
	if err != nil {
		return fmt.Errorf("marshal scale request: %w", err)
	}
	req, err := c.NewRequest(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/scale", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return c.streamProgress(req, "scale", app, out)
}

// streamProgress sends req and streams its response body to out line by
// line, exactly like Deploy: the HTTP status is always 200 once streaming
// starts, and the real outcome is the final line ("ok <machine-id>" or
// "error: <message>").
func (c *Client) streamProgress(req *http.Request, verb, app string, out io.Writer) error {
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s request: %w", verb, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %w", verb, app, httpError(resp))
	}

	var lastLine string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(out, line)
		lastLine = line
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s stream: %w", verb, err)
	}
	if strings.HasPrefix(lastLine, "error:") {
		return fmt.Errorf("%s", lastLine)
	}
	return nil
}

// Destroy tears down app entirely: its machines, all its rows, its on-disk
// artifacts, and its tunnel route.
func (c *Client) Destroy(ctx context.Context, app string) error {
	req, err := c.NewRequest(ctx, http.MethodDelete, "/apps/"+url.PathEscape(app), nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("destroy request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("destroy %s: %w", app, httpError(resp))
	}
	return nil
}

// SecretKeys returns app's secret key names, sorted, never their values.
func (c *Client) SecretKeys(ctx context.Context, app string) ([]string, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, "/apps/"+url.PathEscape(app)+"/secrets", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secrets list request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("secrets list %s: %w", app, httpError(resp))
	}
	var keys []string
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("decode secrets list response: %w", err)
	}
	return keys, nil
}

// UnsetSecrets deletes the named secret keys for app.
func (c *Client) UnsetSecrets(ctx context.Context, app string, keys []string) error {
	body, err := json.Marshal(struct {
		Keys []string `json:"keys"`
	}{Keys: keys})
	if err != nil {
		return fmt.Errorf("marshal unset secrets request: %w", err)
	}
	req, err := c.NewRequest(ctx, http.MethodDelete, "/apps/"+url.PathEscape(app)+"/secrets", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("unset secrets request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unset secrets %s: %w", app, httpError(resp))
	}
	return nil
}

// Apps lists every known app.
func (c *Client) Apps(ctx context.Context) ([]string, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, "/apps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apps request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apps: %w", httpError(resp))
	}
	var apps []string
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, fmt.Errorf("decode apps response: %w", err)
	}
	return apps, nil
}

// Machines lists app's machines.
func (c *Client) Machines(ctx context.Context, app string) ([]Machine, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, "/apps/"+url.PathEscape(app)+"/machines", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("machines request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s: %w", app, httpError(resp))
	}
	var machines []Machine
	if err := json.NewDecoder(resp.Body).Decode(&machines); err != nil {
		return nil, fmt.Errorf("decode machines response: %w", err)
	}
	return machines, nil
}

// Logs streams app's most recent machine's log to out. With follow it tails
// the log until the connection is closed (e.g. the process is interrupted).
func (c *Client) Logs(ctx context.Context, app string, follow bool, out io.Writer) error {
	path := "/apps/" + url.PathEscape(app) + "/logs"
	if follow {
		path += "?follow=1"
	}
	req, err := c.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("logs request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("logs %s: %w", app, httpError(resp))
	}
	if _, err := io.Copy(out, resp.Body); err != nil && !follow {
		return fmt.Errorf("read logs: %w", err)
	}
	return nil
}

// SetSecrets encrypts and stores key/value pairs for app.
func (c *Client) SetSecrets(ctx context.Context, app string, kv map[string]string) error {
	body, err := json.Marshal(kv)
	if err != nil {
		return fmt.Errorf("marshal secrets: %w", err)
	}
	req, err := c.NewRequest(ctx, http.MethodPut, "/apps/"+url.PathEscape(app)+"/secrets", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("secrets request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets set %s: %w", app, httpError(resp))
	}
	return nil
}

// SSH opens a raw duplex connection to app's guest agent through oakd's
// vsock bridge (POST /apps/{name}/ssh, see internal/daemon/api.go's
// handleSSH) and returns it once the request has been accepted. The
// std http.Client hands back a Response, not the hijacked connection's raw
// duplex stream, so this speaks just enough HTTP/1.1 by hand: send the
// request line and headers over a raw Transport.Dial connection, then read
// up to the blank line ending the response headers and check the status is
// 200. From the point this returns, the connection is the raw
// JSON-header-then-PTY stream the wire protocol describes -- the caller
// (cmd/oak's runSSH) owns everything past here.
func (c *Client) SSH(ctx context.Context, app string) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	host := "unix"
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	var req strings.Builder
	fmt.Fprintf(&req, "POST /apps/%s/ssh HTTP/1.1\r\n", url.PathEscape(app))
	fmt.Fprintf(&req, "Host: %s\r\n", host)
	if c.Token != "" {
		fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", c.Token)
	}
	req.WriteString("Content-Length: 0\r\n\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send ssh request: %w", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read ssh response status: %w", err)
	}
	if fields := strings.Fields(statusLine); len(fields) < 2 || fields[1] != "200" {
		conn.Close()
		return nil, fmt.Errorf("ssh %s: unexpected status %q", app, strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read ssh response headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	return &bufferedConn{Conn: conn, r: reader}, nil
}

// bufferedConn is a net.Conn whose Read is served from a bufio.Reader
// wrapping the same connection, so bytes the reader buffered while parsing
// the HTTP response's status line and headers (see Client.SSH) aren't lost
// to a later raw Read on the underlying conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CreateVolume provisions a new volume for app.
func (c *Client) CreateVolume(ctx context.Context, app, name string, sizeGB int) error {
	body, err := json.Marshal(struct {
		Name   string `json:"name"`
		SizeGB int    `json:"size_gb"`
	}{Name: name, SizeGB: sizeGB})
	if err != nil {
		return fmt.Errorf("marshal volume request: %w", err)
	}
	req, err := c.NewRequest(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/volumes", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("volumes request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("volumes create %s: %w", app, httpError(resp))
	}
	return nil
}
