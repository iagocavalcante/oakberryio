// Command oakd is the oakberryio host daemon: it reconciles machine state on
// start, then serves the deploy/status/logs/secrets/volumes API until
// signalled to stop.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/iagocavalcante/oakberryio/internal/daemon"
)

func main() {
	configPath := flag.String("config", "/etc/oak/oakd.toml", "path to oakd.toml")
	flag.Parse()

	var cfg daemon.Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("oakd: load config %s: %v", *configPath, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// ctx becomes the Deployer's BaseCtx, so every microVM Start boots under
	// this process-lifetime context (via context.WithoutCancel) rather than
	// whatever short-lived context triggers a given deploy. It must be
	// created before daemon.New, not after.
	d, err := daemon.New(ctx, cfg)
	if err != nil {
		log.Fatalf("oakd: init: %v", err)
	}
	defer d.Close()

	if err := d.Reconcile(ctx); err != nil {
		log.Printf("oakd: reconcile: %v", err)
	}

	runErr := d.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for id, stopErr := range d.StopAll(shutdownCtx) {
		log.Printf("oakd: stop machine %s: %v", id, stopErr)
	}

	if runErr != nil {
		log.Fatalf("oakd: run: %v", runErr)
	}
}
