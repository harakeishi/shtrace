package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/harakeishi/shtrace/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
