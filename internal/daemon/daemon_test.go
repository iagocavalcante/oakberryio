package daemon

import (
	"context"
	"testing"
)

// TestNewSkipsInvalidStaticRouteHostname confirms an invalid static-route
// hostname in Config is dropped (logged, not propagated) rather than making
// it into the Deployer's StaticRoutes, mirroring appconfig.Parse's rejection
// of a malformed custom domain.
func TestNewSkipsInvalidStaticRouteHostname(t *testing.T) {
	cfg := Config{
		DataDir: t.TempDir(),
		StaticRoutes: []StaticRoute{
			{Hostname: "not a domain", Service: "http://127.0.0.1:4000"},
			{Hostname: "panel.example.com", Service: "http://127.0.0.1:4001"},
		},
	}

	d, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if len(d.Deployer.StaticRoutes) != 1 {
		t.Fatalf("StaticRoutes = %+v, want exactly the one valid route", d.Deployer.StaticRoutes)
	}
	if got := d.Deployer.StaticRoutes[0].Hostname; got != "panel.example.com" {
		t.Fatalf("StaticRoutes[0].Hostname = %q, want panel.example.com", got)
	}
}
