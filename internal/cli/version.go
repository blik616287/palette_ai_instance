// Closes M5 — see reviews/2026-05-15T195833Z-review.md#m5.
// Operator-facing version stamping. main.go injects the values at link time
// via -ldflags="-X main.version=... -X main.gitSHA=... -X main.buildTime=...".
package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// VersionInfo carries the link-time stamps. Zero values render as "dev" /
// "unknown" so an un-stamped local build is still self-identifying.
type VersionInfo struct {
	Version   string
	GitSHA    string
	BuildTime string
}

// RegisterVersion adds a `version` subcommand under root and wires
// `--version` on the root command (cobra's standard pattern).
func RegisterVersion(root *cobra.Command, info VersionInfo) {
	root.Version = fmt.Sprintf("%s (%s, built %s, %s)",
		info.Version, info.GitSHA, info.BuildTime, runtime.Version())
	root.SetVersionTemplate("palette-ai-instance {{.Version}}\n")

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version, git SHA, and build time",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "palette-ai-instance:\n")
			fmt.Fprintf(out, "  version:    %s\n", info.Version)
			fmt.Fprintf(out, "  git sha:    %s\n", info.GitSHA)
			fmt.Fprintf(out, "  build time: %s\n", info.BuildTime)
			fmt.Fprintf(out, "  go:         %s\n", runtime.Version())
			return nil
		},
	})
}
