package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarNames extracts the set of entry names from a tar produced by
// tarContext (directories keep their trailing "/").
func tarNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names[hdr.Name] = true
	}
	return names
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestTarContextRespectsDockerignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep")
	writeFile(t, filepath.Join(dir, "skip.txt"), "skip")
	writeFile(t, filepath.Join(dir, ".dockerignore"), "skip.txt\n")

	buf, err := tarContext(dir, "Dockerfile")
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	names := tarNames(t, buf.Bytes())

	if !names["keep.txt"] {
		t.Errorf("tar missing keep.txt: %v", names)
	}
	if names["skip.txt"] {
		t.Errorf("tar contains skip.txt, want it excluded by .dockerignore: %v", names)
	}
	if names[".dockerignore"] {
		t.Errorf("tar contains .dockerignore itself, want it always excluded: %v", names)
	}
}

func TestTarContextAlwaysExcludesGit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.txt"), "app")
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".git", "objects", "blob"), "binary junk")

	buf, err := tarContext(dir, "Dockerfile")
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	names := tarNames(t, buf.Bytes())

	for name := range names {
		if name == ".git" || name == ".git/" || strings.HasPrefix(name, ".git/") {
			t.Errorf("tar contains .git content %q, want .git always excluded", name)
		}
	}
	if !names["app.txt"] {
		t.Errorf("tar missing app.txt: %v", names)
	}
}

func TestTarContextAlwaysIncludesDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch")
	writeFile(t, filepath.Join(dir, "other.txt"), "x")
	// Phoenix's generated .dockerignore lists the Dockerfile itself; docker
	// build still sends it, so we must too.
	writeFile(t, filepath.Join(dir, ".dockerignore"), "Dockerfile\nother.txt\n")

	buf, err := tarContext(dir, "Dockerfile")
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	names := tarNames(t, buf.Bytes())
	if !names["Dockerfile"] {
		t.Errorf("tar missing Dockerfile although .dockerignore lists it: %v", names)
	}
	if names["other.txt"] {
		t.Errorf("tar contains other.txt, want it excluded: %v", names)
	}
}

func TestTarContextAllowlistDescendsForExceptions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch")
	writeFile(t, filepath.Join(dir, "package.json"), "{}")
	if err := os.MkdirAll(filepath.Join(dir, "apps", "landing", "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "apps", "mobile"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "apps", "landing", "package.json"), "{}")
	writeFile(t, filepath.Join(dir, "apps", "landing", "src", "index.ts"), "")
	writeFile(t, filepath.Join(dir, "apps", "mobile", "big.bin"), "x")
	writeFile(t, filepath.Join(dir, "README.md"), "x")
	// Allowlist style: ignore everything, re-include what the build needs.
	writeFile(t, filepath.Join(dir, ".dockerignore"), "*\n!package.json\n!apps/landing\n")

	buf, err := tarContext(dir, "Dockerfile")
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	names := tarNames(t, buf.Bytes())
	for _, want := range []string{"Dockerfile", "package.json", "apps/landing/package.json", "apps/landing/src/index.ts"} {
		if !names[want] {
			t.Errorf("tar missing %s: %v", want, names)
		}
	}
	for _, skip := range []string{"README.md", "apps/mobile/big.bin", "apps/mobile/"} {
		if names[skip] {
			t.Errorf("tar contains %s, want it excluded: %v", skip, names)
		}
	}
}
