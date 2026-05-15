package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"palette-ai-instance/internal/cleaner"
	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/helm"
)

// cleanupFlags is the raw flag-bound input. Unlike deploy, cleanup options
// are CLI-only — destructive defaults can't lurk in cluster_config.yaml.
type cleanupFlags struct {
	configPath         string
	clusterName        string
	level              string
	keepCertManager    bool
	keepQueue          bool
	namespaceWait      time.Duration
	forceFinalizeStuck bool
}

// cleanupRunner is the seam tests use to inject a fake cleaner.
type cleanupRunner func(ctx context.Context, req cleaner.Request, out io.Writer) error

// NewCleanupCmd returns the `cleanup` subcommand. errOut is accepted for
// symmetry with NewDeployCmd; today the cleaner streams everything to out.
func NewCleanupCmd(out, errOut io.Writer) *cobra.Command {
	return newCleanupCmd(out, errOut, defaultCleanupRunner)
}

func newCleanupCmd(out, errOut io.Writer, run cleanupRunner) *cobra.Command {
	f := &cleanupFlags{}

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete the PaletteAI hub and its connected spokes from one or more clusters",
		Long: "Deletes the PaletteAI hub and all connected spokes.\n\n" +
			"Targets every cluster in --config unless --cluster-name is given.\n" +
			"Unlike `deploy`, cleanup options are CLI-only — they cannot be set\n" +
			"in cluster_config.yaml. Destructive intent must be explicit on\n" +
			"every invocation.\n\n" +
			"Three levels of teardown are available via --level:\n" +
			"  uninstall  helm uninstall mural + mural-crds and stop. Leaves namespaces,\n" +
			"             CRDs, ClusterRoles, and webhooks alone.\n" +
			"  full       (default) uninstall, then delete the namespaces, force-finalize\n" +
			"             any stuck Terminating ones, and clear stale admission webhooks.\n" +
			"  reset      skip helm entirely and force-delete namespaces + webhooks. Use\n" +
			"             when helm itself is wedged on a half-failed release.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCleanupCmd(cmd, f, run, out, errOut)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.configPath, "config", "",
		"path to cluster_config.yaml (required). Relative paths inside the file resolve from this file's directory")
	flags.StringVar(&f.clusterName, "cluster-name", "",
		"cluster from cluster_config.yaml to clean up; if omitted, every cluster in the file is cleaned in order")
	flags.StringVar(&f.level, "level", string(cleaner.LevelFull),
		"teardown level: uninstall | full | reset")
	flags.BoolVar(&f.keepCertManager, "keep-cert-manager", false,
		"don't uninstall cert-manager (use when it's shared with other tenants)")
	flags.BoolVar(&f.keepQueue, "keep-queue", false,
		"don't uninstall the messaging-queue release")
	flags.DurationVar(&f.namespaceWait, "namespace-wait", 10*time.Minute,
		"per-namespace deletion deadline before erroring (or force-finalizing if --force-finalize-stuck)")
	flags.BoolVar(&f.forceFinalizeStuck, "force-finalize-stuck", false,
		"DANGEROUS: when a namespace is stuck Terminating past --namespace-wait, clear "+
			"spec.finalizers via /finalize. Can orphan cluster-scoped resources whose "+
			"controllers were still draining. Implicit at --level reset.")

	_ = cmd.MarkFlagRequired("config")
	return cmd
}

// runCleanupCmd loads cluster_config, narrows to one cluster (or all), and
// dispatches one cleaner.Request per cluster.
//
// Closes C3 — see reviews/2026-05-15T195833Z-review.md#c3.
// main.go is the single stderr sink: we return errors verbatim and let it
// print one "error: ..." line instead of double-printing here too.
func runCleanupCmd(cmd *cobra.Command, f *cleanupFlags, run cleanupRunner, out, _ io.Writer) error {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	clusters, err := selectClusters(cfg, f.clusterName)
	if err != nil {
		return err
	}

	for i := range clusters {
		cluster := clusters[i]
		req := buildCleanupRequest(f, &cluster)
		fmt.Fprintf(out, "→ cleaning cluster %q (%d/%d, level=%s)\n",
			cluster.Name, i+1, len(clusters), req.Level)
		if err := run(cmd.Context(), req, out); err != nil {
			// Closes H5 — see reviews/2026-05-15T195833Z-review.md#h5.
			// "→ cleaning cluster X" above carries cluster ID; cleaner
			// wraps per-step. Re-wrapping with `cluster %q` double-stamps.
			return err
		}
	}
	return nil
}

// buildCleanupRequest assembles a cleaner.Request straight from CLI flags
// (plus the cluster's identity from config). No per-cluster defaults
// participate — see the package doc on Cluster for the rationale.
func buildCleanupRequest(f *cleanupFlags, cluster *config.Cluster) cleaner.Request {
	return cleaner.Request{
		ConfigPath:         f.configPath,
		ClusterName:        cluster.Name,
		Level:              cleaner.Level(strings.ToLower(strings.TrimSpace(f.level))),
		KeepCertManager:    f.keepCertManager,
		KeepQueue:          f.keepQueue,
		NamespaceWait:      f.namespaceWait,
		ForceFinalizeStuck: f.forceFinalizeStuck,
	}
}

// defaultCleanupRunner wires the production cleaner with the real helm
// uninstaller. The cleaner builds its own kube client per invocation.
func defaultCleanupRunner(ctx context.Context, req cleaner.Request, out io.Writer) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
	}
	c := cleaner.New(helm.NewSDKInstaller(logf), logf)
	return c.Cleanup(ctx, req)
}
