package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBytesToRoundedMB(t *testing.T) {
	cases := []struct {
		bytes int64
		want  int
	}{
		{0, 256},               // 0 MB + 256 headroom, already a multiple of 64
		{1, 320},               // 1 byte -> 1 MB, +256 = 257, round up to 320
		{63 * 1 << 20, 320},    // 63 MB + 256 = 319, round up to 320
		{64 * 1 << 20, 320},    // 64 MB + 256 = 320, already a multiple of 64
		{100 * 1 << 20, 384},   // 100 + 256 = 356, round up to 384
		{1000 * 1 << 20, 1280}, // 1000 + 256 = 1256, round up to 1280
	}
	for _, c := range cases {
		got := bytesToRoundedMB(c.bytes)
		if got != c.want {
			t.Errorf("bytesToRoundedMB(%d) = %d, want %d", c.bytes, got, c.want)
		}
		if got%rootfsRoundMB != 0 {
			t.Errorf("bytesToRoundedMB(%d) = %d, not a multiple of %d", c.bytes, got, rootfsRoundMB)
		}
	}
}

func TestRootfsSizeMBSumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 10*1<<20), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b"), make([]byte, 5*1<<20), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := os.Symlink("a", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	gotMB, gotFiles, err := rootfsSizeMB(dir)
	if err != nil {
		t.Fatalf("rootfsSizeMB: %v", err)
	}
	// 15 MB of regular files + 256 headroom = 271, rounded up to 320.
	if gotMB != 320 {
		t.Fatalf("rootfsSizeMB size = %d, want 320", gotMB)
	}
	// "a" and "b" are regular files; "link" is a symlink and must not count.
	if gotFiles != 2 {
		t.Fatalf("rootfsSizeMB fileCount = %d, want 2", gotFiles)
	}
}
