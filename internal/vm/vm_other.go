//go:build !linux

package vm

import (
	"context"
	"errors"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
)

var errLinuxOnly = errors.New("vm: linux only")

// Machine is only implemented on Linux, where Firecracker, tap devices and
// /dev/kvm actually exist.
type Machine struct{}

// Start always fails on non-Linux hosts.
func Start(ctx context.Context, spec Spec, meta mmds.Guest) (*Machine, error) {
	return nil, errLinuxOnly
}

// PID always returns 0 on non-Linux hosts.
func (m *Machine) PID() int { return 0 }

// Stop always fails on non-Linux hosts.
func (m *Machine) Stop(ctx context.Context) error { return errLinuxOnly }

// Wait always fails on non-Linux hosts.
func (m *Machine) Wait(ctx context.Context) error { return errLinuxOnly }
