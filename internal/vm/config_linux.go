//go:build linux

package vm

import (
	"fmt"
	"net"
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
//
// When spec.IP is set, it fills in the interface's static IPConfiguration.
// The SDK's own setupKernelArgs (machine.go) then merges that into an "ip="
// kernel boot argument before the guest ever starts, so the kernel itself
// configures eth0 (address, gateway, nameserver) before any userspace,
// including oak-init, runs — closing the race where oak-init's own MMDS
// fetch would otherwise need eth0 to already have an address. oak-init's
// own netlink calls (routeToMMDS, configureAddr) become a defense-in-depth
// no-op in that case, tolerating "already exists".
func BuildConfig(spec Spec) (firecracker.Config, error) {
	drives := firecracker.NewDrivesBuilder(spec.RootFS)
	for _, volume := range spec.Volumes {
		drives = drives.AddDrive(volume, false)
	}

	staticConf := &firecracker.StaticNetworkConfiguration{
		MacAddress:  spec.MAC,
		HostDevName: spec.Tap,
	}
	if spec.IP != "" {
		ip, ipnet, err := net.ParseCIDR(spec.IP)
		if err != nil {
			return firecracker.Config{}, fmt.Errorf("vm: parse spec ip %q: %w", spec.IP, err)
		}
		var gw net.IP
		if spec.Gateway != "" {
			gw = net.ParseIP(spec.Gateway)
			if gw == nil {
				return firecracker.Config{}, fmt.Errorf("vm: invalid spec gateway %q", spec.Gateway)
			}
		}
		staticConf.IPConfiguration = &firecracker.IPConfiguration{
			IPAddr:      net.IPNet{IP: ip, Mask: ipnet.Mask},
			Gateway:     gw,
			Nameservers: []string{spec.Gateway},
		}
	}

	return firecracker.Config{
		VMID:            spec.ID,
		SocketPath:      filepath.Join(spec.SocketDir, spec.ID+".sock"),
		KernelImagePath: spec.Kernel,
		KernelArgs:      kernelArgs,
		Drives:          drives.Build(),
		NetworkInterfaces: firecracker.NetworkInterfaces{
			{
				StaticConfiguration: staticConf,
				AllowMMDS:           true,
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(spec.CPUs),
			MemSizeMib: firecracker.Int64(spec.MemoryMB),
		},
		MmdsVersion: firecracker.MMDSv1,
	}, nil
}
