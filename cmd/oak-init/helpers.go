package main

import "fmt"

// resolvConf builds the content of /etc/resolv.conf pointing at the guest's
// single nameserver (the host's DNS resolver, see internal/dns).
func resolvConf(dnsIP string) string {
	return fmt.Sprintf("nameserver %s\n", dnsIP)
}

// hostsFile builds the content of /etc/hosts: just enough for the guest's
// own hostname (its machine ID) to resolve locally.
func hostsFile(machineID string) string {
	return fmt.Sprintf("127.0.0.1\tlocalhost\n127.0.0.1\t%s\n", machineID)
}
