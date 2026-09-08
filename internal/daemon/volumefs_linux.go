//go:build linux

package daemon

import (
	"fmt"
	"os/exec"
)

// formatVolume runs mkfs.ext4 on an already-sized sparse file, for a
// freshly created app volume.
func formatVolume(path string) error {
	out, err := exec.Command("mkfs.ext4", "-F", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, out)
	}
	return nil
}
