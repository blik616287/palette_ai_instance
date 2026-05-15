package helm

import (
	"context"
	"errors"
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	helmcli "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// configFactory builds the action.Configuration + helmcli.EnvSettings that
// SDKInstaller.Apply uses. Pulled out as a struct field so tests can inject
// an in-memory configuration and exercise the install/upgrade paths without
// needing a real cluster.
type configFactory func(opts InstallOptions, log LogFunc) (*action.Configuration, *helmcli.EnvSettings, error)

// SDKInstaller is the production Installer. Each Apply call builds its own
// action.Configuration from the given kubeconfig so the installer is safe to
// reuse across clusters / namespaces.
type SDKInstaller struct {
	Log     LogFunc
	factory configFactory
}

// NewSDKInstaller constructs an SDKInstaller. A nil log is silenced.
func NewSDKInstaller(log LogFunc) *SDKInstaller {
	return &SDKInstaller{Log: log, factory: defaultConfigFactory}
}

// Apply implements Installer. If the release already exists in the given
// namespace we upgrade it; otherwise we install it ("helm upgrade --install").
func (s *SDKInstaller) Apply(ctx context.Context, opts InstallOptions) (*Release, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	factory := s.factory
	if factory == nil {
		factory = defaultConfigFactory
	}
	cfg, env, err := factory(opts, s.Log)
	if err != nil {
		return nil, err
	}

	vals, err := loadValues(opts.ValuesFile, opts.SetValues, env)
	if err != nil {
		return nil, err
	}

	exists, err := releaseExists(cfg, opts.ReleaseName)
	if err != nil {
		return nil, err
	}

	var rel *release.Release
	if exists {
		s.logf("upgrading release %q in namespace %q", opts.ReleaseName, opts.Namespace)
		rel, err = runUpgrade(ctx, cfg, opts, env, vals)
	} else {
		s.logf("installing release %q into namespace %q", opts.ReleaseName, opts.Namespace)
		rel, err = runInstall(ctx, cfg, opts, env, vals)
	}
	if err != nil {
		return nil, err
	}
	return toRelease(rel), nil
}

// defaultConfigFactory is the production configFactory. It builds an
// action.Configuration scoped to the target cluster + namespace, paired with
// helmcli.EnvSettings for chart locators.
func defaultConfigFactory(opts InstallOptions, log LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
	rest := genericclioptions.NewConfigFlags(false)
	rest.KubeConfig = strPtr(opts.Kubeconfig)
	if opts.Context != "" {
		rest.Context = strPtr(opts.Context)
	}
	rest.Namespace = strPtr(opts.Namespace)

	env := helmcli.New()
	env.KubeConfig = opts.Kubeconfig
	env.KubeContext = opts.Context
	env.SetNamespace(opts.Namespace)

	helmLog := func(format string, args ...any) {
		if log != nil {
			log("helm: "+format, args...)
		}
	}

	cfg := new(action.Configuration)
	if err := cfg.Init(rest, opts.Namespace, "secret", helmLog); err != nil {
		return nil, nil, fmt.Errorf("init helm config: %w", err)
	}

	// Required so action.LocateChart can resolve `oci://…` URIs. Without this
	// the chart locator errors with "missing registry client".
	regClient, err := registry.NewClient()
	if err != nil {
		return nil, nil, fmt.Errorf("new helm registry client: %w", err)
	}
	cfg.RegistryClient = regClient

	return cfg, env, nil
}

func (s *SDKInstaller) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	s.Log(format, args...)
}

// Uninstall removes a release. A release that doesn't exist returns
// ErrReleaseNotFound so the caller can treat it as a no-op.
//
// Closes C2 + H3 — see reviews/2026-05-15T195833Z-review.md#c2, …#h3.
// Helm's action.Uninstall.Run doesn't take a ctx — but we still check
// ctx.Err() at entry and just before invoking Run, so a SIGINT during a
// long sequence of uninstalls aborts at the next release boundary instead
// of being silently ignored (C2). DisableHooks is no longer hardcoded;
// callers opt in per-release via UninstallOptions.DisableHooks (H3).
func (s *SDKInstaller) Uninstall(ctx context.Context, opts UninstallOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := opts.Validate(); err != nil {
		return err
	}

	factory := s.factory
	if factory == nil {
		factory = defaultConfigFactory
	}
	// Repurpose InstallOptions to carry the same kube routing info — the
	// factory only reads Kubeconfig / Context / Namespace.
	cfg, _, err := factory(InstallOptions{
		Kubeconfig: opts.Kubeconfig,
		Context:    opts.Context,
		Namespace:  opts.Namespace,
	}, s.Log)
	if err != nil {
		return err
	}

	exists, err := releaseExists(cfg, opts.ReleaseName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %q in %q", ErrReleaseNotFound, opts.ReleaseName, opts.Namespace)
	}

	un := action.NewUninstall(cfg)
	un.KeepHistory = opts.KeepHistory
	un.Timeout = opts.Timeout
	un.DisableHooks = opts.DisableHooks

	if err := ctx.Err(); err != nil {
		return err
	}
	s.logf("uninstalling release %q in namespace %q", opts.ReleaseName, opts.Namespace)
	if _, err := un.Run(opts.ReleaseName); err != nil {
		return fmt.Errorf("helm uninstall %q: %w", opts.ReleaseName, err)
	}
	return nil
}

// locateAndLoad resolves uri to a local chart path (downloading OCI / HTTPS
// charts as needed) and parses the chart. The ChartPathOptions argument must
// come from an action.Install / action.Upgrade so it carries the registry
// client needed for `oci://` URIs — that field is unexported, so we can't
// build a ChartPathOptions in isolation.
func locateAndLoad(co *action.ChartPathOptions, env *helmcli.EnvSettings, uri string) (*chart.Chart, error) {
	path, err := co.LocateChart(uri, env)
	if err != nil {
		return nil, fmt.Errorf("locate chart %q: %w", uri, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart %q: %w", path, err)
	}
	return ch, nil
}

// loadValues merges a values.yaml file with helm-style `--set` overrides.
// Both inputs are optional; an empty result means "use the chart's defaults".
// Overrides apply on top of the file (helm's documented merge order).
func loadValues(path string, sets []string, env *helmcli.EnvSettings) (map[string]any, error) {
	opt := values.Options{Values: sets}
	if path != "" {
		opt.ValueFiles = []string{path}
	}
	if path == "" && len(sets) == 0 {
		return map[string]any{}, nil
	}
	vals, err := opt.MergeValues(getter.All(env))
	if err != nil {
		return nil, fmt.Errorf("merge values (file=%q sets=%d): %w", path, len(sets), err)
	}
	return vals, nil
}

// releaseExists returns true when the named release has at least one
// *active* revision (status deployed or in-flight) in the namespace targeted
// by cfg.
//
// Closes M6 — see reviews/2026-05-15T195833Z-review.md#m6.
// Uninstalled-with-kept-history releases return false so Apply can re-install
// over them — helm refuses to `upgrade` a release whose latest revision has
// status "uninstalled".
func releaseExists(cfg *action.Configuration, name string) (bool, error) {
	hist := action.NewHistory(cfg)
	rels, err := hist.Run(name)
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("query release history: %w", err)
	}
	// hist.Run returns revisions in ascending-revision order; we care about
	// the latest one's status. An "uninstalled" tail means there's no live
	// release to upgrade, even though history exists. Info is always set
	// by helm's storage layer (it carries the status secret labels) so we
	// don't guard for nil here.
	if len(rels) == 0 {
		return false, nil
	}
	switch rels[len(rels)-1].Info.Status {
	case release.StatusUninstalled, release.StatusUninstalling:
		return false, nil
	}
	return true, nil
}

func runInstall(ctx context.Context, cfg *action.Configuration, opts InstallOptions,
	env *helmcli.EnvSettings, vals map[string]any) (*release.Release, error) {
	ins := action.NewInstall(cfg)
	ins.ReleaseName = opts.ReleaseName
	ins.Namespace = opts.Namespace
	ins.CreateNamespace = opts.CreateNS
	ins.Wait = opts.Wait
	ins.Timeout = opts.Timeout
	ins.Version = opts.Version
	ins.PostRenderer = opts.PostRenderer

	ch, err := locateAndLoad(&ins.ChartPathOptions, env, opts.ChartURI)
	if err != nil {
		return nil, err
	}

	rel, err := ins.RunWithContext(ctx, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm install %q: %w", opts.ReleaseName, err)
	}
	return rel, nil
}

func runUpgrade(ctx context.Context, cfg *action.Configuration, opts InstallOptions,
	env *helmcli.EnvSettings, vals map[string]any) (*release.Release, error) {
	up := action.NewUpgrade(cfg)
	up.Namespace = opts.Namespace
	up.Wait = opts.Wait
	up.Timeout = opts.Timeout
	up.Version = opts.Version
	up.PostRenderer = opts.PostRenderer

	ch, err := locateAndLoad(&up.ChartPathOptions, env, opts.ChartURI)
	if err != nil {
		return nil, err
	}

	rel, err := up.RunWithContext(ctx, opts.ReleaseName, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm upgrade %q: %w", opts.ReleaseName, err)
	}
	return rel, nil
}

func toRelease(r *release.Release) *Release {
	if r == nil {
		return nil
	}
	out := &Release{
		Name:      r.Name,
		Namespace: r.Namespace,
		Revision:  r.Version,
	}
	if r.Info != nil {
		out.Status = r.Info.Status.String()
	}
	return out
}

func strPtr(s string) *string { return &s }
