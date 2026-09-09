//go:build linux

package rootfs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// MakeExt4 installs the oak-init binary into dir/sbin/oak-init (so it becomes
// the guest's PID 1), then formats dir into an ext4 filesystem image at out,
// sized sizeMB with inodeCount inodes.
func MakeExt4(dir, out string, sizeMB, inodeCount int) error {
	if err := installOakInit(dir); err != nil {
		return fmt.Errorf("install oak-init: %w", err)
	}

	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		_ = os.Remove(out)
		return fmt.Errorf("allocate %s: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", out, err)
	}

	cmd := exec.Command("mkfs.ext4", "-F", "-N", strconv.Itoa(inodeCount), "-d", dir, out)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(out)
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", out, err, outBytes)
	}
	return nil
}

// installOakInit copies the oak-init binary (path from the OAK_INIT_BIN
// environment variable) into dir/sbin/oak-init, and lays down the minimal
// skeleton oak-init needs at boot: mount points for the pseudo-filesystems
// it mounts, /dev/console so the kernel has somewhere to send early console
// output before oak-init takes over, and a real (non-symlink) /etc so
// oak-init can write resolv.conf/hosts into it.
func installOakInit(dir string) error {
	src := os.Getenv("OAK_INIT_BIN")
	if src == "" {
		return fmt.Errorf("OAK_INIT_BIN not set")
	}

	for _, d := range []string{"dev", "proc", "sys", "etc", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	console := filepath.Join(dir, "dev", "console")
	if err := unix.Mknod(console, unix.S_IFCHR|0600, int(unix.Mkdev(5, 1))); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mknod %s: %w", console, err)
	}

	// Many base images ship /etc/resolv.conf as a symlink to a path (e.g.
	// /run/systemd/resolve/stub-resolv.conf) that doesn't exist in the
	// guest. oak-init needs to write a real file there at boot.
	resolvConf := filepath.Join(dir, "etc", "resolv.conf")
	if fi, err := os.Lstat(resolvConf); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(resolvConf); err != nil {
			return fmt.Errorf("remove symlink %s: %w", resolvConf, err)
		}
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
