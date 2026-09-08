package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewTransportUsesUnixSocketByDefault(t *testing.T) {
	// macOS's sun_path is limited to ~104 bytes; t.TempDir() nests too deep
	// for that, so use a short path directly under /tmp instead.
	sock := fmt.Sprintf("/tmp/oak-test-%d.sock", os.Getpid())
	defer os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var gotAuth string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	t.Setenv("OAK_SOCKET", sock)
	t.Setenv("OAK_API", "")

	tr, err := NewTransport()
	if err != nil {
		t.Fatal(err)
	}
	if tr.BaseURL != "http://unix" {
		t.Fatalf("BaseURL = %q, want http://unix", tr.BaseURL)
	}

	req, err := tr.NewRequest(context.Background(), http.MethodGet, "/apps", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header over the unix socket, got %q", gotAuth)
	}
}

func TestNewTransportUsesHTTPSWithBearerTokenWhenOAKAPISet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OAK_API", srv.URL)
	t.Setenv("OAK_TOKEN", "s3cr3t")

	tr, err := NewTransport()
	if err != nil {
		t.Fatal(err)
	}
	if tr.BaseURL != srv.URL {
		t.Fatalf("BaseURL = %q, want %q", tr.BaseURL, srv.URL)
	}

	req, err := tr.NewRequest(context.Background(), http.MethodGet, "/apps", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
}

func TestNewTransportFallsBackToTokenFile(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".oak"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".oak", "token"), []byte("from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("OAK_API", srv.URL)
	t.Setenv("OAK_TOKEN", "")

	tr, err := NewTransport()
	if err != nil {
		t.Fatal(err)
	}
	req, err := tr.NewRequest(context.Background(), http.MethodGet, "/apps", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer from-file" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer from-file")
	}
}
