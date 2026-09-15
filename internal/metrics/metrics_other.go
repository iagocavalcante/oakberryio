//go:build !linux

package metrics

// otherReader stubs metrics collection on non-Linux hosts (macOS dev
// machines) so this package -- and its pure-parsing tests -- builds and
// runs without /proc, which doesn't exist there. It always reports zero
// values and never errors: a darwin build should never fail because of this
// package, only report nothing, mirroring internal/vm's vm_other.go split.
type otherReader struct{}

func newReader() reader { return otherReader{} }

func (otherReader) hostCPU() (idle, total uint64, err error)                       { return 0, 0, nil }
func (otherReader) memInfo() (usedBytes, totalBytes uint64, err error)             { return 0, 0, nil }
func (otherReader) diskUsage(dir string) (usedBytes, totalBytes uint64, err error) { return 0, 0, nil }
func (otherReader) load1() (float64, error)                                        { return 0, nil }
func (otherReader) uptime() (uint64, error)                                        { return 0, nil }
func (otherReader) pidCPU(pid int) (ticks uint64, ok bool, err error)              { return 0, false, nil }
func (otherReader) pidMem(pid int) (uint64, error)                                 { return 0, nil }
