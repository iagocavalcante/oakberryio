//go:build !linux

package daemon

import "errors"

// formatVolume is only implemented on Linux, where mkfs.ext4 runs.
func formatVolume(path string) error {
	return errors.New("daemon: formatVolume: linux only")
}
