//go:build linux

package rootfs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// MakeExt4 installs the oak-init binary into dir/sbin/oak-init (so it becomes
// the guest's PID 1), then formats dir into an ext4 filesystem image at out,
// sized sizeMB.
func MakeExt4(dir, out string, sizeMB int) error {
	if err := installOakInit(dir); err != nil {
		return fmt.Errorf("install oak-init: %w", err)
	}

	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		return fmt.Errorf("allocate %s: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", out, err)
	}

	cmd := exec.Command("mkfs.ext4", "-F", "-d", dir, out)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", out, err, outBytes)
	}
	return nil
}

// installOakInit copies the oak-init binary (path from the OAK_INIT_BIN
// environment variable) into dir/sbin/oak-init.
func installOakInit(dir string) error {
	src := os.Getenv("OAK_INIT_BIN")
	if src == "" {
		return fmt.Errorf("OAK_INIT_BIN not set")
	}
	sbin := filepath.Join(dir, "sbin")
	if err := os.MkdirAll(sbin, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", sbin, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	dst := filepath.Join(sbin, "oak-init")
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy oak-init to %s: %w", dst, err)
	}
	return out.Close()
}
