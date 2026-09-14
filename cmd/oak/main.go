// Command oak is oakberryio's CLI: it builds and pushes an app's image,
// deploys it, and talks to oakd's status/logs/restart/scale/destroy/secrets/
// volumes API. See docs/host.md and internal/daemon/api.go for the wire
// contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/iagocavalcante/oakberryio/internal/appconfig"
	"github.com/iagocavalcante/oakberryio/internal/cli"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "apps":
		err = runApps(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "logs":
		err = runLogs(os.Args[2:])
	case "secrets":
		err = runSecrets(os.Args[2:])
	case "volumes":
		err = runVolumes(os.Args[2:])
	case "restart":
		err = runRestart(os.Args[2:])
	case "scale":
		err = runScale(os.Args[2:])
	case "destroy", "rm":
		err = runDestroy(os.Args[2:])
	case "ssh":
		err = runSSH(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "oak:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: oak <command> [args]

commands:
  deploy [-c oak.toml]
  apps
  status <app>
  logs <app> [-f]
  restart <app>
  scale <app> [--memory 1gb] [--cpus 2]
  destroy <app> -y   (alias: rm)
  secrets set <app> KEY=VALUE...
  secrets list <app>
  secrets unset <app> KEY...
  volumes create <app> <name> <size_gb>
  ssh <app> [-- cmd args...]`)
}

// runDeploy builds the app's image with docker, pushes it to OAK_REGISTRY
// (default localhost:5000), then POSTs it to oakd along with the oak.toml
// text -- always, not just on first deploy, per handleDeploy's contract
// (internal/daemon/api.go).
func runDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	configPath := fs.String("c", "oak.toml", "path to oak.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", *configPath, err)
	}
	cfg, err := appconfig.Parse(raw)
	if err != nil {
		return err
	}

	registry := os.Getenv("OAK_REGISTRY")
	if registry == "" {
		registry = "localhost:5000"
	}
	image := fmt.Sprintf("%s/%s:%d", registry, cfg.App, time.Now().Unix())

	buildArgs := make([]string, 0, len(cfg.Build.Args))
	for k := range cfg.Build.Args {
		buildArgs = append(buildArgs, k)
	}
	sort.Strings(buildArgs)

	// Always build for linux/amd64: oakd, oak-init and the Firecracker guest
	// kernel are all amd64, so an image built for the host's native arch (e.g.
	// arm64 on an Apple Silicon Mac) boots to "exec format error" inside the
	// guest. Pinning the platform makes a deploy from any dev machine produce
	// a runnable image (via the local Docker's emulation when needed).
	dockerArgs := []string{"build", "--platform", "linux/amd64", "-t", image, "-f", cfg.Build.Dockerfile}
	for _, k := range buildArgs {
		dockerArgs = append(dockerArgs, "--build-arg", fmt.Sprintf("%s=%s", k, cfg.Build.Args[k]))
	}
	dockerArgs = append(dockerArgs, ".")

	if err := runStreamed("docker", dockerArgs...); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	if err := runStreamed("docker", "push", image); err != nil {
		return fmt.Errorf("docker push: %w", err)
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Deploy(context.Background(), cfg.App, image, string(raw), os.Stdout)
}

// runStreamed runs name(args...), streaming its stdout/stderr straight to
// ours.
func runStreamed(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runApps(args []string) error {
	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	apps, err := client.Apps(context.Background())
	if err != nil {
		return err
	}
	for _, a := range apps {
		fmt.Println(a)
	}
	return nil
}

func runStatus(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: oak status <app>")
	}
	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	machines, err := client.Machines(context.Background(), args[0])
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tIP\tPID")
	for _, m := range machines {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", m.ID, m.State, m.IP, m.PID)
	}
	return w.Flush()
}

// parseFlagsAnywhere parses fs but, unlike a bare fs.Parse, allows flags to
// appear before or after positional arguments (Go's flag package otherwise
// stops parsing at the first non-flag token, so `oak destroy app -y` would
// silently drop the -y). It returns the positional arguments in order.
func parseFlagsAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	pos, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: oak logs <app> [-f]")
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Logs(context.Background(), pos[0], *follow, os.Stdout)
}

func runSecrets(args []string) error {
	usage := "usage: oak secrets set <app> KEY=VALUE... | oak secrets list <app> | oak secrets unset <app> KEY..."
	if len(args) < 2 {
		return errors.New(usage)
	}
	sub, app := args[0], args[1]

	client, err := cli.NewClient()
	if err != nil {
		return err
	}

	switch sub {
	case "set":
		if len(args) < 3 {
			return errors.New(usage)
		}
		kv, err := cli.ParseKV(args[2:])
		if err != nil {
			return err
		}
		if err := client.SetSecrets(context.Background(), app, kv); err != nil {
			return err
		}
		fmt.Printf("run: oak restart %s to apply\n", app)
		return nil
	case "list":
		if len(args) != 2 {
			return errors.New(usage)
		}
		keys, err := client.SecretKeys(context.Background(), app)
		if err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		return nil
	case "unset":
		if len(args) < 3 {
			return errors.New(usage)
		}
		if err := client.UnsetSecrets(context.Background(), app, args[2:]); err != nil {
			return err
		}
		fmt.Printf("run: oak restart %s to apply\n", app)
		return nil
	default:
		return errors.New(usage)
	}
}

// runRestart reboots app from its latest release, with no rebuild.
func runRestart(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: oak restart <app>")
	}
	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Restart(context.Background(), args[0], os.Stdout)
}

// runScale resizes an app's VM and reboots it from its latest release.
func runScale(args []string) error {
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	memory := fs.String("memory", "", "memory size, e.g. 1gb, 512mb, or a bare integer for MB")
	cpus := fs.Int("cpus", 0, "number of vCPUs")
	pos, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: oak scale <app> [--memory 1gb] [--cpus 2]")
	}
	if *memory == "" && *cpus == 0 {
		return fmt.Errorf("scale requires at least one of --memory or --cpus")
	}

	memoryMB := 0
	if *memory != "" {
		mb, err := cli.ParseMemoryMB(*memory)
		if err != nil {
			return err
		}
		memoryMB = mb
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Scale(context.Background(), pos[0], memoryMB, *cpus, os.Stdout)
}

// runDestroy tears down an app entirely. Refuses to run without -y.
func runDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	yes := fs.Bool("y", false, "confirm destruction")
	pos, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: oak destroy <app> -y")
	}
	if !*yes {
		return fmt.Errorf("destroy requires -y to confirm")
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Destroy(context.Background(), pos[0])
}

// sshHeader is the single JSON line sent right after the HTTP handshake;
// must match cmd/oak-init/agent.go's sshHeader struct on the guest side.
type sshHeader struct {
	Cmd  []string `json:"cmd"`
	Rows uint16   `json:"rows"`
	Cols uint16   `json:"cols"`
}

// runSSH opens an interactive shell (or runs a one-off command) inside
// app's running machine, over oakd's vsock bridge. Everything after "--"
// becomes the command to run in the guest; with no "--" it defaults to an
// interactive shell. See docs/plans/2026-09-14-ssh-and-stateful-design.md.
func runSSH(args []string) error {
	appArgs := args
	var cmdArgs []string
	for i, a := range args {
		if a == "--" {
			appArgs, cmdArgs = args[:i], args[i+1:]
			break
		}
	}
	if len(appArgs) != 1 {
		return fmt.Errorf("usage: oak ssh <app> [-- cmd args...]")
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	conn, err := client.SSH(context.Background(), appArgs[0])
	if err != nil {
		return err
	}
	defer conn.Close()

	// Only put the terminal in raw mode (and only report a real size) when
	// stdin actually is one -- oak ssh app -- some-command may be piped.
	rows, cols := 24, 80
	stdinFD := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFD) {
		if w, h, err := term.GetSize(stdinFD); err == nil {
			cols, rows = w, h
		}
		oldState, err := term.MakeRaw(stdinFD)
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}
		defer term.Restore(stdinFD, oldState)
	}

	header, err := json.Marshal(sshHeader{Cmd: cmdArgs, Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return fmt.Errorf("marshal ssh header: %w", err)
	}
	if _, err := conn.Write(append(header, '\n')); err != nil {
		return fmt.Errorf("send ssh header: %w", err)
	}

	// The stdin->conn copy runs in its own goroutine since it blocks on
	// terminal input; once the session ends (conn->stdout below returns,
	// e.g. the remote command exited) the process exits and takes that
	// goroutine with it -- live resize (SIGWINCH) and a clean join here are
	// both out of scope for v1, see the design doc.
	go func() { _, _ = io.Copy(conn, os.Stdin) }()
	_, _ = io.Copy(os.Stdout, conn)
	return nil
}

func runVolumes(args []string) error {
	if len(args) != 4 || args[0] != "create" {
		return fmt.Errorf("usage: oak volumes create <app> <name> <size_gb>")
	}
	app, name := args[1], args[2]
	sizeGB, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("size_gb must be an integer: %w", err)
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.CreateVolume(context.Background(), app, name, sizeGB)
}
