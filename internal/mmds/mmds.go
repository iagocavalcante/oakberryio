// Package mmds is the JSON contract oakd writes into a Firecracker
// microVM's MMDS and oak-init reads back on boot to configure and launch
// the guest.
package mmds

import "errors"

// Guest is the full payload for one machine, served over MMDS at
// http://169.254.169.254/.
type Guest struct {
	MachineID  string            `json:"machine_id"`
	App        string            `json:"app"`
	IP         string            `json:"ip"`      // "10.200.0.5/16"
	Gateway    string            `json:"gateway"` // "10.200.0.1"
	DNS        string            `json:"dns"`     // "10.200.0.1"
	Env        map[string]string `json:"env"`
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`
	WorkingDir string            `json:"workdir"`
	Mounts     []Mount           `json:"mounts"`
}

// Mount is one extra block device to mount inside the guest.
type Mount struct {
	Device string `json:"device"`      // "/dev/vdb"
	Dest   string `json:"destination"` // "/data"
}

// Argv returns the command to run as PID 1's child: the image entrypoint
// followed by cmd, Docker-style. It errors if both are empty since there is
// then nothing to run.
func (g Guest) Argv() ([]string, error) {
	if len(g.Entrypoint) == 0 && len(g.Cmd) == 0 {
		return nil, errors.New("mmds: guest has no entrypoint or cmd to run")
	}
	argv := make([]string, 0, len(g.Entrypoint)+len(g.Cmd))
	argv = append(argv, g.Entrypoint...)
	argv = append(argv, g.Cmd...)
	return argv, nil
}
