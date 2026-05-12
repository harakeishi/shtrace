package main

import (
	"context"
	"os"

	"github.com/harakeishi/shtrace/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args, os.Stdout, os.Stderr))
}
