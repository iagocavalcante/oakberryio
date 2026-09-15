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

	buf, err := tarContext(dir)
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

	buf, err := tarContext(dir)
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
