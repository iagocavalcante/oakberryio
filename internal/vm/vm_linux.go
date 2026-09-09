//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/vishvananda/netlink"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
)

// Machine is a running Firecracker microVM plus the host-side resources
// (tap device, serial console log file) that must be torn down with it.
type Machine struct {
	fc      *firecracker.Machine
	tap     netlink.Link
	logFile *os.File
}

// Start creates spec.Tap on bridge oak0, boots a Firecracker microVM per
// spec, and pushes meta into its MMDS before the guest starts running. On
// any error after the tap is created, the tap is deleted.
func Start(ctx context.Context, spec Spec, meta mmds.Guest) (*Machine, error) {
	tap, err := createTap(spec.Tap)
	if err != nil {
		return nil, fmt.Errorf("vm: create tap %s: %w", spec.Tap, err)
	}

	m, err := startMachine(ctx, spec, meta, tap)
	if err != nil {
		_ = netlink.LinkDel(tap)
		return nil, err
	}
	return m, nil
}

func createTap(name string) (netlink.Link, error) {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = name
	tap := &netlink.Tuntap{LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TAP}
	if err := netlink.LinkAdd(tap); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			return nil, fmt.Errorf("link add: %w", err)
		}
		// A tap with this name was left behind, most likely by an unclean
		// daemon restart before the previous Machine's Stop/release ran.
		// Delete it and retry once rather than failing the whole deploy.
		if existing, lookupErr := netlink.LinkByName(name); lookupErr == nil {
			_ = netlink.LinkDel(existing)
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return nil, fmt.Errorf("link add (retry after delete): %w", err)
		}
	}

	bridge, err := netlink.LinkByName("oak0")
	if err != nil {
		_ = netlink.LinkDel(tap)
		return nil, fmt.Errorf("bridge oak0: %w", err)
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		_ = netlink.LinkDel(tap)
		return nil, fmt.Errorf("set master oak0: %w", err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		_ = netlink.LinkDel(tap)
		return nil, fmt.Errorf("set up: %w", err)
	}
	return tap, nil
}

func startMachine(ctx context.Context, spec Spec, meta mmds.Guest, tap netlink.Link) (*Machine, error) {
	cfg, err := BuildConfig(spec)
	if err != nil {
		return nil, fmt.Errorf("vm: build config: %w", err)
	}

	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open serial console log %s: %w", spec.LogPath, err)
	}

	// Firecracker writes the guest's serial console (ttyS0, per the
	// "console=ttyS0" kernel arg in BuildConfig) to the VMM process's own
	// stdout/stderr. That's a different thing from firecracker.Config.
	// LogPath/LogLevel, which configures Firecracker's own operational
	// logging and isn't used here. So capturing what the guest prints means
	// giving the VMM process itself a redirected stdout/stderr, via a
	// custom command in place of the SDK's default (which points at our own
	// os.Stdout/os.Stderr).
	cmd := firecracker.VMCommandBuilder{}.
		WithBin("firecracker").
		WithSocketPath(cfg.SocketPath).
		WithStdout(logFile).
		WithStderr(logFile).
		Build(ctx)

	fcMachine, err := firecracker.NewMachine(ctx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("new machine: %w", err)
	}

	// Machine.SetMetadata (PutMmds) talks to the Firecracker API socket,
	// which only exists once the VMM process is running. Machine.Start
	// first runs the FcInit handler chain (which starts the VMM, then wires
	// up drives/network/MMDS config) and only afterwards calls
	// startInstance, the action that actually boots the guest's vCPUs. The
	// SDK's default FcInit chain (v1.0.0) does not include a metadata
	// handler, so we append one: it runs at the end of that chain, meaning
	// after the socket exists but still strictly before the guest ever
	// executes, guaranteeing oak-init's first MMDS request already sees it.
	fcMachine.Handlers.FcInit = fcMachine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(meta))

	if err := fcMachine.Start(ctx); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start machine: %w", err)
	}

	return &Machine{fc: fcMachine, tap: tap, logFile: logFile}, nil
}

// PID returns the Firecracker VMM process's host PID, or 0 if unavailable.
func (m *Machine) PID() int {
	pid, err := m.fc.PID()
	if err != nil {
		return 0
	}
	return pid
}

// Stop asks the guest to shut down cleanly (Ctrl-Alt-Del, which the kernel
// arg reboot=k turns into a reboot that oak-init intercepts as a poweroff),
// then force-kills the VMM if it hasn't exited within 10s. The tap device
// and log file are always released, however the VM stopped.
func (m *Machine) Stop(ctx context.Context) error {
	defer m.release()

	_ = m.fc.Shutdown(ctx)

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.fc.Wait(waitCtx); err != nil {
		if stopErr := m.fc.StopVMM(); stopErr != nil {
			return fmt.Errorf("force stop: %w", stopErr)
		}
		return m.fc.Wait(ctx)
	}
	return nil
}

// Wait blocks until the VMM process exits.
func (m *Machine) Wait(ctx context.Context) error {
	return m.fc.Wait(ctx)
}

func (m *Machine) release() {
	if m.tap != nil {
		_ = netlink.LinkDel(m.tap)
	}
	if m.logFile != nil {
		_ = m.logFile.Close()
	}
}
