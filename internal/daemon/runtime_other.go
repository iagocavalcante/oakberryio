//go:build !linux

package daemon

import (
	"context"
	"errors"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
	"github.com/iagocavalcante/oakberryio/internal/rootfs"
	"github.com/iagocavalcante/oakberryio/internal/vm"
)

var errRuntimeLinuxOnly = errors.New("daemon: FirecrackerRuntime is linux only")

// FirecrackerRuntime is only implemented on Linux, where Firecracker,
// mkfs.ext4 and /dev/kvm actually exist. It still exists on other platforms
// so the daemon package builds everywhere and can be wired up in tests
// (against fakes, never this type) -- every method here just fails.
type FirecrackerRuntime struct{}

// BuildRootfs always fails on non-Linux hosts.
func (FirecrackerRuntime) BuildRootfs(ctx context.Context, image, out string) (*rootfs.ImageMeta, error) {
	return nil, errRuntimeLinuxOnly
}

// Start always fails on non-Linux hosts.
func (FirecrackerRuntime) Start(ctx context.Context, spec vm.Spec, meta mmds.Guest) (Handle, error) {
	return nil, errRuntimeLinuxOnly
}
