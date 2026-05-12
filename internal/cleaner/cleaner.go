// Package cleaner reverses what deployer.Deploy set up: helm-uninstalls the
// releases, deletes the namespaces, and prunes stale admission webhooks.
// It is the implementation behind the `palette-ai-instance cleanup` CLI
// subcommand.
package cleaner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/helm"
	"palette-ai-instance/internal/kube"
)

// Level is the destructiveness dial that maps 1:1 to `cleanup --level`.
type Level string

const (
	// LevelUninstall: helm-uninstall mural + mural-crds and stop. Leaves
	// namespaces, CRDs, ClusterRoles, and webhooks alone — the safest mode.
	LevelUninstall Level = "uninstall"
	// LevelFull: uninstall, then delete the namespaces, force-finalize any
	// stuck Terminating ones, and clear stale admission webhooks. Standard
	// teardown of everything `deploy` installs.
	LevelFull Level = "full"
	// LevelReset: skip helm entirely — force-delete namespaces and webhooks
	// directly. Use when helm itself is wedged (e.g. failed release stuck
	// mid-rollback so `helm uninstall` hangs on a pre-delete hook).
	LevelReset Level = "reset"
)

// AllLevels is the canonical list used by the CLI's flag validation.
func AllLevels() []Level { return []Level{LevelUninstall, LevelFull, LevelReset} }

// Request is the cleaner's input — mirrors the cleanup CLI flags.
type Request struct {
	ConfigPath  string
	ClusterName string
	Level       Level
	// Per-component opt-outs (full + reset levels). The hub release (mural +
	// mural-crds) is always cleaned because it's what `cleanup` is for.
	KeepCertManager bool
	KeepQueue       bool
	NamespaceWait   time.Duration // per-namespace WaitForNamespaceGone deadline
}

// Cleaner orchestrates the teardown. ConfigLoader, Helm, and Kube are exposed
// for substitution in tests; Log nils-out silently.
type Cleaner struct {
	ConfigLoader func(path string) (*config.ClusterConfig, error)
	Helm         helm.Uninstaller
	Kube         func(kubeconfig, kubeContext string) (kube.Client, error)
	Log          func(format string, args ...any)
}

// New wires the production defaults: real config loader, real kube client
// builder. The helm uninstaller is injected because the CLI also reuses the
// shared SDKInstaller it built for `deploy`.
func New(helmClient helm.Uninstaller, log func(format string, args ...any)) *Cleaner {
	return &Cleaner{
		ConfigLoader: config.Load,
		Helm:         helmClient,
		Kube:         kube.NewClient,
		Log:          log,
	}
}

// Default per-namespace deletion wait. Picked to be longer than helm's
// pre-delete hook timeout but short enough that operators don't lose hope.
const defaultNamespaceWait = 3 * time.Minute

// releases lists every helm release deployer can create, in *reverse* install
// order so dependents (mural) come down before dependencies (mural-crds,
// cert-manager).
type releaseSpec struct {
	name      string
	namespace string
	skip      func(*Request) bool // when non-nil + true → don't touch
}

// fixedPlan is the canonical list. It's a method on Cleaner only for clean
// substitution in tests if we ever need to (today it's effectively static).
func (c *Cleaner) plan() []releaseSpec {
	return []releaseSpec{
		{name: "mural", namespace: "mural-system"},
		{name: "mural-crds", namespace: "mural-system"},
		{name: "queue", namespace: "messaging", skip: func(r *Request) bool { return r.KeepQueue }},
		{name: "cert-manager", namespace: "cert-manager", skip: func(r *Request) bool { return r.KeepCertManager }},
	}
}

// ancillaryNamespaces are namespaces the mural runtime creates *after* the
// helm install — managed-cluster-set-* and open-cluster-management* come
// from the bundled OCM controller; tenant-default comes from the tenant
// controller. They survive `helm uninstall mural` and have to be cleaned up
// separately for the cluster to actually return to a fresh state.
var ancillaryNamespaces = []string{
	"managed-cluster-set-default",
	"managed-cluster-set-global",
	"managed-cluster-set-spokes",
	"open-cluster-management",
	"open-cluster-management-agent",
	"open-cluster-management-agent-addon",
	"open-cluster-management-hub",
	"tenant-default",
}

// namespacesFor collects the distinct namespaces the cleaner should drop at
// LevelFull / LevelReset. Respects the same Keep* flags as plan(). Ancillary
// mural-runtime namespaces are always included once the hub is being torn
// down — they only exist because mural was running.
func (c *Cleaner) namespacesFor(req Request) []string {
	seen := map[string]struct{}{}
	out := []string{}
	add := func(ns string) {
		if _, dup := seen[ns]; dup {
			return
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	for _, r := range c.plan() {
		if r.skip != nil && r.skip(&req) {
			continue
		}
		add(r.namespace)
	}
	for _, ns := range ancillaryNamespaces {
		add(ns)
	}
	return out
}

// staleWebhooks are admission webhooks the mural chart leaves behind on a
// half-failed install. The Ansible role's prequel cleans the same set.
var staleWebhooks = struct {
	validating []string
	mutating   []string
}{
	validating: []string{"ingress-nginx-admission", "fleetconfig-controller-admission", "hue-admission"},
	mutating:   []string{"fleetconfig-controller-admission", "hue-admission"},
}

// Cleanup runs the teardown for the chosen Level. Errors short-circuit at
// each phase; partial progress is preserved (the operator can retry).
func (c *Cleaner) Cleanup(ctx context.Context, req Request) error {
	cluster, err := c.resolveCluster(req)
	if err != nil {
		return err
	}
	if req.Level == "" {
		req.Level = LevelFull
	}
	if err := validateLevel(req.Level); err != nil {
		return err
	}
	if req.NamespaceWait <= 0 {
		req.NamespaceWait = defaultNamespaceWait
	}

	c.logf("cleanup level=%s cluster=%q kubeconfig=%s", req.Level, cluster.Name, cluster.Kubeconfig)

	if req.Level != LevelReset {
		if err := c.runHelmUninstalls(ctx, cluster, req); err != nil {
			return err
		}
	}

	if req.Level == LevelUninstall {
		return nil
	}

	kubeClient, err := c.newKubeClient(cluster.Kubeconfig, cluster.Context)
	if err != nil {
		return err
	}

	if err := c.dropNamespaces(ctx, kubeClient, req); err != nil {
		return err
	}
	return c.clearStaleWebhooks(ctx, kubeClient)
}

func (c *Cleaner) resolveCluster(req Request) (*config.Cluster, error) {
	if strings.TrimSpace(req.ClusterName) == "" {
		return nil, errors.New("cluster name is required")
	}
	if c.ConfigLoader == nil {
		return nil, errors.New("cleaner: ConfigLoader is nil")
	}
	cfg, err := c.ConfigLoader(req.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load cluster config: %w", err)
	}
	return cfg.Get(req.ClusterName)
}

func (c *Cleaner) newKubeClient(kubeconfig, kubeContext string) (kube.Client, error) {
	if c.Kube == nil {
		return nil, errors.New("cleaner: Kube builder is nil")
	}
	return c.Kube(kubeconfig, kubeContext)
}

// runHelmUninstalls walks the release plan in reverse-install order. Missing
// releases are logged and skipped — the operation is idempotent.
func (c *Cleaner) runHelmUninstalls(ctx context.Context, cluster *config.Cluster, req Request) error {
	if c.Helm == nil {
		return errors.New("cleaner: Helm uninstaller is nil")
	}
	for _, r := range c.plan() {
		if r.skip != nil && r.skip(&req) {
			c.logf("skip helm uninstall %q (--keep flag set)", r.name)
			continue
		}
		err := c.Helm.Uninstall(ctx, helm.UninstallOptions{
			ReleaseName: r.name,
			Namespace:   r.namespace,
			Kubeconfig:  cluster.Kubeconfig,
			Context:     cluster.Context,
			Timeout:     req.NamespaceWait,
		})
		switch {
		case err == nil:
			c.logf("uninstalled release %q in %q", r.name, r.namespace)
		case errors.Is(err, helm.ErrReleaseNotFound):
			c.logf("release %q in %q not present, skipping", r.name, r.namespace)
		default:
			return fmt.Errorf("uninstall %s: %w", r.name, err)
		}
	}
	return nil
}

// dropNamespaces deletes each namespace and, when the standard delete is
// still pending past req.NamespaceWait, force-finalizes it (mirrors
// cleanup_namespace.yml in the Ansible role).
func (c *Cleaner) dropNamespaces(ctx context.Context, k kube.Client, req Request) error {
	for _, ns := range c.namespacesFor(req) {
		exists, err := k.NamespaceExists(ctx, ns)
		if err != nil {
			return err
		}
		if !exists {
			c.logf("namespace %q already gone", ns)
			continue
		}
		c.logf("deleting namespace %q", ns)
		if err := k.DeleteNamespace(ctx, ns); err != nil {
			return err
		}
		if err := k.WaitForNamespaceGone(ctx, ns, req.NamespaceWait); err != nil {
			if !errors.Is(err, kube.ErrTimeout) {
				return err
			}
			c.logf("namespace %q stuck terminating after %s; force-finalizing", ns, req.NamespaceWait)
			if err := k.ForceFinalizeNamespace(ctx, ns); err != nil {
				return err
			}
			if err := k.WaitForNamespaceGone(ctx, ns, req.NamespaceWait); err != nil {
				return fmt.Errorf("namespace %q still terminating after force-finalize: %w", ns, err)
			}
		}
		c.logf("namespace %q deleted", ns)
	}
	return nil
}

func (c *Cleaner) clearStaleWebhooks(ctx context.Context, k kube.Client) error {
	for _, name := range staleWebhooks.validating {
		if err := k.DeleteValidatingWebhookConfiguration(ctx, name); err != nil {
			return err
		}
	}
	for _, name := range staleWebhooks.mutating {
		if err := k.DeleteMutatingWebhookConfiguration(ctx, name); err != nil {
			return err
		}
	}
	c.logf("cleared stale admission webhooks")
	return nil
}

func validateLevel(l Level) error {
	for _, ok := range AllLevels() {
		if ok == l {
			return nil
		}
	}
	return fmt.Errorf("invalid level %q (want one of %v)", l, AllLevels())
}

func (c *Cleaner) logf(format string, args ...any) {
	if c.Log == nil {
		return
	}
	c.Log(format, args...)
}
