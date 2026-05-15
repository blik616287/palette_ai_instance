// Package deployer orchestrates a PaletteAI deploy: resolve the target
// cluster, install the mural-crds chart, and install/upgrade the mural chart.
package deployer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/helm"
	"palette-ai-instance/internal/kube"
	"palette-ai-instance/internal/postrender"
)

// Request is the full input for one deploy invocation. It maps 1:1 to the
// `deploy` CLI flags, plus the resolved cluster_config path.
type Request struct {
	ConfigPath   string
	ClusterName  string
	MuralVersion string
	ValuesFile   string
	// SetValues mirrors helm's --set flag for the mural release. Applied
	// on top of ValuesFile so callers can inject domain / Dex secrets /
	// bcrypt'd admin hash without pre-rendering the values file.
	SetValues    []string
	ChartURI     string
	CRDsChartURI string // optional — when set, installed before mural
	// CertManagerChartURI, when set, installs cert-manager (required by
	// mural) into the cert-manager namespace before CRDs + mural.
	CertManagerChartURI   string
	CertManagerVersion    string
	CertManagerValuesFile string
	// CertManagerSkipCreateNamespace skips helm's `--create-namespace`
	// for the cert-manager step. Needed for the Spectro FIPS chart which
	// bundles its own Namespace as a pre-install hook.
	CertManagerSkipCreateNamespace bool
	CRDsVersion                    string
	// QueueChartURI, when set, installs a durable-but-simple messaging
	// queue (e.g. Bitnami RabbitMQ, nats-io/nats with JetStream) into the
	// `messaging` namespace before CRDs+mural. Used by PaletteAI workloads
	// that need at-least-once delivery between components.
	QueueChartURI   string
	QueueVersion    string
	QueueValuesFile string
	HelmTimeout     time.Duration
	// Validate, when true, runs a post-install verification pass that
	// waits for pods Ready in cluster.Namespace, reads the deployed dex
	// Ingress + ingress-nginx Service to derive the public URL, and
	// prints connection details. Failures bubble up as deploy errors.
	Validate     bool
	ValidateWait time.Duration
}

// Defaults for cosmetic fields; the CLI layer can override.
const (
	muralReleaseName       = "mural"
	crdsReleaseName        = "mural-crds"
	certManagerReleaseName = "cert-manager"
	certManagerNamespace   = "cert-manager"
	queueReleaseName       = "queue"
	queueNamespace         = "messaging"
	defaultTimeout         = 20 * time.Minute
)

// Deployer wires together the cluster-config loader and the helm installer.
// All collaborators are exposed for substitution in tests.
type Deployer struct {
	ConfigLoader func(path string) (*config.ClusterConfig, error)
	Helm         helm.Installer
	// Kube builds a cluster client for the --validate post-install pass.
	// Nil is fine when Request.Validate is false.
	Kube func(kubeconfig, kubeContext string) (kube.Client, error)
	Log  func(format string, args ...any)
}

// New constructs a Deployer with the production wirings.
func New(helmInstaller helm.Installer, log func(format string, args ...any)) *Deployer {
	return &Deployer{
		ConfigLoader: config.Load,
		Helm:         helmInstaller,
		Kube:         kube.NewClient,
		Log:          log,
	}
}

// Deploy runs the full sequence. On any step error it returns immediately —
// callers should not assume partial completion is idempotent (helm itself is,
// but the Palette stub is best-effort).
func (d *Deployer) Deploy(ctx context.Context, req Request) error {
	cluster, err := d.resolveCluster(req)
	if err != nil {
		return err
	}

	d.logf("targeting cluster %q (kubeconfig=%s namespace=%s)",
		cluster.Name, cluster.Kubeconfig, cluster.Namespace)

	// Closes M8 — see reviews/2026-05-15T195833Z-review.md#m8.
	// HelmTimeout must be positive. A zero/negative value is almost always
	// "the operator typed --helm-timeout 0 expecting `no timeout`" — but
	// helm interprets zero as "fail immediately." Reject up front rather
	// than silently substituting a different value the operator didn't ask
	// for. The cobra default of 20m applies when --helm-timeout is unset.
	if req.HelmTimeout <= 0 {
		return fmt.Errorf("helm timeout must be > 0, got %s", req.HelmTimeout)
	}
	timeout := req.HelmTimeout

	if req.CertManagerChartURI != "" {
		// cert-manager is a cluster-wide prereq, not part of the PaletteAI
		// release itself. Use the upstream Jetstack chart for day-1
		// minikube — it doesn't ship its own Namespace template, so the
		// usual CreateNamespace=true path works cleanly. (The Spectro FIPS
		// build ships templates/namespace.yaml as a pre-install hook and
		// fights helm's --create-namespace; if you have to use FIPS, set
		// --cert-manager-no-create-ns.)
		if err := d.installChart(ctx, cluster, helm.InstallOptions{
			ReleaseName: certManagerReleaseName,
			Namespace:   certManagerNamespace,
			ChartURI:    req.CertManagerChartURI,
			Version:     req.CertManagerVersion,
			ValuesFile:  req.CertManagerValuesFile,
			Wait:        true,
			Timeout:     timeout,
			CreateNS:    !req.CertManagerSkipCreateNamespace,
		}); err != nil {
			return fmt.Errorf("install %s: %w", certManagerReleaseName, err)
		}
	}

	if req.QueueChartURI != "" {
		// Durable-but-simple messaging queue (RabbitMQ / NATS JetStream)
		// lives in its own `messaging` namespace so its lifecycle is
		// decoupled from the mural release.
		if err := d.installChart(ctx, cluster, helm.InstallOptions{
			ReleaseName: queueReleaseName,
			Namespace:   queueNamespace,
			ChartURI:    req.QueueChartURI,
			Version:     req.QueueVersion,
			ValuesFile:  req.QueueValuesFile,
			Wait:        true,
			Timeout:     timeout,
			CreateNS:    true,
		}); err != nil {
			return fmt.Errorf("install %s: %w", queueReleaseName, err)
		}
	}

	if req.CRDsChartURI != "" {
		if err := d.installChart(ctx, cluster, helm.InstallOptions{
			ReleaseName: crdsReleaseName,
			Namespace:   cluster.Namespace,
			ChartURI:    req.CRDsChartURI,
			Version:     req.CRDsVersion,
			Wait:        true,
			Timeout:     timeout,
			CreateNS:    true,
		}); err != nil {
			return fmt.Errorf("install %s: %w", crdsReleaseName, err)
		}
	}

	// The mural chart has two rendering bugs that fix-nil-values papers over.
	// The post-renderer mirrors ansible/paletteai/fix-nil-values.sh.
	//
	// Closes L1 — see reviews/2026-05-15T195833Z-review.md#l1.
	// Wait is intentionally false: combining helm's wait phase with a
	// post-renderer trips a race where helm reports "no Ingress with the
	// name X found" even though the resource is present in the API. The
	// post-install pod-readiness wait happens out-of-band in validate()
	// below when Request.Validate is set.
	if err := d.installChart(ctx, cluster, helm.InstallOptions{
		ReleaseName:  muralReleaseName,
		Namespace:    cluster.Namespace,
		ChartURI:     req.ChartURI,
		Version:      req.MuralVersion,
		ValuesFile:   req.ValuesFile,
		SetValues:    req.SetValues,
		Wait:         false,
		Timeout:      timeout,
		CreateNS:     true,
		PostRenderer: postrender.FixNilValues{},
	}); err != nil {
		return fmt.Errorf("install %s: %w", muralReleaseName, err)
	}

	if req.Validate {
		if err := d.validate(ctx, cluster, req); err != nil {
			return fmt.Errorf("validate: %w", err)
		}
	}
	return nil
}

// validate runs the post-install verification pass. The logic mirrors what
// an operator would do by hand: wait for pods, then ask the cluster what
// URLs to use. Connection details print via d.logf so the same output goes
// wherever the CLI's stdout is wired.
const (
	validateDefaultWait  = 5 * time.Minute
	dexIngressName       = "dex"
	ingressControllerSvc = "ingress-nginx-controller"
	ingressHTTPSPortName = "https"
)

func (d *Deployer) validate(ctx context.Context, cluster *config.Cluster, req Request) error {
	if d.Kube == nil {
		return errors.New("deployer: Kube builder is nil (required by --validate)")
	}
	k, err := d.Kube(cluster.Kubeconfig, cluster.Context)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	wait := req.ValidateWait
	if wait <= 0 {
		wait = validateDefaultWait
	}

	d.logf("validate: waiting up to %s for pods in %q to be Ready", wait, cluster.Namespace)
	summary, err := k.WaitForPodsReady(ctx, cluster.Namespace, wait)
	if err != nil {
		d.logf("validate: %d/%d pods Ready; not ready: %v",
			summary.Ready, summary.Total, summary.NotReady)
		return err
	}
	d.logf("validate: %d/%d pods Ready in %q", summary.Ready, summary.Total, cluster.Namespace)

	host, err := k.GetIngressHost(ctx, cluster.Namespace, dexIngressName)
	if err != nil {
		return err
	}
	nodePort, err := k.GetServiceNodePort(ctx, cluster.Namespace, ingressControllerSvc, ingressHTTPSPortName)
	if err != nil {
		return err
	}

	base := fmt.Sprintf("https://%s:%d", host, nodePort)
	d.logf("")
	d.logf("=== PaletteAI connection details ===")
	d.logf("  Canvas UI       %s/ai", base)
	d.logf("  Dex login       %s/dex", base)
	d.logf("  Cluster         %s (kubeconfig: %s)", cluster.Name, cluster.Kubeconfig)
	d.logf("  Namespace       %s", cluster.Namespace)
	d.logf("  Note: ingress-nginx serves a self-signed cert on minikube; the browser will warn.")
	return nil
}

func (d *Deployer) resolveCluster(req Request) (*config.Cluster, error) {
	if strings.TrimSpace(req.ClusterName) == "" {
		return nil, errors.New("cluster name is required")
	}
	if strings.TrimSpace(req.ChartURI) == "" {
		return nil, errors.New("chart URI is required")
	}
	if d.ConfigLoader == nil {
		return nil, errors.New("deployer: ConfigLoader is nil")
	}
	cfg, err := d.ConfigLoader(req.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load cluster config: %w", err)
	}
	return cfg.Get(req.ClusterName)
}

func (d *Deployer) installChart(ctx context.Context, cluster *config.Cluster, base helm.InstallOptions) error {
	if d.Helm == nil {
		return errors.New("deployer: Helm installer is nil")
	}
	opts := base
	opts.Kubeconfig = cluster.Kubeconfig
	opts.Context = cluster.Context

	rel, err := d.Helm.Apply(ctx, opts)
	if err != nil {
		return err
	}
	if rel != nil {
		d.logf("release %q rev=%d status=%s", rel.Name, rel.Revision, rel.Status)
	}
	return nil
}

func (d *Deployer) logf(format string, args ...any) {
	if d.Log == nil {
		return
	}
	d.Log(format, args...)
}
