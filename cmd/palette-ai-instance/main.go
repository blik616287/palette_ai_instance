// Binary palette-ai-instance is the operator CLI for PaletteAI hub + spoke
// deployments. See `palette-ai-instance --help`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"palette-ai-instance/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCmd(os.Stdout, os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
