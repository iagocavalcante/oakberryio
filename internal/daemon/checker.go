package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// HTTPChecker implements Checker: an HTTP GET against path when one is
// configured (requiring a 2xx response), otherwise a plain TCP dial.
type HTTPChecker struct{}

// Healthy checks whether the service at ip:port is up.
func (HTTPChecker) Healthy(ctx context.Context, ip string, port int, path string) error {
	addr := fmt.Sprintf("%s:%d", ip, port)

	if path == "" {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("dial %s: %w", addr, err)
		}
		return conn.Close()
	}

	url := fmt.Sprintf("http://%s%s", addr, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return nil
}
