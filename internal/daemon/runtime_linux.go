//go:build linux

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
	"github.com/iagocavalcante/oakberryio/internal/rootfs"
	"github.com/iagocavalcante/oakberryio/internal/vm"
)

// FirecrackerRuntime is the real Runtime: it pulls and flattens OCI images
// into ext4 rootfs files and boots them as Firecracker microVMs. Only
// buildable/usable on Linux, where mkfs.ext4, /dev/kvm and Firecracker
// itself exist.
type FirecrackerRuntime struct{}

// BuildRootfs pulls image, flattens it into a scratch directory, sizes and
// formats an ext4 image at out (which also installs oak-init as the
// image's PID 1, see rootfs.MakeExt4), and returns the image's runtime
// metadata.
func (FirecrackerRuntime) BuildRootfs(ctx context.Context, image, out string) (*rootfs.ImageMeta, error) {
	img, err := rootfs.Pull(image)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", image, err)
	}

	scratch, err := os.MkdirTemp("", "oak-rootfs-")
	if err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	meta, err := rootfs.Flatten(img, scratch)
	if err != nil {
		return nil, fmt.Errorf("flatten %s: %w", image, err)
	}

	sizeMB, entryCount, err := rootfsSizeMB(scratch)
	if err != nil {
		return nil, fmt.Errorf("size %s: %w", scratch, err)
	}
	// Headroom over the exact entry count: the guest may create new files at
	// runtime (logs, temp files), and ext4 can't add inodes after mkfs.
	inodeCount := entryCount*2 + 1024

	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(out), err)
	}
	if err := rootfs.MakeExt4(scratch, out, sizeMB, inodeCount); err != nil {
		return nil, fmt.Errorf("mkfs %s: %w", out, err)
	}
	return meta, nil
}

// Start boots a Firecracker microVM per spec, pushing meta into its MMDS.
func (FirecrackerRuntime) Start(ctx context.Context, spec vm.Spec, meta mmds.Guest) (Handle, error) {
	m, err := vm.Start(ctx, spec, meta)
	if err != nil {
		return nil, err
	}
	return m, nil
}
