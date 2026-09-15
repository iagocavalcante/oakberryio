package metrics

import (
	"testing"
	"time"
)

const sampleProcStat = `cpu  132559 2 22881 2119421 3213 0 1345 0 0 0
cpu0 132559 2 22881 2119421 3213 0 1345 0 0 0
intr 12345 0 0 0
ctxt 98765
btime 1700000000
processes 4321
`

func TestParseProcStat(t *testing.T) {
	idle, total, err := parseProcStat([]byte(sampleProcStat))
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	wantIdle := uint64(2119421 + 3213)
	wantTotal := uint64(132559 + 2 + 22881 + 2119421 + 3213 + 0 + 1345 + 0) // excludes guest/guest_nice
	if idle != wantIdle {
		t.Errorf("idle = %d, want %d", idle, wantIdle)
	}
	if total != wantTotal {
		t.Errorf("total = %d, want %d", total, wantTotal)
	}
}

func TestParseProcStatNoCPULine(t *testing.T) {
	if _, _, err := parseProcStat([]byte("intr 1 2 3\n")); err == nil {
		t.Fatal("expected error for missing cpu line")
	}
}

const sampleMemInfo = `MemTotal:       16384000 kB
MemFree:         2048000 kB
MemAvailable:    8192000 kB
Buffers:          512000 kB
Cached:          4096000 kB
`

func TestParseMemInfo(t *testing.T) {
	used, total, err := parseMemInfo([]byte(sampleMemInfo))
	if err != nil {
		t.Fatalf("parseMemInfo: %v", err)
	}
	wantTotal := uint64(16384000) * 1024
	wantUsed := uint64(16384000-8192000) * 1024
	if total != wantTotal {
		t.Errorf("total = %d, want %d", total, wantTotal)
	}
	if used != wantUsed {
		t.Errorf("used = %d, want %d", used, wantUsed)
	}
}

func TestParseMemInfoMissingField(t *testing.T) {
	if _, _, err := parseMemInfo([]byte("MemTotal: 16384000 kB\n")); err == nil {
		t.Fatal("expected error for missing MemAvailable")
	}
}

func TestParseLoadAvg(t *testing.T) {
	load1, err := parseLoadAvg([]byte("0.52 0.58 0.59 2/456 12345\n"))
	if err != nil {
		t.Fatalf("parseLoadAvg: %v", err)
	}
	if load1 != 0.52 {
		t.Errorf("load1 = %v, want 0.52", load1)
	}
}

func TestParseUptime(t *testing.T) {
	uptime, err := parseUptime([]byte("123456.78 98765.43\n"))
	if err != nil {
		t.Fatalf("parseUptime: %v", err)
	}
	if uptime != 123456 {
		t.Errorf("uptime = %d, want 123456", uptime)
	}
}

func TestParsePidStat(t *testing.T) {
	// comm field deliberately contains a space and parens to exercise the
	// last-')' split.
	line := "4321 (oak init (fc)) S 1 4321 4321 0 -1 4194560 100 0 0 0 200 50 0 0 20 0 1 0 12345 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0\n"
	ticks, err := parsePidStat([]byte(line))
	if err != nil {
		t.Fatalf("parsePidStat: %v", err)
	}
	if ticks != 200+50 {
		t.Errorf("ticks = %d, want %d", ticks, 250)
	}
}

func TestParsePidStatTooShort(t *testing.T) {
	if _, err := parsePidStat([]byte("4321 (oak) S 1\n")); err == nil {
		t.Fatal("expected error for too few fields")
	}
}

func TestParsePidStatus(t *testing.T) {
	status := "Name:\toak-init\nVmPeak:\t   20000 kB\nVmRSS:\t   12345 kB\nVmSize:\t   20000 kB\n"
	rss, err := parsePidStatus([]byte(status))
	if err != nil {
		t.Fatalf("parsePidStatus: %v", err)
	}
	if want := uint64(12345 * 1024); rss != want {
		t.Errorf("rss = %d, want %d", rss, want)
	}
}

func TestParsePidStatusMissingVmRSS(t *testing.T) {
	rss, err := parsePidStatus([]byte("Name:\toak-init\n"))
	if err != nil {
		t.Fatalf("parsePidStatus: %v", err)
	}
	if rss != 0 {
		t.Errorf("rss = %d, want 0", rss)
	}
}

func TestHostCPUPercentDelta(t *testing.T) {
	// 1s of wall time, 200 total jiffies elapsed, 50 of them idle -> 75% busy.
	pct := hostCPUPercent(1000, 5000, 1050, 5200)
	if pct != 75 {
		t.Errorf("pct = %v, want 75", pct)
	}
}

func TestHostCPUPercentNoElapsedTime(t *testing.T) {
	if pct := hostCPUPercent(1000, 5000, 1000, 5000); pct != 0 {
		t.Errorf("pct = %v, want 0", pct)
	}
}

func TestPidCPUPercentDelta(t *testing.T) {
	// 100 ticks (USER_HZ) delta over 2 seconds of one process's utime+stime
	// = 1 full CPU-second/sec = 50%.
	pct := pidCPUPercent(1000, 1100, 2*time.Second)
	if pct != 50 {
		t.Errorf("pct = %v, want 50", pct)
	}
}

func TestPidCPUPercentGoneOrRolledBack(t *testing.T) {
	if pct := pidCPUPercent(1000, 500, time.Second); pct != 0 {
		t.Errorf("pct = %v, want 0 for a ticks value that went backwards", pct)
	}
}

// fakeReader is an in-memory reader for exercising Sampler.Sample's
// delta/mutex logic without any real /proc.
type fakeReader struct {
	idle, total uint64
	mem         [2]uint64 // used, total
	disk        [2]uint64
	load        float64
	up          uint64
	pidTicks    map[int]uint64
	pidRSS      map[int]uint64
	gonePids    map[int]bool
}

func (f *fakeReader) hostCPU() (uint64, uint64, error) { return f.idle, f.total, nil }
func (f *fakeReader) memInfo() (uint64, uint64, error) { return f.mem[0], f.mem[1], nil }
func (f *fakeReader) diskUsage(string) (uint64, uint64, error) {
	return f.disk[0], f.disk[1], nil
}
func (f *fakeReader) load1() (float64, error) { return f.load, nil }
func (f *fakeReader) uptime() (uint64, error) { return f.up, nil }
func (f *fakeReader) pidCPU(pid int) (uint64, bool, error) {
	if f.gonePids[pid] {
		return 0, false, nil
	}
	return f.pidTicks[pid], true, nil
}
func (f *fakeReader) pidMem(pid int) (uint64, error) {
	if f.gonePids[pid] {
		return 0, nil
	}
	return f.pidRSS[pid], nil
}

func TestSamplerFirstSampleHasZeroCPU(t *testing.T) {
	fr := &fakeReader{idle: 1000, total: 5000, pidTicks: map[int]uint64{42: 100}, pidRSS: map[int]uint64{42: 1024}}
	s := &Sampler{DataDir: "/data", reader: fr}

	snap, err := s.Sample([]MachinePID{{ID: "m1", PID: 42}})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if snap.Host.CPUPct != 0 {
		t.Errorf("first sample host CPUPct = %v, want 0", snap.Host.CPUPct)
	}
	if snap.Machines["m1"].CPUPct != 0 {
		t.Errorf("first sample machine CPUPct = %v, want 0", snap.Machines["m1"].CPUPct)
	}
	if snap.Machines["m1"].MemBytes != 1024 {
		t.Errorf("MemBytes = %d, want 1024", snap.Machines["m1"].MemBytes)
	}
	if snap.Host.VMCount != 1 {
		t.Errorf("VMCount = %d, want 1", snap.Host.VMCount)
	}
}

func TestSamplerSecondSampleComputesDelta(t *testing.T) {
	fr := &fakeReader{idle: 1000, total: 5000, pidTicks: map[int]uint64{42: 100}, pidRSS: map[int]uint64{42: 1024}}
	s := &Sampler{DataDir: "/data", reader: fr}
	if _, err := s.Sample([]MachinePID{{ID: "m1", PID: 42}}); err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	// force a known elapsed time rather than depending on real wall clock
	s.prev.at = time.Now().Add(-2 * time.Second)

	fr.idle, fr.total = 1050, 5200 // matches TestHostCPUPercentDelta: 75% busy
	fr.pidTicks[42] = 300          // +200 ticks over 2s = 100%

	snap, err := s.Sample([]MachinePID{{ID: "m1", PID: 42}})
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if snap.Host.CPUPct != 75 {
		t.Errorf("host CPUPct = %v, want 75", snap.Host.CPUPct)
	}
	if pct := snap.Machines["m1"].CPUPct; pct < 99.9 || pct > 100.1 {
		t.Errorf("machine CPUPct = %v, want ~100", pct)
	}
}

func TestSamplerGonePidReportsZeroNotError(t *testing.T) {
	fr := &fakeReader{gonePids: map[int]bool{42: true}}
	s := &Sampler{DataDir: "/data", reader: fr}

	snap, err := s.Sample([]MachinePID{{ID: "m1", PID: 42}})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	mm, ok := snap.Machines["m1"]
	if !ok {
		t.Fatal("expected an entry for m1 even though its pid is gone")
	}
	if mm.CPUPct != 0 || mm.MemBytes != 0 {
		t.Errorf("gone pid metrics = %+v, want zero value", mm)
	}
}
