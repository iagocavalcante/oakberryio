package daemon

import (
	"fmt"
	"io"
	"net"
	"strings"
)

// sshAgentPort is the AF_VSOCK port oak-init's guest shell agent listens on
// inside every machine (see cmd/oak-init/agent.go). Fixed and shared only by
// both sides being built from the same source tree -- there is no
// negotiation, so this value must stay in sync with the guest agent's own
// constant.
const sshAgentPort = 10000

// dialVsock connects to a guest's vsock listener on port by dialing
// Firecracker's host-side Unix socket for the machine's vsock device and
// performing Firecracker's host-initiated vsock handshake: write
// "CONNECT <port>\n", then read back a "OK <port>\n" line before the
// connection is a raw byte stream to whatever is listening on that vsock
// port inside the guest. See
// https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md.
func dialVsock(udsPath string, port int) (net.Conn, error) {
	conn, err := net.Dial("unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", udsPath, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send connect: %w", err)
	}

	// Read the reply one byte at a time rather than through a buffered
	// reader: a buffered reader could read ahead past the "OK ...\n" line
	// into bytes the guest's agent sends once the vsock stream is live, and
	// those bytes would be silently lost since callers get back the raw
	// conn, not our reader.
	reply, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read connect reply: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock connect: unexpected reply %q", strings.TrimSpace(reply))
	}
	return conn, nil
}

// readLine reads a single '\n'-terminated line from conn, one byte at a
// time.
func readLine(conn net.Conn) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := conn.Read(b)
		if n > 0 {
			line = append(line, b[0])
			if b[0] == '\n' {
				return string(line), nil
			}
		}
		if err != nil {
			return string(line), err
		}
	}
}

// bridge copies bytes between a and b in both directions until either side
// closes or errors, then closes both. Past the vsock CONNECT handshake,
// oakd is a dumb pipe between the CLI's connection and the guest agent; see
// handleSSH.
func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
}
