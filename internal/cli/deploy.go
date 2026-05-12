package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/deployer"
	"palette-ai-instance/internal/helm"
)

// deployFlags is the raw flag-bound input. Per-cluster defaults from the
// config file merge with these via mergeDeployRequest — CLI values that the
// user explicitly set override the config, anything else falls through to
// the config-provided defaults.
type deployFlags struct {
	configPath                     string
	clusterName                    string
	version                        string
	valuesFile                     string
	chartURI                       string
	setValues                      []string
	crdsChartURI                   string
	certManagerChartURI            string
	certManagerVersion             string
	certManagerValuesFile          string
	certManagerSkipCreateNamespace bool
	crdsVersion                    string
	fluxChartURI                   string
	fluxVersion                    string
	fluxValuesFile                 string
	queueChartURI                  string
	queueVersion                   string
	queueValuesFile                string
	helmTimeout                    time.Duration
	validate                       bool
	validateWait                   time.Duration
}

// deployRunner is the seam tests use to inject a fake deployer.
type deployRunner func(ctx context.Context, req deployer.Request, out io.Writer) error

// NewDeployCmd returns the `deploy` subcommand.
func NewDeployCmd(out, errOut io.Writer) *cobra.Command {
	return newDeployCmd(out, errOut, defaultDeployRunner)
}

func newDeployCmd(out, errOut io.Writer, run deployRunner) *cobra.Command {
	f := &deployFlags{}

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy PaletteAI hub and spokes onto one or more clusters",
		Long: "Deploys the PaletteAI hub and all connected spokes.\n\n" +
			"Targets every cluster in --config unless --cluster-name is given.\n" +
			"All other flags are optional: when not set on the CLI, the\n" +
			"`deploy:` block under each cluster in cluster_config.yaml is\n" +
			"consulted; a CLI flag that *was* set overrides the config value.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeployCmd(cmd, f, run, out, errOut)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.configPath, "config", "",
		"path to cluster_config.yaml (required). Relative paths inside the file resolve from this file's directory")
	flags.StringVar(&f.clusterName, "cluster-name", "",
		"cluster from cluster_config.yaml to deploy to; if omitted, every cluster in the file is deployed in order")
	flags.StringVar(&f.version, "version", "", "version of mural")
	flags.StringVar(&f.valuesFile, "values-file", "", "helm chart values file")
	flags.StringVar(&f.chartURI, "chart-uri", "", "URI of the mural helm chart (oci://, https://, or local path)")
	flags.StringArrayVar(&f.setValues, "set", nil, "helm-style overrides for the mural values, e.g. --set global.dns.domain=paletteai.local (repeatable)")
	flags.StringVar(&f.crdsChartURI, "crds-chart-uri", "", "optional: URI of the mural-crds chart, installed before the mural chart")
	flags.StringVar(&f.crdsVersion, "crds-version", "", "version of the mural-crds chart (OCI/repo refs only)")
	flags.StringVar(&f.certManagerChartURI, "cert-manager-chart-uri", "", "optional: URI of the cert-manager chart, installed into cert-manager namespace before CRDs+mural")
	flags.StringVar(&f.certManagerVersion, "cert-manager-version", "", "version of the cert-manager chart (OCI/repo refs only)")
	flags.StringVar(&f.certManagerValuesFile, "cert-manager-values-file", "", "values file for the cert-manager install")
	flags.BoolVar(&f.certManagerSkipCreateNamespace, "cert-manager-no-create-ns", false, "skip helm --create-namespace for cert-manager (use when the chart ships its own Namespace, e.g. Spectro FIPS)")
	flags.StringVar(&f.fluxChartURI, "flux-chart-uri", "", "optional: URI of the flux2 chart, installed into flux-system before CRDs+mural so PaletteAI can use Flux for helm queueing")
	flags.StringVar(&f.fluxVersion, "flux-version", "", "version of the flux2 chart (OCI/repo refs only)")
	flags.StringVar(&f.fluxValuesFile, "flux-values-file", "", "values file for the flux2 install")
	flags.StringVar(&f.queueChartURI, "queue-chart-uri", "", "optional: URI of a durable-but-simple messaging-queue chart (e.g. Bitnami RabbitMQ, NATS JetStream), installed into the messaging namespace before CRDs+mural")
	flags.StringVar(&f.queueVersion, "queue-version", "", "version of the messaging-queue chart (OCI/repo refs only)")
	flags.StringVar(&f.queueValuesFile, "queue-values-file", "", "values file for the messaging-queue install")
	flags.DurationVar(&f.helmTimeout, "helm-timeout", 20*time.Minute, "per-release helm install/upgrade timeout")
	flags.BoolVar(&f.validate, "validate", false, "after the deploy, wait for pods Ready, derive the public URL from the dex Ingress + ingress-nginx Service, and print connection details")
	flags.DurationVar(&f.validateWait, "validate-wait", 5*time.Minute, "max time to wait for pods to become Ready during --validate")

	_ = cmd.MarkFlagRequired("config")
	return cmd
}

// flagSetCheck lets the merge logic ask "did the user actually set this
// flag, or is it sitting at its default?". cmd.Flags().Changed satisfies it.
type flagSetCheck func(name string) bool

// runDeployCmd loads cluster_config, narrows to one cluster (or all), and
// dispatches one deployer.Request per cluster.
func runDeployCmd(cmd *cobra.Command, f *deployFlags, run deployRunner, out, errOut io.Writer) error {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return err
	}

	clusters, err := selectClusters(cfg, f.clusterName)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return err
	}

	for i := range clusters {
		cluster := clusters[i]
		req := mergeDeployRequest(cmd.Flags().Changed, f, &cluster)
		fmt.Fprintf(out, "→ deploying cluster %q (%d/%d)\n", cluster.Name, i+1, len(clusters))
		if err := run(cmd.Context(), req, out); err != nil {
			return fmt.Errorf("cluster %q: %w", cluster.Name, err)
		}
	}
	return nil
}

// selectClusters returns the named cluster, or every cluster if name == "".
func selectClusters(cfg *config.ClusterConfig, name string) ([]config.Cluster, error) {
	if name == "" {
		return cfg.Clusters, nil
	}
	cluster, err := cfg.Get(name)
	if err != nil {
		return nil, err
	}
	return []config.Cluster{*cluster}, nil
}

// mergeDeployRequest layers values in this order, lowest-priority first:
//  1. the cluster's deploy: defaults from cluster_config.yaml
//  2. the cobra flag default (whatever StringVar/etc. baked in)
//  3. the CLI value if cmd.Flags().Changed("name") is true
//
// Concretely: if the user set --validate on the CLI we honour that; otherwise
// we use cluster.Deploy.Validate from config. Identical for every other flag.
func mergeDeployRequest(changed flagSetCheck, f *deployFlags, cluster *config.Cluster) deployer.Request {
	d := cluster.Deploy
	return deployer.Request{
		ConfigPath:                     f.configPath,
		ClusterName:                    cluster.Name,
		MuralVersion:                   pickStr(changed, "version", f.version, d.Version),
		ValuesFile:                     pickStr(changed, "values-file", f.valuesFile, d.ValuesFile),
		SetValues:                      pickStrSlice(changed, "set", f.setValues, d.SetValues),
		ChartURI:                       pickStr(changed, "chart-uri", f.chartURI, d.ChartURI),
		CRDsChartURI:                   pickStr(changed, "crds-chart-uri", f.crdsChartURI, d.CRDsChartURI),
		CRDsVersion:                    pickStr(changed, "crds-version", f.crdsVersion, d.CRDsVersion),
		CertManagerChartURI:            pickStr(changed, "cert-manager-chart-uri", f.certManagerChartURI, d.CertManagerChartURI),
		CertManagerVersion:             pickStr(changed, "cert-manager-version", f.certManagerVersion, d.CertManagerVersion),
		CertManagerValuesFile:          pickStr(changed, "cert-manager-values-file", f.certManagerValuesFile, d.CertManagerValuesFile),
		CertManagerSkipCreateNamespace: pickBool(changed, "cert-manager-no-create-ns", f.certManagerSkipCreateNamespace, d.CertManagerSkipCreateNamespace),
		FluxChartURI:                   pickStr(changed, "flux-chart-uri", f.fluxChartURI, d.FluxChartURI),
		FluxVersion:                    pickStr(changed, "flux-version", f.fluxVersion, d.FluxVersion),
		FluxValuesFile:                 pickStr(changed, "flux-values-file", f.fluxValuesFile, d.FluxValuesFile),
		QueueChartURI:                  pickStr(changed, "queue-chart-uri", f.queueChartURI, d.QueueChartURI),
		QueueVersion:                   pickStr(changed, "queue-version", f.queueVersion, d.QueueVersion),
		QueueValuesFile:                pickStr(changed, "queue-values-file", f.queueValuesFile, d.QueueValuesFile),
		HelmTimeout:                    pickDuration(changed, "helm-timeout", f.helmTimeout, d.HelmTimeout),
		Validate:                       pickBool(changed, "validate", f.validate, d.Validate),
		ValidateWait:                   pickDuration(changed, "validate-wait", f.validateWait, d.ValidateWait),
	}
}

// ---- override helpers -----------------------------------------------------
//
// Each pickX returns cliVal when the user explicitly set that flag on the
// command line; otherwise it falls back to the config value. A nil changed
// (e.g. unit tests calling mergeDeployRequest directly) is treated as "no
// flag was changed" — so config wins, matching the production semantics.

func pickStr(changed flagSetCheck, name, cliVal, cfgVal string) string {
	if changed != nil && changed(name) {
		return cliVal
	}
	return cfgVal
}

func pickBool(changed flagSetCheck, name string, cliVal, cfgVal bool) bool {
	if changed != nil && changed(name) {
		return cliVal
	}
	return cfgVal
}

func pickDuration(changed flagSetCheck, name string, cliVal, cfgVal time.Duration) time.Duration {
	if changed != nil && changed(name) {
		return cliVal
	}
	return cfgVal
}

func pickStrSlice(changed flagSetCheck, name string, cliVal, cfgVal []string) []string {
	if changed != nil && changed(name) {
		return cliVal
	}
	return cfgVal
}

// defaultDeployRunner is the production wiring: real helm SDK installer.
func defaultDeployRunner(ctx context.Context, req deployer.Request, out io.Writer) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
	}
	return deployer.New(helm.NewSDKInstaller(logf), logf).Deploy(ctx, req)
}
