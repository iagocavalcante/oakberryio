package tunnel

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderGoldenTwoRoutes(t *testing.T) {
	got := Render("abc-123", "/etc/cloudflared/abc-123.json", []Route{
		{Hostname: "hello.apps.example.com", Service: "http://10.200.0.2:8080"},
		{Hostname: "oak.apps.example.com", Service: "http://127.0.0.1:7000"},
	})

	want := "tunnel: abc-123\n" +
		"credentials-file: /etc/cloudflared/abc-123.json\n" +
		"ingress:\n" +
		"  - hostname: hello.apps.example.com\n" +
		"    service: http://10.200.0.2:8080\n" +
		"  - hostname: oak.apps.example.com\n" +
		"    service: http://127.0.0.1:7000\n" +
		"  - service: http_status:404\n"

	if string(got) != want {
		t.Fatalf("Render() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderEmptyRoutesStillHasCatchAll(t *testing.T) {
	got := Render("t", "creds.json", nil)
	want := "tunnel: t\ncredentials-file: creds.json\ningress:\n  - service: http_status:404\n"
	if string(got) != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestApplyWritesAtomicallyWithoutRestartingOnTestOverride(t *testing.T) {
	restarted := false
	orig := restartCloudflared
	restartCloudflared = func() error { restarted = true; return nil }
	t.Cleanup(func() { restartCloudflared = orig })

	path := filepath.Join(t.TempDir(), "config.yml")
	data := []byte("tunnel: t\n")
	if err := Apply(path, data); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !restarted {
		t.Fatal("want restartCloudflared to be called")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(data) {
		t.Fatalf("file content = %q, want %q", got, data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.tmp should not remain", path)
	}
}

func TestApplyRestartFailureIsErrRestart(t *testing.T) {
	orig := restartCloudflared
	restartCloudflared = func() error { return errors.New("boom") }
	t.Cleanup(func() { restartCloudflared = orig })

	path := filepath.Join(t.TempDir(), "config.yml")
	err := Apply(path, []byte("tunnel: t\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, ErrRestart) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRestart)", err)
	}
}
