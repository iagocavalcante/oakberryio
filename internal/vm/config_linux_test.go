//go:build linux

package vm

import (
	"testing"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

func testSpec() Spec {
	return Spec{
		ID:        "m1",
		Kernel:    "/var/lib/oak/kernel/vmlinux",
		RootFS:    "/var/lib/oak/rootfs/hello/1.ext4",
		Volumes:   []string{"/var/lib/oak/volumes/hello/data.ext4"},
		Tap:       "tap-m1",
		MAC:       "06:00:0a:c8:00:05",
		MemoryMB:  256,
		CPUs:      1,
		LogPath:   "/var/log/oak/hello/m1.log",
		SocketDir: "/run/oak",
	}
}

func TestBuildConfigKernelAndSocket(t *testing.T) {
	spec := testSpec()
	cfg := BuildConfig(spec)

	if cfg.VMID != spec.ID {
		t.Errorf("VMID = %q, want %q", cfg.VMID, spec.ID)
	}
	if cfg.KernelImagePath != spec.Kernel {
		t.Errorf("KernelImagePath = %q, want %q", cfg.KernelImagePath, spec.Kernel)
	}
	want := "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/oak-init"
	if cfg.KernelArgs != want {
		t.Errorf("KernelArgs = %q, want %q", cfg.KernelArgs, want)
	}
	if cfg.SocketPath != "/run/oak/m1.sock" {
		t.Errorf("SocketPath = %q, want /run/oak/m1.sock", cfg.SocketPath)
	}
	if cfg.MmdsVersion != firecracker.MMDSv1 {
		t.Errorf("MmdsVersion = %q, want %q", cfg.MmdsVersion, firecracker.MMDSv1)
	}
}

func TestBuildConfigDrives(t *testing.T) {
	spec := testSpec()
	cfg := BuildConfig(spec)

	if len(cfg.Drives) != 2 {
		t.Fatalf("len(Drives) = %d, want 2", len(cfg.Drives))
	}

	// DrivesBuilder.Build() appends the root drive last.
	root := cfg.Drives[1]
	if root.IsRootDevice == nil || !*root.IsRootDevice {
		t.Error("root drive: IsRootDevice not true")
	}
	if root.PathOnHost == nil || *root.PathOnHost != spec.RootFS {
		t.Errorf("root drive path = %v, want %q", root.PathOnHost, spec.RootFS)
	}

	extra := cfg.Drives[0]
	if extra.IsRootDevice == nil || *extra.IsRootDevice {
		t.Error("extra drive: IsRootDevice should be false")
	}
	if extra.IsReadOnly == nil || *extra.IsReadOnly {
		t.Error("extra drive: IsReadOnly should be false")
	}
	if extra.PathOnHost == nil || *extra.PathOnHost != spec.Volumes[0] {
		t.Errorf("extra drive path = %v, want %q", extra.PathOnHost, spec.Volumes[0])
	}
}

func TestBuildConfigNetworkInterface(t *testing.T) {
	spec := testSpec()
	cfg := BuildConfig(spec)

	if len(cfg.NetworkInterfaces) != 1 {
		t.Fatalf("len(NetworkInterfaces) = %d, want 1", len(cfg.NetworkInterfaces))
	}
	iface := cfg.NetworkInterfaces[0]
	if !iface.AllowMMDS {
		t.Error("AllowMMDS = false, want true")
	}
	if iface.StaticConfiguration == nil {
		t.Fatal("StaticConfiguration is nil")
	}
	if iface.StaticConfiguration.MacAddress != spec.MAC {
		t.Errorf("MacAddress = %q, want %q", iface.StaticConfiguration.MacAddress, spec.MAC)
	}
	if iface.StaticConfiguration.HostDevName != spec.Tap {
		t.Errorf("HostDevName = %q, want %q", iface.StaticConfiguration.HostDevName, spec.Tap)
	}
}

func TestBuildConfigMachineCfg(t *testing.T) {
	spec := testSpec()
	cfg := BuildConfig(spec)

	if cfg.MachineCfg.VcpuCount == nil || *cfg.MachineCfg.VcpuCount != spec.CPUs {
		t.Errorf("VcpuCount = %v, want %d", cfg.MachineCfg.VcpuCount, spec.CPUs)
	}
	if cfg.MachineCfg.MemSizeMib == nil || *cfg.MachineCfg.MemSizeMib != spec.MemoryMB {
		t.Errorf("MemSizeMib = %v, want %d", cfg.MachineCfg.MemSizeMib, spec.MemoryMB)
	}
}
