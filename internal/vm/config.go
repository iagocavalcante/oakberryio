// Package vm drives Firecracker microVMs: turning a Spec into a
// firecracker.Config (config_linux.go) and actually booting one via tap
// devices and the Firecracker API (vm_linux.go).
//
// Both of those live behind //go:build linux: github.com/firecracker-microvm/
// firecracker-go-sdk transitively imports
// github.com/containernetworking/plugins/pkg/ns, which ships only a
// ns_linux.go with no non-Linux stub anywhere in that dependency. That makes
// the SDK package itself fail to compile on darwin, not just call into
// Linux-only syscalls — so, unlike internal/rootfs's mkfs split, BuildConfig
// cannot be a "pure, cross-platform" function that merely avoids syscalls;
// the mere import forces the whole file behind the build tag. This file
// holds only the parts that don't need the SDK, so they stay testable on
// the Mac.
package vm

import (
	"fmt"
	"net"
)

// Spec is everything needed to boot one microVM. It carries no host
// resources itself (no open files, no live tap) so it can be built and
// tested without touching the machine.
type Spec struct {
	ID        string
	Kernel    string   // path to the shared vmlinux
	RootFS    string   // path to this machine's rootfs ext4 image
	Volumes   []string // extra drive images, become /dev/vdb, /dev/vdc, ...
	Tap       string   // host tap device name
	Bridge    string   // host bridge tap attaches to, e.g. "oak3"; empty falls back to "oak0" (see createTap)
	MAC       string   // guest NIC MAC, see MACFromIP
	IP        string   // guest static IP incl. netmask, e.g. "10.200.3.5/24"; empty means no static IP
	Gateway   string   // guest default gateway, e.g. "10.200.3.1"; empty means no static IP
	MemoryMB  int64
	CPUs      int64
	LogPath   string // guest serial console (ttyS0) output file
	SocketDir string // directory for the Firecracker API socket

	// VsockUDS is the host-side Unix-domain socket path Firecracker exposes
	// for this machine's vsock device (host<->guest bridge for `oak ssh`,
	// see internal/daemon/api.go's handleSSH). Empty means no vsock device.
	VsockUDS string
	// GuestCID is the vsock device's 32-bit guest Context Identifier; oakd
	// always sets this to 3 (the lowest CID not reserved for the hypervisor
	// or host, see vsock(7)) alongside VsockUDS.
	GuestCID uint32
}

// MACFromIP deterministically derives a locally-administered MAC address
// from a machine's IPv4 address, so the guest NIC's MAC is always
// reproducible from its IP: 06:00:<4 IP octets in hex>.
func MACFromIP(ip string) (string, error) {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return "", fmt.Errorf("vm: %q is not an IPv4 address", ip)
	}
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3]), nil
}
