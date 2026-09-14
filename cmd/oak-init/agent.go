//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/iagocavalcante/oakberryio/internal/mmds"
)

// sshAgentPort is the AF_VSOCK port this agent listens on for `oak ssh`.
// Must match internal/daemon/ssh.go's sshAgentPort constant on the host
// side -- there is no negotiation, only both sides being built from the
// same source tree.
const sshAgentPort = 10000

// sshHeader is the single JSON line a client sends right after connecting:
// what to run, and its terminal's initial size.
type sshHeader struct {
	Cmd  []string `json:"cmd"`
	Rows uint16   `json:"rows"`
	Cols uint16   `json:"cols"`
}

// runSSHAgent listens for host-initiated vsock connections and services
// each with an interactive PTY session, for as long as the machine lives
// (see run()'s call site: it starts this as a goroutine alongside the app's
// own child, not after it). Every failure here -- opening the vsock
// listener, or servicing one connection -- is logged and never fatal to
// boot or to the app: `oak ssh` not working is not a reason to fail the
// app's own boot. See docs/plans/2026-09-14-ssh-and-stateful-design.md for
// the wire protocol.
func runSSHAgent(guest mmds.Guest) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Printf("oak-init: ssh agent: open vsock socket: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: sshAgentPort}); err != nil {
		log.Printf("oak-init: ssh agent: bind vsock port %d: %v", sshAgentPort, err)
		_ = unix.Close(fd)
		return
	}
	if err := unix.Listen(fd, 4); err != nil {
		log.Printf("oak-init: ssh agent: listen: %v", err)
		_ = unix.Close(fd)
		return
	}
	defer unix.Close(fd)

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			log.Printf("oak-init: ssh agent: accept: %v", err)
			return
		}
		conn, err := vsockConn(nfd)
		if err != nil {
			log.Printf("oak-init: ssh agent: wrap accepted conn: %v", err)
			continue
		}
		go handleSSHConn(conn, guest)
	}
}

// vsockConn wraps a raw accepted vsock file descriptor as a net.Conn.
// net.FileConn dups fd internally, so the *os.File used to build it is
// closed right away without affecting the returned conn.
func vsockConn(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "oak-ssh-conn")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("file conn: %w", err)
	}
	return conn, nil
}

// handleSSHConn services one accepted `oak ssh` connection: it reads the
// JSON header, allocates a PTY, runs the requested command (default a
// shell) with the app's own environment, and copies bytes between conn and
// the PTY until the command exits or the client disconnects.
func handleSSHConn(conn net.Conn, guest mmds.Guest) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("oak-init: ssh agent: read header: %v", err)
		return
	}

	var hdr sshHeader
	if err := json.Unmarshal([]byte(line), &hdr); err != nil {
		fmt.Fprintf(conn, "error: invalid request: %v\n", err)
		return
	}
	argv := hdr.Cmd
	if len(argv) == 0 {
		argv = []string{"/bin/sh"}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = childEnv(guest)
	cmd.Dir = guest.WorkingDir

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: hdr.Rows, Cols: hdr.Cols})
	if err != nil {
		// e.g. no shell at all in a scratch image -- report it on the
		// stream rather than just closing, so the CLI can show why.
		fmt.Fprintf(conn, "error: %v\n", err)
		return
	}

	go func() {
		_, _ = io.Copy(ptmx, reader)
	}()
	_, _ = io.Copy(conn, ptmx)

	// io.Copy above returns once the command exits (reading the PTY master
	// errors once nothing holds its slave open anymore) or the client
	// disconnects. Closing both here unblocks the other direction's
	// goroutine if it's still blocked reading client input. The exited
	// process itself is reaped by PID 1's own reap loop (see runChild/reap
	// in main.go), which wait4(-1)s every child for as long as the app is
	// running; calling cmd.Wait() here too would race that loop to reap the
	// same pid.
	_ = ptmx.Close()
	_ = conn.Close()
}
