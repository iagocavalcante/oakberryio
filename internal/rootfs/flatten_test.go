package rootfs

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// tarEntry is either a regular file (Content != nil) or a whiteout marker
// (Content == nil, Whiteout is the ".wh."-prefixed name to write).
type tarEntry struct {
	Name    string
	Content string
	Dir     bool
}

func buildLayer(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.Dir {
			if err := tw.WriteHeader(&tar.Header{Name: e.Name, Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
				t.Fatalf("write dir header %s: %v", e.Name, err)
			}
			continue
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Typeflag: tar.TypeReg,
			Mode:     0644,
			Size:     int64(len(e.Content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.Name, err)
		}
		if _, err := tw.Write([]byte(e.Content)); err != nil {
			t.Fatalf("write content %s: %v", e.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return static.NewLayer(buf.Bytes(), types.OCIUncompressedLayer)
}

func TestFlattenAppliesWhiteoutsAndReturnsMeta(t *testing.T) {
	base, err := random.Image(0, 0)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	layer1 := buildLayer(t, []tarEntry{
		{Name: "a", Content: "layer1-a"},
		{Name: "dir", Dir: true},
		{Name: "dir/x", Content: "layer1-dir-x"},
	})
	layer2 := buildLayer(t, []tarEntry{
		{Name: ".wh.a", Content: ""},
		{Name: "b", Content: "layer2-b"},
	})

	img, err := mutate.AppendLayers(base, layer1, layer2)
	if err != nil {
		t.Fatalf("append layers: %v", err)
	}

	wantCfg := v1.Config{
		Entrypoint: []string{"/bin/app"},
		Cmd:        []string{"serve"},
		Env:        []string{"PORT=8080"},
		WorkingDir: "/app",
	}
	img, err = mutate.Config(img, wantCfg)
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}

	dir := t.TempDir()
	meta, err := Flatten(img, dir)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("expected /a to be whited out, stat err = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "b")); err != nil || string(b) != "layer2-b" {
		t.Fatalf("/b = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "dir/x")); err != nil || string(b) != "layer1-dir-x" {
		t.Fatalf("/dir/x = %q, %v", b, err)
	}

	wantMeta := &ImageMeta{
		Entrypoint: wantCfg.Entrypoint,
		Cmd:        wantCfg.Cmd,
		Env:        wantCfg.Env,
		WorkingDir: wantCfg.WorkingDir,
	}
	if !reflect.DeepEqual(meta, wantMeta) {
		t.Fatalf("meta = %+v, want %+v", meta, wantMeta)
	}
}

func TestFlattenOpaqueWhiteoutClearsDir(t *testing.T) {
	base, err := random.Image(0, 0)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	layer1 := buildLayer(t, []tarEntry{
		{Name: "dir", Dir: true},
		{Name: "dir/x", Content: "x"},
		{Name: "dir/y", Content: "y"},
	})
	layer2 := buildLayer(t, []tarEntry{
		{Name: "dir/.wh..wh..opq", Content: ""},
		{Name: "dir/z", Content: "z"},
	})

	img, err := mutate.AppendLayers(base, layer1, layer2)
	if err != nil {
		t.Fatalf("append layers: %v", err)
	}

	dir := t.TempDir()
	if _, err := Flatten(img, dir); err != nil {
		t.Fatalf("flatten: %v", err)
	}

	for _, gone := range []string{"dir/x", "dir/y"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be cleared by opaque whiteout, err = %v", gone, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "dir/z")); err != nil || string(b) != "z" {
		t.Fatalf("dir/z = %q, %v", b, err)
	}
}

func TestUntarLayerRejectsPathTraversal(t *testing.T) {
	layer := buildLayer(t, []tarEntry{
		{Name: "../../etc/passwd", Content: "pwned"},
	})
	dir := t.TempDir()
	rc, err := layer.Uncompressed()
	if err != nil {
		t.Fatalf("uncompressed: %v", err)
	}
	defer rc.Close()

	if err := untarLayer(rc, dir); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}
