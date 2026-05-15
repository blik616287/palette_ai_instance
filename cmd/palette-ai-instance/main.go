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

// Closes M5 — see reviews/2026-05-15T195833Z-review.md#m5.
// Stamped at link time via -ldflags="-X main.version=... -X main.gitSHA=...
// -X main.buildTime=...". Default values are returned by `--version` when
// the binary was built without -ldflags, which never happens in CI but is
// the normal local-dev case.
var (
	version   = "dev"
	gitSHA    = "unknown"
	buildTime = "unknown"
)

func main() {
	os.Exit(run())
}

// run is the testable entrypoint: it returns the exit code so the deferred
// signal stop runs before os.Exit. The gocritic `exitAfterDefer` lint
// flagged the previous shape because os.Exit inside main bypassed defer.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCmd(os.Stdout, os.Stderr)
	cli.RegisterVersion(root, cli.VersionInfo{
		Version:   version,
		GitSHA:    gitSHA,
		BuildTime: buildTime,
	})
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
