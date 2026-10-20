package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tend: load config: %v\n", err)
		os.Exit(1)
	}

	// Cancel on SIGINT / SIGTERM for clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Run(ctx, cfg, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// tend exec exits with the status of the command it ran, which has
		// said what it had to.
		var exit *cli.ExitError
		if errors.As(err, &exit) {
			stop()
			os.Exit(exit.Code)
		}
		fmt.Fprintf(os.Stderr, "tend: %v\n", err)
		os.Exit(1)
	}
}
