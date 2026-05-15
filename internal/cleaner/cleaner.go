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
	"sync"
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
	// ForceFinalizeStuck enables the spec.finalizers escape hatch when a
	// namespace is stuck Terminating past NamespaceWait.
	//
	// Closes H2 — see reviews/2026-05-15T195833Z-review.md#h2.
	// Previously this fired automatically at --level full, which can
	// orphan cluster-scoped resources controllers were still cleaning up.
	// Now it's opt-in for `full`; `reset` keeps automatic force-finalize
	// because that's the explicit "blow it away" lever.
	ForceFinalizeStuck bool
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

// Closes H2 — see reviews/2026-05-15T195833Z-review.md#h2.
// Default per-namespace deletion wait. Bumped from 3m to 10m: real clusters
// commonly take 5-8 minutes to drain finalized resources (CSI PVCs, OCM
// CRs, cert-manager Certificates). The previous 3m default was tripping
// force-finalize on healthy teardowns.
const defaultNamespaceWait = 10 * time.Minute

// releases lists every helm release deployer can create, in *reverse* install
// order so dependents (mural) come down before dependencies (mural-crds,
// cert-manager).
type releaseSpec struct {
	name      string
	namespace string
	skip      func(*Request) bool // when non-nil + true → don't touch
	// Closes H3 — see reviews/2026-05-15T195833Z-review.md#h3.
	// disableHooks gates --no-hooks for this release. Only `mural` sets it
	// (its pre-delete hook Job is known to hang on partial installs); the
	// rest want hooks intact for clean teardown.
	disableHooks bool
}

// fixedPlan is the canonical list. It's a method on Cleaner only for clean
// substitution in tests if we ever need to (today it's effectively static).
func (c *Cleaner) plan() []releaseSpec {
	return []releaseSpec{
		{name: "mural", namespace: "mural-system", disableHooks: true},
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
//
// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// The hardcoded list is kept as a belt-and-braces fallback for any resource
// the chart doesn't yet label. The cleaner *also* sweeps by label selector
// (ancillaryNamespaceLabelSelector below) so future renames/additions are
// picked up without source changes. The union of both wins.
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

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// Label selectors used by the cleaner to discover resources owned by the
// mural release. Resources that match are merged with the hardcoded lists
// (the hardcoded lists win as a static fallback when labels are missing).
const (
	// ancillaryNamespaceLabelSelector matches namespaces created by helm
	// (any release) or by the mural project. The cleaner intersects with
	// what mural's chart actually leaves behind by also checking against
	// the static ancillaryNamespaces list when present.
	ancillaryNamespaceLabelSelector = "app.kubernetes.io/part-of=mural,app.kubernetes.io/managed-by=Helm"
	// staleWebhookLabelSelector matches admission webhooks owned by helm
	// releases under the mural umbrella. Same union semantics.
	staleWebhookLabelSelector = "app.kubernetes.io/managed-by=Helm,app.kubernetes.io/part-of=mural"
)

// namespacesFor collects the distinct namespaces the cleaner should drop at
// LevelFull / LevelReset. Respects the same Keep* flags as plan(). Ancillary
// mural-runtime namespaces are always included once the hub is being torn
// down — they only exist because mural was running.
//
// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// The discovered list (via ListNamespacesByLabel) is merged with the static
// ancillary list. Discovery errors are non-fatal: we log and fall back to
// the static list so a label query failure doesn't strand teardown.
func (c *Cleaner) namespacesFor(ctx context.Context, k kube.Client, req Request) []string {
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
	// Label-selector sweep. Best-effort: failures are logged, not fatal.
	if k != nil {
		discovered, err := k.ListNamespacesByLabel(ctx, ancillaryNamespaceLabelSelector)
		if err != nil {
			c.logf("namespace discovery via label %q failed (continuing with static list): %v",
				ancillaryNamespaceLabelSelector, err)
		}
		for _, ns := range discovered {
			add(ns)
		}
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
//
// Closes C2 — see reviews/2026-05-15T195833Z-review.md#c2.
// ctx is checked between releases so a SIGINT during a multi-release teardown
// aborts at the next boundary instead of running every release to completion.
func (c *Cleaner) runHelmUninstalls(ctx context.Context, cluster *config.Cluster, req Request) error {
	if c.Helm == nil {
		return errors.New("cleaner: Helm uninstaller is nil")
	}
	for _, r := range c.plan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.skip != nil && r.skip(&req) {
			c.logf("skip helm uninstall %q (--keep flag set)", r.name)
			continue
		}
		err := c.Helm.Uninstall(ctx, helm.UninstallOptions{
			ReleaseName:  r.name,
			Namespace:    r.namespace,
			Kubeconfig:   cluster.Kubeconfig,
			Context:      cluster.Context,
			Timeout:      req.NamespaceWait,
			DisableHooks: r.disableHooks,
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

// namespaceDeleteConcurrency caps parallel namespace deletes. 4 is high enough
// to overlap the slow finalizer waits across mural-system, messaging,
// cert-manager, and the OCM ancillaries without overwhelming the apiserver.
const namespaceDeleteConcurrency = 4

// dropNamespaces deletes each namespace and, when force-finalize is enabled
// and a standard delete is still pending past req.NamespaceWait, clears
// spec.finalizers via the /finalize subresource (mirrors cleanup_namespace.yml
// in the Ansible role).
//
// Closes H2 + L6 — see reviews/2026-05-15T195833Z-review.md#h2, …#l6.
// Force-finalize is opt-in for level=full (via --force-finalize-stuck) and
// always-on for level=reset. Auto force-finalize was orphaning cluster-scoped
// resources whose controllers were still draining (H2). Deletions run in
// parallel with a small concurrency cap because the per-namespace wait
// dominates wall-clock — 11 namespaces × 10m serial = ~2h worst-case (L6).
func (c *Cleaner) dropNamespaces(ctx context.Context, k kube.Client, req Request) error {
	forceFinalize := req.ForceFinalizeStuck || req.Level == LevelReset
	namespaces := c.namespacesFor(ctx, k, req)

	sem := make(chan struct{}, namespaceDeleteConcurrency)
	var wg sync.WaitGroup
	var (
		errMu  sync.Mutex
		errOut error
	)
	recordErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if errOut == nil {
			errOut = err
		}
	}

	hasErr := func() bool {
		errMu.Lock()
		defer errMu.Unlock()
		return errOut != nil
	}

	for _, ns := range namespaces {
		if hasErr() {
			break
		}
		select {
		case <-ctx.Done():
			recordErr(ctx.Err())
			break
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ns string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.dropOneNamespace(ctx, k, ns, req.NamespaceWait, forceFinalize); err != nil {
				recordErr(err)
			}
		}(ns)
	}
	wg.Wait()
	return errOut
}

// dropOneNamespace runs the delete + wait + (opt) force-finalize sequence
// for a single namespace. Factored out of dropNamespaces so the parallel
// driver loop above stays readable.
func (c *Cleaner) dropOneNamespace(ctx context.Context, k kube.Client, ns string, wait time.Duration, forceFinalize bool) error {
	exists, err := k.NamespaceExists(ctx, ns)
	if err != nil {
		return err
	}
	if !exists {
		c.logf("namespace %q already gone", ns)
		return nil
	}
	c.logf("deleting namespace %q", ns)
	if err := k.DeleteNamespace(ctx, ns); err != nil {
		return err
	}
	if err := k.WaitForNamespaceGone(ctx, ns, wait); err != nil {
		if !errors.Is(err, kube.ErrTimeout) {
			return err
		}
		if !forceFinalize {
			return fmt.Errorf("namespace %q still terminating after %s; "+
				"pass --force-finalize-stuck to clear spec.finalizers (dangerous: "+
				"can orphan cluster-scoped resources) or use --level reset", ns, wait)
		}
		c.logf("namespace %q stuck terminating after %s; force-finalizing", ns, wait)
		if err := k.ForceFinalizeNamespace(ctx, ns); err != nil {
			return err
		}
		if err := k.WaitForNamespaceGone(ctx, ns, wait); err != nil {
			return fmt.Errorf("namespace %q still terminating after force-finalize: %w", ns, err)
		}
	}
	c.logf("namespace %q deleted", ns)
	return nil
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// clearStaleWebhooks sweeps both the hardcoded staleWebhooks list and any
// admission webhooks matching staleWebhookLabelSelector. The union is
// deduplicated. Label-list errors are non-fatal — fall back to the static set.
func (c *Cleaner) clearStaleWebhooks(ctx context.Context, k kube.Client) error {
	validating := append([]string{}, staleWebhooks.validating...)
	mutating := append([]string{}, staleWebhooks.mutating...)

	if extra, err := k.ListValidatingWebhooksByLabel(ctx, staleWebhookLabelSelector); err != nil {
		c.logf("validating-webhook label sweep failed (continuing with static list): %v", err)
	} else {
		validating = mergeUnique(validating, extra)
	}
	if extra, err := k.ListMutatingWebhooksByLabel(ctx, staleWebhookLabelSelector); err != nil {
		c.logf("mutating-webhook label sweep failed (continuing with static list): %v", err)
	} else {
		mutating = mergeUnique(mutating, extra)
	}

	for _, name := range validating {
		if err := k.DeleteValidatingWebhookConfiguration(ctx, name); err != nil {
			return err
		}
	}
	for _, name := range mutating {
		if err := k.DeleteMutatingWebhookConfiguration(ctx, name); err != nil {
			return err
		}
	}
	c.logf("cleared stale admission webhooks (%d validating, %d mutating)", len(validating), len(mutating))
	return nil
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, x := range a {
		if _, dup := seen[x]; !dup {
			seen[x] = struct{}{}
			out = append(out, x)
		}
	}
	for _, x := range b {
		if _, dup := seen[x]; !dup {
			seen[x] = struct{}{}
			out = append(out, x)
		}
	}
	return out
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
