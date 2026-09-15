package appconfig

import (
	"fmt"
	"regexp"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// domainRe matches a plausible DNS hostname: lowercase letters, digits and
// hyphens in each dot-separated label (1-63 chars per label), at least one
// dot. It's intentionally permissive about real-world DNS rules (no check
// that labels don't start/end with a hyphen) since this only guards against
// obviously malformed entries before they're templated into cloudflared's
// ingress config. See ValidDomain.
var domainRe = regexp.MustCompile(`^[a-z0-9-]{1,63}(\.[a-z0-9-]{1,63})+$`)

// ValidName reports whether name is a valid app name: lowercase letters,
// digits and dashes, max 32 characters. Shared with internal/daemon/api.go
// so path-derived app names get the same validation as oak.toml's app field
// before they're used to build filesystem paths or SQL lookups.
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// ValidDomain reports whether d is a plausible DNS hostname (see domainRe).
// Shared with internal/daemon so a static route's hostname gets the same
// validation as an app's custom Domains before being templated into
// cloudflared's ingress config.
func ValidDomain(d string) bool {
	return domainRe.MatchString(d)
}

type Config struct {
	App      string            `toml:"app"`
	Build    Build             `toml:"build"`
	Image    string            `toml:"image"`
	Env      map[string]string `toml:"env"`
	Services []Service         `toml:"services"`
	Mounts   []Mount           `toml:"mounts"`
	VM       VM                `toml:"vm"`
	// Domains lists extra public hostnames that should route to this app's
	// service, in addition to the default "<app>.<OAK_DOMAIN>". Each one is
	// arbitrary -- it need not be a subdomain of OAK_DOMAIN -- so the
	// operator is responsible for pointing its DNS at the tunnel and for
	// TLS coverage in that domain's own Cloudflare zone.
	Domains []string `toml:"domains"`
	Deploy  Deploy   `toml:"deploy"`
}
type Build struct {
	Dockerfile string            `toml:"dockerfile"`
	Args       map[string]string `toml:"args"`
}
type Service struct {
	InternalPort int   `toml:"internal_port"`
	Check        Check `toml:"check"`
}
type Check struct {
	Path string `toml:"path"`
}
type Mount struct {
	Volume      string `toml:"volume"`
	Destination string `toml:"destination"`
}
type VM struct {
	MemoryMB int `toml:"memory_mb"`
	CPUs     int `toml:"cpus"`
}
type Deploy struct {
	// ReleaseCommand, if non-empty, is run to completion in a transient
	// microVM before each Deploy boots the app's new machine (e.g. fly.toml's
	// release_command equivalent -- database migrations). Empty means no
	// release command; never run on Reconcile/Restart/Scale, only on Deploy.
	// See Deployer.runReleaseCommand.
	ReleaseCommand string `toml:"release_command"`
}

func Parse(b []byte) (*Config, error) {
	c := &Config{VM: VM{MemoryMB: 256, CPUs: 1}}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse oak.toml: %w", err)
	}
	if !ValidName(c.App) {
		return nil, fmt.Errorf("app name %q: lowercase letters, digits, dashes, max 32", c.App)
	}
	if c.Build.Dockerfile == "" {
		c.Build.Dockerfile = "Dockerfile"
	}
	for _, d := range c.Domains {
		if !ValidDomain(d) {
			return nil, fmt.Errorf("domain %q: must be a valid hostname (lowercase letters, digits, hyphens, dot-separated labels)", d)
		}
	}
	return c, nil
}
