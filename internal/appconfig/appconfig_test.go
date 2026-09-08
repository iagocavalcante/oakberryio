package appconfig

import "testing"

const sample = `
app = "hello"
[build]
dockerfile = "Dockerfile"
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
