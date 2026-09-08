//go:build !linux

package rootfs

import "errors"

// MakeExt4 is only implemented on Linux, where mkfs.ext4 and the Firecracker
// host actually run.
func MakeExt4(dir, out string, sizeMB int) error {
	return errors.New("rootfs.MakeExt4: linux only")
}
