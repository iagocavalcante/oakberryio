package mmds

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGuestJSONRoundTrip(t *testing.T) {
	g := Guest{
		MachineID:  "m1",
		App:        "hello",
		IP:         "10.200.0.5/16",
		Gateway:    "10.200.0.1",
		DNS:        "10.200.0.1",
		Env:        map[string]string{"PORT": "8080"},
		Entrypoint: []string{"/bin/app"},
		Cmd:        []string{"serve"},
		WorkingDir: "/app",
		Mounts:     []Mount{{Device: "/dev/vdb", Dest: "/data"}},
	}

	b, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Guest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(g, got) {
		t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", got, g)
	}
}

func TestArgvPrefersEntrypointThenCmd(t *testing.T) {
	g := Guest{Entrypoint: []string{"/bin/app"}, Cmd: []string{"serve", "--port", "8080"}}
	argv, err := g.Argv()
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	want := []string{"/bin/app", "serve", "--port", "8080"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}

func TestArgvEntrypointOnly(t *testing.T) {
	g := Guest{Entrypoint: []string{"/bin/app"}}
	argv, err := g.Argv()
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	if !reflect.DeepEqual(argv, []string{"/bin/app"}) {
		t.Fatalf("argv = %v", argv)
	}
}

func TestArgvCmdOnly(t *testing.T) {
	g := Guest{Cmd: []string{"/bin/app", "serve"}}
	argv, err := g.Argv()
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	if !reflect.DeepEqual(argv, []string{"/bin/app", "serve"}) {
		t.Fatalf("argv = %v", argv)
	}
}

func TestArgvErrorsWhenBothEmpty(t *testing.T) {
	g := Guest{}
	if _, err := g.Argv(); err == nil {
		t.Fatal("expected error when entrypoint and cmd are both empty")
	}
}
