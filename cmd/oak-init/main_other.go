//go:build !linux

package main

import (
	"fmt"
	"os"
)

// oak-init only ever runs as PID 1 inside a Linux guest kernel. This stub
// exists so `go build ./...` succeeds on the Mac too, matching
// internal/rootfs's mkfs_linux.go/mkfs_other.go split.
func main() {
	fmt.Fprintln(os.Stderr, "oak-init: linux only")
	os.Exit(1)
}
