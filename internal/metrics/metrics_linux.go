//go:build linux

package metrics

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// linuxReader reads real counters from /proc and statfs. Parsing lives in
// metrics.go (no build tag) so it's testable without a real /proc; this
// file is only file I/O and the statfs syscall.
type linuxReader struct{}

func newReader() reader { return linuxReader{} }

func (linuxReader) hostCPU() (idle, total uint64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	return parseProcStat(data)
}

func (linuxReader) memInfo() (usedBytes, totalBytes uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseMemInfo(data)
}

func (linuxReader) diskUsage(dir string) (usedBytes, totalBytes uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bsize and the block counts are architecture-dependent unsigned/signed
	// integer widths across GOARCH; convert through uint64 explicitly
	// rather than relying on an untyped constant conversion.
	totalBytes = st.Blocks * uint64(st.Bsize)
	freeBytes := st.Bfree * uint64(st.Bsize)
	return totalBytes - freeBytes, totalBytes, nil
}

func (linuxReader) load1() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, fmt.Errorf("read /proc/loadavg: %w", err)
	}
	return parseLoadAvg(data)
}

func (linuxReader) uptime() (uint64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("read /proc/uptime: %w", err)
	}
	return parseUptime(data)
}

// pidCPU returns pid's utime+stime jiffies. A pid that has already exited
// (its VM shut down between the store's snapshot and this read) is not an
// error: ok is false so Sample zeroes that machine's CPU% instead of
// failing the whole snapshot.
func (linuxReader) pidCPU(pid int) (ticks uint64, ok bool, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	ticks, err = parsePidStat(data)
	if err != nil {
		return 0, false, err
	}
	return ticks, true, nil
}

// pidMem returns pid's resident set size, or 0 with no error if pid has
// already exited -- same reasoning as pidCPU.
func (linuxReader) pidMem(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read /proc/%d/status: %w", pid, err)
	}
	return parsePidStatus(data)
}
