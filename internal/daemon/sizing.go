package daemon

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

const (
	// rootfsHeadroomMB is extra space added on top of the flattened image's
	// own size, so the guest has room to write logs, temp files, etc.
	rootfsHeadroomMB = 256
	// rootfsRoundMB is the granularity the final size is rounded up to.
	rootfsRoundMB = 64
)

// rootfsSizeMB walks dir (a flattened OCI image tree) and returns the ext4
// image size to allocate for it (total regular-file bytes, converted to MB,
// plus rootfsHeadroomMB, rounded up to the next multiple of rootfsRoundMB)
// alongside the total number of filesystem entries found (regular files,
// directories and symlinks alike -- every one of them costs an inode), which
// the caller uses to size mkfs.ext4's inode count. Only regular-file bytes
// count toward the size, since directories and symlinks take negligible
// space but still need their own inode.
func rootfsSizeMB(dir string) (sizeMB int, entryCount int, err error) {
	var totalBytes int64
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entryCount++
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			totalBytes += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("walk %s: %w", dir, walkErr)
	}
	return bytesToRoundedMB(totalBytes), entryCount, nil
}

// bytesToRoundedMB converts a byte count to whole megabytes (rounded up),
// adds rootfsHeadroomMB, then rounds up to the next multiple of
// rootfsRoundMB.
func bytesToRoundedMB(totalBytes int64) int {
	const mib = 1 << 20
	mb := (totalBytes + mib - 1) / mib
	mb += rootfsHeadroomMB
	if rem := mb % rootfsRoundMB; rem != 0 {
		mb += rootfsRoundMB - rem
	}
	return int(mb)
}
