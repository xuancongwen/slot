// Command slot runs the Slot booking server.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"homelab/slot/internal/slot"
)

func main() {
	if err := run(); err != nil {
		slog.Error("slot", "error", err)
		os.Exit(1)
	}
}

func run() error {
	c, err := slot.LoadConfig()
	if err != nil {
		return err
	}
	app, err := slot.New(c)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx)
}
