package appconfig

import "testing"

const sample = `
app = "hello"
[build]
dockerfile = "Dockerfile"
[build.args]
NEXT_PUBLIC_API_URL = "https://api.example.com"
[env]
PORT = "8080"
[[services]]
internal_port = 8080
[services.check]
path = "/health"
[[mounts]]
volume = "data"
destination = "/data"
[vm]
memory_mb = 256
cpus = 1
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.App != "hello" {
		t.Fatalf("app=%q", c.App)
	}
	if c.Services[0].InternalPort != 8080 {
		t.Fatal("port")
	}
	if c.Services[0].Check.Path != "/health" {
		t.Fatal("check")
	}
	if c.Mounts[0].Destination != "/data" {
		t.Fatal("mount")
	}
	if c.VM.MemoryMB != 256 {
		t.Fatal("mem")
	}
	if c.Build.Args["NEXT_PUBLIC_API_URL"] != "https://api.example.com" {
		t.Fatalf("build args = %+v", c.Build.Args)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`app = "x"`))
	if err != nil {
		t.Fatal(err)
	}
	if c.VM.MemoryMB != 256 || c.VM.CPUs != 1 {
		t.Fatal("defaults")
	}
}

func TestRejectsBadName(t *testing.T) {
	if _, err := Parse([]byte(`app = "Bad Name"`)); err == nil {
		t.Fatal("want error")
	}
}

func TestParsesCustomDomains(t *testing.T) {
	c, err := Parse([]byte(`
app = "hello"
domains = ["misesnag.app", "www.misesnag.app"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Domains) != 2 || c.Domains[0] != "misesnag.app" || c.Domains[1] != "www.misesnag.app" {
		t.Fatalf("domains = %v", c.Domains)
	}
}

func TestRejectsBadDomain(t *testing.T) {
	if _, err := Parse([]byte(`
app = "hello"
domains = ["not a domain"]
`)); err == nil {
		t.Fatal("want error")
	}
}
