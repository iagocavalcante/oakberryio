//go:build linux

package vm

import (
	"path/filepath"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// kernelArgs are fixed for every oak-managed VM: serial console on ttyS0,
// reboot via keyboard controller (oak-init calls unix.Reboot, which needs
// reboot=k to actually power the VM off instead of hanging), panic=1 so a
// kernel panic reboots rather than hangs, pci=off since nothing needs PCI,
// and oak-init as PID 1.
const kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/oak-init"

// BuildConfig turns a Spec into the firecracker.Config the SDK needs to
// start a Machine. It touches no host state: no files are opened, no tap is
// created (that's vm_linux.go's Start).
func BuildConfig(spec Spec) firecracker.Config {
	drives := firecracker.NewDrivesBuilder(spec.RootFS)
	for _, volume := range spec.Volumes {
		drives = drives.AddDrive(volume, false)
	}

	return firecracker.Config{
		VMID:            spec.ID,
		SocketPath:      filepath.Join(spec.SocketDir, spec.ID+".sock"),
		KernelImagePath: spec.Kernel,
		KernelArgs:      kernelArgs,
		Drives:          drives.Build(),
		NetworkInterfaces: firecracker.NetworkInterfaces{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  spec.MAC,
					HostDevName: spec.Tap,
				},
				AllowMMDS: true,
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(spec.CPUs),
			MemSizeMib: firecracker.Int64(spec.MemoryMB),
		},
		MmdsVersion: firecracker.MMDSv1,
	}
}
