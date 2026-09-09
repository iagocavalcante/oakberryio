// Package tunnel renders and applies the cloudflared ingress config that
// routes each app's hostname to its running machine.
package tunnel

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrRestart wraps any failure to restart cloudflared after writing a new
// config, so callers can distinguish "config written but cloudflared didn't
// pick it up" (errors.Is(err, ErrRestart)) from a failure to write the
// config at all.
var ErrRestart = errors.New("tunnel: cloudflared restart failed")

// Route is one cloudflared ingress rule: a hostname and the local service it
// proxies to (e.g. "http://10.200.0.5:8080").
type Route struct {
	Hostname string
	Service  string
}

// Render produces cloudflared config.yml content for tunnelID, using
// credsFile as its credentials file, with one ingress rule per route in the
// order given (cloudflared matches ingress rules top to bottom, first match
// wins), followed by the mandatory catch-all rule. Callers are responsible
// for including a route to oak's own API (e.g. "oak.<domain>" ->
// "http://127.0.0.1:<api_port>") in routes if they want it reachable
// remotely; Render itself only templates whatever routes it's given plus
// the catch-all.
func Render(tunnelID, credsFile string, routes []Route) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tunnel: %s\n", tunnelID)
	fmt.Fprintf(&b, "credentials-file: %s\n", credsFile)
	b.WriteString("ingress:\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "  - hostname: %s\n    service: %s\n", r.Hostname, r.Service)
	}
	b.WriteString("  - service: http_status:404\n")
	return []byte(b.String())
}

// restartCloudflared reloads cloudflared's ingress config.
//
// ponytail: a full restart causes a ~2s ingress blip on every deploy;
// upgrade path is Cloudflare's API for remote-managed tunnel config, which
// pushes ingress changes without restarting the daemon at all. Not worth it
// until the blip actually hurts.
var restartCloudflared = func() error {
	return exec.Command("systemctl", "restart", "cloudflared").Run()
}

// writeAtomic writes data to path via a temp file + rename, so a concurrent
// reader (cloudflared reloading, or another process) never observes a
// partially written file.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("tunnel: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("tunnel: rename %s to %s: %w", tmp, path, err)
	}
	return nil
}

// Apply atomically writes data to path and restarts cloudflared so it picks
// up the new config.
func Apply(path string, data []byte) error {
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	if err := restartCloudflared(); err != nil {
		return fmt.Errorf("%w: %v", ErrRestart, err)
	}
	return nil
}
