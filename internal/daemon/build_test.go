package daemon

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tarOf builds a tar stream from a single entry, for the extraction guard
// tests below -- they only need one adversarial entry, not a realistic
// build context.
func tarOf(t *testing.T, name string, typ byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Typeflag: typ, Mode: 0644, Size: 0}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBuildContextRejectsParentEscape(t *testing.T) {
	dir := t.TempDir()
	data := tarOf(t, "../escape", tar.TypeReg)
	if err := extractBuildContext(bytes.NewReader(data), dir); err == nil {
		t.Fatal("extractBuildContext: want error for a \"../escape\" entry, got nil")
	}
}

func TestExtractBuildContextRejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	data := tarOf(t, "/etc/passwd", tar.TypeReg)
	if err := extractBuildContext(bytes.NewReader(data), dir); err == nil {
		t.Fatal("extractBuildContext: want error for an absolute-path entry, got nil")
	}
}

func TestExtractBuildContextAcceptsNormalTree(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeFile := func(name, content string) {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "sub", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatalf("write dir header: %v", err)
	}
	writeFile("Dockerfile", "FROM scratch\n")
	writeFile("sub/app.txt", "hello\n")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	if err := extractBuildContext(&buf, dir); err != nil {
		t.Fatalf("extractBuildContext: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "sub", "app.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("extracted content = %q, want %q", got, "hello\n")
	}
	if _, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatalf("read extracted Dockerfile: %v", err)
	}
}

func TestBuildDockerBuildArgs(t *testing.T) {
	got := buildDockerBuildArgs(
		"localhost:5000/hello:123",
		"/tmp/ctx/Dockerfile",
		"/tmp/ctx",
		map[string]string{"B": "2", "A": "1"},
	)
	want := []string{
		"build", "--platform", "linux/amd64",
		"-f", "/tmp/ctx/Dockerfile",
		"--build-arg", "A=1",
		"--build-arg", "B=2",
		"-t", "localhost:5000/hello:123",
		"/tmp/ctx",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildDockerBuildArgs = %#v, want %#v", got, want)
	}
}

func TestBuildDockerBuildArgsNoBuildArgs(t *testing.T) {
	got := buildDockerBuildArgs("img:1", "/ctx/Dockerfile", "/ctx", nil)
	want := []string{"build", "--platform", "linux/amd64", "-f", "/ctx/Dockerfile", "-t", "img:1", "/ctx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildDockerBuildArgs = %#v, want %#v", got, want)
	}
}

func TestParseBuildArgsHeader(t *testing.T) {
	got, err := parseBuildArgsHeader(`{"A":"1","B":"2"}`)
	if err != nil {
		t.Fatalf("parseBuildArgsHeader: %v", err)
	}
	want := map[string]string{"A": "1", "B": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseBuildArgsHeader = %#v, want %#v", got, want)
	}
}

func TestParseBuildArgsHeaderEmpty(t *testing.T) {
	got, err := parseBuildArgsHeader("")
	if err != nil {
		t.Fatalf("parseBuildArgsHeader: %v", err)
	}
	if got != nil {
		t.Fatalf("parseBuildArgsHeader(\"\") = %#v, want nil", got)
	}
}

func TestParseBuildArgsHeaderInvalidJSON(t *testing.T) {
	if _, err := parseBuildArgsHeader("not json"); err == nil {
		t.Fatal("parseBuildArgsHeader: want error for invalid JSON, got nil")
	}
}
