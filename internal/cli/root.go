// Package cli wires cobra commands into the palette-ai-instance binary.
package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// NewRootCmd returns the top-level cobra command. Output is configurable so
// tests can capture stdout/stderr without colliding with the real os.Stdout.
func NewRootCmd(out, errOut io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "palette-ai-instance",
		Short:         "Manage PaletteAI instances",
		Long:          "palette-ai-instance is the CLI used to deploy and lifecycle PaletteAI hub + spokes against a target cluster defined in cluster_config.yaml.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetOut(out)
	cmd.SetErr(errOut)

	cmd.AddCommand(NewDeployCmd(out, errOut))
	cmd.AddCommand(NewCleanupCmd(out, errOut))
	return cmd
}
