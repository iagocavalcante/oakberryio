// Package metrics samples host and per-VM resource usage for oakd's
// read-only control panel (GET /metrics; see
// docs/plans/2026-09-15-control-panel-design.md).
//
// Reading /proc and statfs is Linux-only, so the raw-counter source sits
// behind the reader interface: linuxReader (metrics_linux.go) implements it
// for real, otherReader (metrics_other.go) stubs it with zero values,
// mirroring how internal/vm splits its Linux-only Firecracker driver from
// its cross-platform Spec type. Everything in this file -- the types, the
// text parsing, and the CPU%-from-two-samples math -- has no platform
// dependency and is fully testable on darwin.
package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HostMetrics is a snapshot of the box's own resource usage.
type HostMetrics struct {
	CPUPct         float64 `json:"cpu_pct"`
	MemUsedBytes   uint64  `json:"mem_used_bytes"`
	MemTotalBytes  uint64  `json:"mem_total_bytes"`
	DiskUsedBytes  uint64  `json:"disk_used_bytes"`
	DiskTotalBytes uint64  `json:"disk_total_bytes"`
	Load1          float64 `json:"load1"`
	UptimeSecs     uint64  `json:"uptime_secs"`
	VMCount        int     `json:"vm_count"`
}

// MachineMetrics is a snapshot of one running microVM's resource usage.
type MachineMetrics struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemBytes uint64  `json:"mem_bytes"`
}

// Snapshot is one Sample call's result: host metrics plus per-machine
// metrics keyed by machine id.
type Snapshot struct {
	Host     HostMetrics
	Machines map[string]MachineMetrics
}

// MachinePID is a running machine's id and the host pid backing it -- the
// slice of these is Sample's input, built by the daemon from the store's
// running machines (see store.Machine.PID).
type MachinePID struct {
	ID  string
	PID int
}

// clockTicksPerSecond is Linux's USER_HZ: the unit /proc/<pid>/stat's
// utime/stime fields are counted in. It's a compile-time kernel constant
// that has been 100 on every mainstream distribution/architecture oak
// targets (x86_64, arm64) for decades, so it's hardcoded here rather than
// discovered at runtime -- golang.org/x/sys/unix has no portable
// sysconf(_SC_CLK_TCK) equivalent short of cgo.
const clockTicksPerSecond = 100

// reader is the platform-specific source of raw counters. See the package
// doc comment.
type reader interface {
	hostCPU() (idle, total uint64, err error)
	memInfo() (usedBytes, totalBytes uint64, err error)
	diskUsage(dir string) (usedBytes, totalBytes uint64, err error)
	load1() (float64, error)
	uptime() (uint64, error)
	// pidCPU returns pid's utime+stime jiffies. ok is false, with no error,
	// when pid no longer exists -- a VM that exited between the store's
	// snapshot and this read is not a sampling failure.
	pidCPU(pid int) (ticks uint64, ok bool, err error)
	// pidMem returns pid's resident set size, or 0 with no error if pid no
	// longer exists.
	pidMem(pid int) (bytes uint64, err error)
}

// rawSample is one Sample call's raw counters, kept so the next call can
// compute deltas against it.
type rawSample struct {
	at        time.Time
	hostIdle  uint64
	hostTotal uint64
	pidTicks  map[string]uint64 // by machine id; absent means that pid was gone
}

// Sampler computes resource-usage snapshots for the host and its running
// microVMs. CPU% needs two points in time -- a single /proc/stat or
// /proc/<pid>/stat read carries only cumulative counters since boot, not a
// rate -- so Sampler holds the previous raw sample and each Sample call
// computes the delta against it. The daemon calls Sample on a ticker (see
// daemon.go's metrics goroutine); the very first call has no previous
// sample to diff against, so it reports 0% CPU everywhere.
//
// A Sampler is safe for concurrent use: Sample may run in the sampling
// goroutine while the API reads the previously published Snapshot (which
// the daemon, not Sampler, is responsible for storing -- see daemon.go).
type Sampler struct {
	// DataDir is statfs'd for host disk usage.
	DataDir string

	reader reader

	mu   sync.Mutex
	prev *rawSample
}

// NewSampler returns a Sampler that reports disk usage for dataDir (oakd's
// data directory, e.g. the Deployer's DataDir).
func NewSampler(dataDir string) *Sampler {
	return &Sampler{DataDir: dataDir, reader: newReader()}
}

// Sample reads current host and per-machine counters and returns the
// resulting snapshot, with CPU% computed as the delta against the previous
// Sample call. machines is the current set of running microVMs; a machine
// whose pid has already exited reports zero CPU and memory rather than
// failing the whole snapshot.
func (s *Sampler) Sample(machines []MachinePID) (Snapshot, error) {
	now := time.Now()

	hostIdle, hostTotal, err := s.reader.hostCPU()
	if err != nil {
		return Snapshot{}, fmt.Errorf("metrics: host cpu: %w", err)
	}
	memUsed, memTotal, err := s.reader.memInfo()
	if err != nil {
		return Snapshot{}, fmt.Errorf("metrics: mem info: %w", err)
	}
	diskUsed, diskTotal, err := s.reader.diskUsage(s.DataDir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("metrics: disk usage %s: %w", s.DataDir, err)
	}
	load1, err := s.reader.load1()
	if err != nil {
		return Snapshot{}, fmt.Errorf("metrics: load average: %w", err)
	}
	uptime, err := s.reader.uptime()
	if err != nil {
		return Snapshot{}, fmt.Errorf("metrics: uptime: %w", err)
	}

	pidTicksNow := make(map[string]uint64, len(machines))
	machineMetrics := make(map[string]MachineMetrics, len(machines))
	for _, m := range machines {
		ticks, ok, err := s.reader.pidCPU(m.PID)
		if err != nil {
			return Snapshot{}, fmt.Errorf("metrics: pid %d cpu: %w", m.PID, err)
		}
		mem, err := s.reader.pidMem(m.PID)
		if err != nil {
			return Snapshot{}, fmt.Errorf("metrics: pid %d mem: %w", m.PID, err)
		}
		if ok {
			pidTicksNow[m.ID] = ticks
		}
		machineMetrics[m.ID] = MachineMetrics{MemBytes: mem}
	}

	host := HostMetrics{
		MemUsedBytes:   memUsed,
		MemTotalBytes:  memTotal,
		DiskUsedBytes:  diskUsed,
		DiskTotalBytes: diskTotal,
		Load1:          load1,
		UptimeSecs:     uptime,
		VMCount:        len(machines),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev != nil {
		elapsed := now.Sub(s.prev.at)
		host.CPUPct = hostCPUPercent(s.prev.hostIdle, s.prev.hostTotal, hostIdle, hostTotal)
		for id, ticks := range pidTicksNow {
			prevTicks, ok := s.prev.pidTicks[id]
			if !ok {
				continue // this machine wasn't running (or wasn't sampled) last time
			}
			mm := machineMetrics[id]
			mm.CPUPct = pidCPUPercent(prevTicks, ticks, elapsed)
			machineMetrics[id] = mm
		}
	}
	s.prev = &rawSample{at: now, hostIdle: hostIdle, hostTotal: hostTotal, pidTicks: pidTicksNow}

	return Snapshot{Host: host, Machines: machineMetrics}, nil
}

// hostCPUPercent computes host CPU utilization from two raw /proc/stat
// readings, using the standard top/htop formula: %busy = 1 - deltaIdle /
// deltaTotal, where idle includes iowait. Both deltas are jiffie counts
// over the same wall-clock interval, so the interval itself cancels out --
// unlike a single process's ticks, no elapsed-time term is needed here.
func hostCPUPercent(prevIdle, prevTotal, idle, total uint64) float64 {
	if total <= prevTotal || idle < prevIdle {
		return 0 // no time has passed, or a counter rolled over -- report nothing rather than garbage
	}
	deltaTotal := total - prevTotal
	deltaIdle := idle - prevIdle
	if deltaIdle > deltaTotal {
		return 0
	}
	return (1 - float64(deltaIdle)/float64(deltaTotal)) * 100
}

// pidCPUPercent computes one process's CPU% from two raw utime+stime
// readings (in clock ticks) and the wall-clock time elapsed between them.
func pidCPUPercent(prevTicks, ticks uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || ticks < prevTicks {
		return 0
	}
	return float64(ticks-prevTicks) / clockTicksPerSecond / elapsed.Seconds() * 100
}

// parseProcStat parses /proc/stat's aggregate "cpu" line into idle and
// total jiffies: idle = idle + iowait; total sums every field except guest
// and guest_nice, which Linux already counts inside user/nice.
func parseProcStat(data []byte) (idle, total uint64, err error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] != "cpu" {
			continue
		}
		vals := make([]uint64, len(fields)-1)
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse /proc/stat field %q: %w", f, err)
			}
			vals[i] = v
		}
		// user nice system idle iowait irq softirq steal [guest guest_nice]
		idle = vals[3] + vals[4]
		for i, v := range vals {
			if i == 8 || i == 9 {
				continue
			}
			total += v
		}
		return idle, total, nil
	}
	return 0, 0, fmt.Errorf("parse /proc/stat: no aggregate cpu line")
}

// parseMemInfo parses /proc/meminfo for MemTotal and MemAvailable, reporting
// used = total - available (the same definition `free -m` uses since
// MemAvailable was added in Linux 3.14).
func parseMemInfo(data []byte) (usedBytes, totalBytes uint64, err error) {
	var totalKB, availKB uint64
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			totalKB, haveTotal = v, true
		case "MemAvailable":
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
			availKB, haveAvail = v, true
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("parse /proc/meminfo: missing MemTotal or MemAvailable")
	}
	if availKB > totalKB {
		availKB = totalKB
	}
	return (totalKB - availKB) * 1024, totalKB * 1024, nil
}

// parseLoadAvg parses /proc/loadavg's first field, the 1-minute load
// average.
func parseLoadAvg(data []byte) (float64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("parse /proc/loadavg: empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/loadavg load1 %q: %w", fields[0], err)
	}
	return v, nil
}

// parseUptime parses /proc/uptime's first field, seconds since boot.
func parseUptime(data []byte) (uint64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("parse /proc/uptime: empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime %q: %w", fields[0], err)
	}
	return uint64(v), nil
}

// parsePidStat parses /proc/<pid>/stat's utime+stime fields (14 and 15,
// 1-indexed including pid and comm). comm (field 2) is parenthesized and
// may itself contain spaces or parens, so the split point is the *last*
// ')' in the line, not a naive Fields split.
func parsePidStat(data []byte) (ticks uint64, err error) {
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, fmt.Errorf("parse pid stat: no comm field")
	}
	rest := strings.Fields(s[end+1:])
	// rest starts at field 3 (state); utime is field 14 -> index 11, stime
	// is field 15 -> index 12.
	if len(rest) < 13 {
		return 0, fmt.Errorf("parse pid stat: too few fields after comm")
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse pid stat utime: %w", err)
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse pid stat stime: %w", err)
	}
	return utime + stime, nil
}

// parsePidStatus parses /proc/<pid>/status's VmRSS line into bytes. Returns
// 0, no error, if the line is absent (oak never boots a process without an
// RSS, but there's no reason to fail the sample over it).
func parsePidStatus(data []byte) (rssBytes uint64, err error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "VmRSS:" {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, nil
}
