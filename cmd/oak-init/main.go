//go:build linux

// Command oak-init is PID 1 inside an oakberryio guest microVM. It brings
// up networking from Firecracker's MMDS, mounts volumes, execs the image's
// entrypoint/cmd as a child, reaps zombies, forwards termination signals,
// and powers the VM off when the child exits.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
)

// mmdsAddr is Firecracker's fixed MMDS v1 address, reachable once eth0 is up
// and a route to it exists.
const mmdsAddr = "169.254.169.254"

func main() {
	if err := run(); err != nil {
		fatal(err)
	}
	// run() only returns nil after triggering a reboot; if we get here the
	// reboot syscall itself didn't take effect (should not happen on a real
	// kernel), so make sure we don't fall through into whatever comes after
	// main in a statically linked binary.
	select {}
}

// fatal prints the error to the serial console, gives the log a moment to
// flush, then powers the VM off. There is nothing else useful oak-init can
// do once boot fails: there is no shell to drop into.
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "oak-init: fatal: %v\n", err)
	time.Sleep(2 * time.Second)
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	os.Exit(1)
}

func run() error {
	if err := mountAll(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	if err := linkUp("lo"); err != nil {
		return fmt.Errorf("lo up: %w", err)
	}
	if err := linkUp("eth0"); err != nil {
		return fmt.Errorf("eth0 up: %w", err)
	}
	if err := routeToMMDS(); err != nil {
		return fmt.Errorf("route to mmds: %w", err)
	}

	guest, err := fetchGuest()
	if err != nil {
		return fmt.Errorf("fetch mmds guest config: %w", err)
	}

	if err := configureAddr(guest); err != nil {
		return fmt.Errorf("configure eth0: %w", err)
	}
	if err := writeNetworkFiles(guest); err != nil {
		return fmt.Errorf("write network files: %w", err)
	}
	if err := mountVolumes(guest); err != nil {
		return fmt.Errorf("mount volumes: %w", err)
	}

	argv, err := guest.Argv()
	if err != nil {
		return fmt.Errorf("build argv: %w", err)
	}

	code, err := runChild(argv, guest)
	if err != nil {
		return fmt.Errorf("run child: %w", err)
	}

	fmt.Printf("oak-init: exit %d\n", code)
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

// mountAll sets up the pseudo-filesystems every guest needs before anything
// else can work: /proc for the child process, /sys and /dev for device
// access, and writable /tmp and /run.
func mountAll() error {
	mounts := []struct{ source, target, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"tmpfs", "/run", "tmpfs"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		if err := unix.Mount(m.source, m.target, m.fstype, 0, ""); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	return nil
}

func linkUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set %s up: %w", name, err)
	}
	return nil
}

// routeToMMDS adds a route to Firecracker's MMDS address over eth0. MMDS is
// only reachable once this exists; there is no DHCP or default route yet at
// this point in boot.
func routeToMMDS() error {
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("eth0: %w", err)
	}
	_, dst, err := net.ParseCIDR(mmdsAddr + "/32")
	if err != nil {
		return fmt.Errorf("parse mmds cidr: %w", err)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}); err != nil {
		return fmt.Errorf("add route: %w", err)
	}
	return nil
}

// fetchGuest fetches this machine's config from MMDS v1. A few retries
// absorb the small race between eth0 coming up and Firecracker's MMDS
// endpoint being ready to answer.
func fetchGuest() (mmds.Guest, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+mmdsAddr+"/", nil)
	if err != nil {
		return mmds.Guest{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		var guest mmds.Guest
		if err := json.NewDecoder(resp.Body).Decode(&guest); err != nil {
			return mmds.Guest{}, fmt.Errorf("decode mmds response: %w", err)
		}
		return guest, nil
	}
	return mmds.Guest{}, fmt.Errorf("mmds unreachable after retries: %w", lastErr)
}

// configureAddr applies the real IP and default route from the guest's MMDS
// payload, replacing the bootstrap route added by routeToMMDS.
func configureAddr(guest mmds.Guest) error {
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("eth0: %w", err)
	}

	addr, err := netlink.ParseAddr(guest.IP)
	if err != nil {
		return fmt.Errorf("parse guest ip %q: %w", guest.IP, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("add addr %s: %w", guest.IP, err)
	}

	gw := net.ParseIP(guest.Gateway)
	if gw == nil {
		return fmt.Errorf("invalid gateway %q", guest.Gateway)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
		return fmt.Errorf("add default route via %s: %w", guest.Gateway, err)
	}
	return nil
}

func writeNetworkFiles(guest mmds.Guest) error {
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf(guest.DNS)), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	if err := os.WriteFile("/etc/hosts", []byte(hostsFile(guest.MachineID)), 0644); err != nil {
		return fmt.Errorf("write hosts: %w", err)
	}
	return nil
}

// mountVolumes mounts every extra block device MMDS lists. The host formats
// volumes at create time, so oak-init only ever mounts, never formats.
func mountVolumes(guest mmds.Guest) error {
	for _, m := range guest.Mounts {
		if err := os.MkdirAll(m.Dest, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.Dest, err)
		}
		if err := unix.Mount(m.Device, m.Dest, "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.Device, m.Dest, err)
		}
	}
	return nil
}

// runChild execs argv as a child of PID 1 (not a replacement of it, since
// PID 1 must stay alive to reap zombies), forwards SIGTERM/SIGINT to it, and
// blocks until it exits. It reaps every reparented zombie along the way, as
// PID 1 must, but only reports the exit status of the direct child.
func runChild(argv []string, guest mmds.Guest) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = mmds.MergeEnv(nil, guest.Env)
	cmd.Dir = guest.WorkingDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start %v: %w", argv, err)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	forwarding := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigCh:
				_ = cmd.Process.Signal(sig)
			case <-forwarding:
				return
			}
		}
	}()

	exitCode, err := reap(cmd.Process.Pid)
	close(forwarding)
	return exitCode, err
}

// reap waits for children in a loop, as PID 1 must to avoid accumulating
// zombies from reparented orphans, and returns once childPID itself has
// exited.
func reap(childPID int) (int, error) {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, 0, nil)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return -1, fmt.Errorf("wait4: %w", err)
		}
		if pid != childPID {
			continue
		}
		if status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return status.ExitStatus(), nil
	}
}
