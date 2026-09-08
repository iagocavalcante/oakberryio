// Command oak is oakberryio's CLI: it builds and pushes an app's image,
// deploys it, and talks to oakd's status/logs/secrets/volumes API. See
// docs/host.md and internal/daemon/api.go for the wire contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"text/tabwriter"
	"time"

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
  secrets set <app> KEY=VALUE...
  volumes create <app> <name> <size_gb>`)
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

	if err := runStreamed("docker", "build", "-t", image, "-f", cfg.Build.Dockerfile, "."); err != nil {
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

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow log output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: oak logs <app> [-f]")
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.Logs(context.Background(), fs.Arg(0), *follow, os.Stdout)
}

func runSecrets(args []string) error {
	if len(args) < 3 || args[0] != "set" {
		return fmt.Errorf("usage: oak secrets set <app> KEY=VALUE...")
	}
	app := args[1]
	kv, err := cli.ParseKV(args[2:])
	if err != nil {
		return err
	}

	client, err := cli.NewClient()
	if err != nil {
		return err
	}
	return client.SetSecrets(context.Background(), app, kv)
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
