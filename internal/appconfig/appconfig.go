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
// obviously malformed entries in oak.toml before they're templated into
// cloudflared's ingress config.
var domainRe = regexp.MustCompile(`^[a-z0-9-]{1,63}(\.[a-z0-9-]{1,63})+$`)

// ValidName reports whether name is a valid app name: lowercase letters,
// digits and dashes, max 32 characters. Shared with internal/daemon/api.go
// so path-derived app names get the same validation as oak.toml's app field
// before they're used to build filesystem paths or SQL lookups.
func ValidName(name string) bool {
	return nameRe.MatchString(name)
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
		if !domainRe.MatchString(d) {
			return nil, fmt.Errorf("domain %q: must be a valid hostname (lowercase letters, digits, hyphens, dot-separated labels)", d)
		}
	}
	return c, nil
}
