//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
)

// ensureBridgeLink idempotently creates the Linux bridge for tenant subnet
// idx (oak<idx>) and assigns it that subnet's gateway address
// (10.200.<idx>.1/24), bringing the link up. Safe to call repeatedly and
// concurrently for the same idx -- Daemon.ensureBridge serializes callers,
// but netlink.LinkAdd/AddrAdd are themselves tolerant of "already exists"
// too, matching vm.createTap's style.
//
// Tolerates oak0 already existing with the host-setup.sh systemd-networkd
// address (10.200.0.1/16, not /24, see that script's comments): a gateway
// IP already present under any prefix length is left alone rather than
// also adding it as /24, which would risk two overlapping addresses on the
// same interface.
func ensureBridgeLink(idx int) (name, gateway string, err error) {
	name = bridgeName(idx)
	gateway = gatewayIP(idx)
	gatewayAddr := net.ParseIP(gateway)

	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil && !errors.Is(err, syscall.EEXIST) {
		return "", "", fmt.Errorf("link add %s: %w", name, err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", "", fmt.Errorf("lookup %s: %w", name, err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return "", "", fmt.Errorf("list addrs on %s: %w", name, err)
	}
	hasGateway := false
	for _, a := range addrs {
		if a.IP.Equal(gatewayAddr) {
			hasGateway = true
			break
		}
	}
	if !hasGateway {
		addr, err := netlink.ParseAddr(gateway + "/24")
		if err != nil {
			return "", "", fmt.Errorf("parse addr %s/24: %w", gateway, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
			return "", "", fmt.Errorf("addr add %s to %s: %w", gateway, name, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return "", "", fmt.Errorf("set up %s: %w", name, err)
	}

	return name, gateway, nil
}
