//go:build !linux

package daemon

import "errors"

var errBridgeLinuxOnly = errors.New("daemon: bridge management is linux only")

// ensureBridgeLink is only implemented on Linux, where netlink bridges
// actually exist. It still exists on other platforms so the daemon package
// builds everywhere and its tests (against fakes -- see testDeployer's
// EnsureBridge -- never this function) run on darwin.
func ensureBridgeLink(idx int) (name, gateway string, err error) {
	return "", "", errBridgeLinuxOnly
}
