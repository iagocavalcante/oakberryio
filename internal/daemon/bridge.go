package daemon

import (
	"fmt"
	"log"
	"net"

	"github.com/iagocavalcante/oakberryio/internal/dns"
)

// ensureBridge idempotently ensures tenant subnet idx's bridge exists (via
// the platform-specific ensureBridgeLink, see bridge_linux.go/
// bridge_other.go) and, the first time idx is seen, starts a DNS listener
// on its gateway. It's wired into Deployer.EnsureBridge by New and called
// directly by Run for the admin subnet (idx 0) and by reconcileOne/
// bootFromRelease/runReleaseCommand for whichever subnet an app's owner
// resolves to.
//
// A DNS listener per bridge is necessary, not just convenient: a tenant's
// nameserver is its own gateway (10.200.<idx>.1), because cross-subnet DNS
// to another bridge's gateway would be dropped by the cross-tenant
// nftables rule (see scripts/host-setup.sh) exactly like any other
// cross-tenant traffic. Every listener shares resolveDNS, which uses each
// query's source IP to scope its answer to the requester's own tenant.
func (d *Daemon) ensureBridge(idx int) (bridge, gateway string, err error) {
	bridge, gateway, err = ensureBridgeLink(idx)
	if err != nil {
		return "", "", fmt.Errorf("ensure bridge for tenant subnet %d: %w", idx, err)
	}

	d.bridgeMu.Lock()
	defer d.bridgeMu.Unlock()
	if d.dnsListening[idx] {
		return bridge, gateway, nil
	}

	addr := gateway + ":53"
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return "", "", fmt.Errorf("dns listen %s: %w", addr, err)
	}
	srv := &dns.Server{Resolve: d.resolveDNS}
	go func() {
		if err := srv.Serve(conn); err != nil {
			log.Printf("oakd: dns server on %s (tenant subnet %d): %v", addr, idx, err)
		}
	}()

	if d.dnsListening == nil {
		d.dnsListening = make(map[int]bool)
	}
	d.dnsListening[idx] = true
	return bridge, gateway, nil
}

// resolveDNS answers a .internal query for app from a client at srcIP,
// scoped to what that client is allowed to see (requesterCanSee). Wired
// into every bridge's dns.Server as the shared Resolve callback.
func (d *Daemon) resolveDNS(app string, srcIP net.IP) []net.IP {
	machines, err := d.Store.MachinesForApp(app)
	if err != nil {
		return nil
	}

	if !d.requesterCanSee(app, srcIP) {
		// Deliberately indistinguishable from "app doesn't exist": both
		// return no IPs, and dns.Server turns an empty result into NXDOMAIN
		// either way, so a tenant probing for another tenant's app name
		// can't tell it apart from a typo.
		return nil
	}

	var ips []net.IP
	for _, m := range machines {
		if m.State != "running" {
			continue
		}
		if ip := net.ParseIP(m.IP); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

// requesterCanSee reports whether a DNS query from srcIP may see app's
// machine IPs. srcIP's third octet is decoded as a tenant subnet index
// (10.200.<idx>.0/24) and mapped to that subnet's owner via the store; the
// requester can see app only if it shares that owner. A source outside
// every tenant subnet entirely (10.200.0.0/16) -- the host itself, since
// each bridge's listener is bound to that bridge's specific gateway IP and
// oak's own tooling isn't a tenant VM -- is treated as admin and can see
// every app, per docs/plans/2026-09-15-phase-d-network-isolation-design.md.
func (d *Daemon) requesterCanSee(app string, srcIP net.IP) bool {
	v4 := srcIP.To4()
	if v4 == nil || v4[0] != 10 || v4[1] != 200 {
		return true
	}

	owner, err := d.Store.OwnerForSubnet(int(v4[2]))
	if err != nil {
		return false
	}
	appOwner, err := d.Store.AppOwner(app)
	if err != nil {
		return false
	}
	return appOwner == owner
}
