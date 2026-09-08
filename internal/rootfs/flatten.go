// Package rootfs turns an OCI image into a flat directory tree suitable for
// mkfs.ext4, and (on Linux) formats that tree into a rootfs image for a
// Firecracker microVM.
package rootfs

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ImageMeta is the subset of the image's OCI config needed to run it as a
// guest: how to start the process and what environment to give it.
type ImageMeta struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// Pull fetches the image at ref (e.g. "localhost:5000/hello:1699999999") from
// the plaintext local registry.
func Pull(ref string) (v1.Image, error) {
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", ref, err)
	}
	img, err := remote.Image(r, remote.WithAuthFromKeychain(nil))
	if err != nil {
		return nil, fmt.Errorf("pull image %q: %w", ref, err)
	}
	return img, nil
}

// Flatten writes img's filesystem into dir, applying each layer in order and
// honoring OCI whiteouts, and returns the image's runtime metadata.
func Flatten(img v1.Image, dir string) (*ImageMeta, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("image layers: %w", err)
	}
	for i, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return nil, fmt.Errorf("layer %d: uncompressed: %w", i, err)
		}
		err = untarLayer(rc, dir)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("layer %d: untar: %w", i, err)
		}
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	return &ImageMeta{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
	}, nil
}

const whiteoutPrefix = ".wh."
const opaqueWhiteout = ".wh..wh..opq"

// untarLayer extracts one uncompressed layer tar stream into dir, applying
// OCI whiteout conventions: a ".wh.<name>" entry deletes <name>, and a
// ".wh..wh..opq" entry in a directory clears everything already written to
// that directory by earlier layers.
func untarLayer(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		entryName := filepath.Clean(hdr.Name)
		base := filepath.Base(entryName)
		parent := filepath.Dir(entryName)

		if base == opaqueWhiteout {
			target, err := safeJoin(dir, parent)
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("read dir %s for opaque whiteout: %w", target, err)
			}
			for _, e := range entries {
				if err := os.RemoveAll(filepath.Join(target, e.Name())); err != nil {
					return fmt.Errorf("clear %s: %w", filepath.Join(target, e.Name()), err)
				}
			}
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			victim := filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))
			target, err := safeJoin(dir, victim)
			if err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("whiteout %s: %w", target, err)
			}
			continue
		}

		target, err := safeJoin(dir, entryName)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm|0700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&os.ModePerm|0600)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(dir, filepath.Clean(hdr.Linkname))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", target, linkTarget, err)
			}
		default:
			// Character/block devices, FIFOs: not needed for an app rootfs.
		}
	}
}

// safeJoin joins dir and name, rejecting any path that would escape dir
// (e.g. via ".." in a malicious tar entry).
func safeJoin(dir, name string) (string, error) {
	cleanDir := filepath.Clean(dir)
	target := filepath.Join(cleanDir, name)
	if target != cleanDir && !strings.HasPrefix(target, cleanDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry %q escapes rootfs %s", name, dir)
	}
	return target, nil
}
