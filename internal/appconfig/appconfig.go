package appconfig

import (
	"fmt"
	"regexp"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

type Config struct {
	App      string            `toml:"app"`
	Build    Build             `toml:"build"`
	Image    string            `toml:"image"`
	Env      map[string]string `toml:"env"`
	Services []Service         `toml:"services"`
	Mounts   []Mount           `toml:"mounts"`
	VM       VM                `toml:"vm"`
}
type Build struct {
	Dockerfile string `toml:"dockerfile"`
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
	if !nameRe.MatchString(c.App) {
		return nil, fmt.Errorf("app name %q: lowercase letters, digits, dashes, max 32", c.App)
	}
	if c.Build.Dockerfile == "" {
		c.Build.Dockerfile = "Dockerfile"
	}
	return c, nil
}
