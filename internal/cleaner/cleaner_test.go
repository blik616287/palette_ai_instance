package cleaner_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"palette-ai-instance/internal/cleaner"
	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/helm"
	"palette-ai-instance/internal/kube"
)

// ---- fakes ----------------------------------------------------------------

type fakeUninstaller struct {
	calls   []helm.UninstallOptions
	missing map[string]bool // release names → return ErrReleaseNotFound
	err     error           // when set, return on every call
}

func (f *fakeUninstaller) Uninstall(_ context.Context, opts helm.UninstallOptions) error {
	f.calls = append(f.calls, opts)
	if f.err != nil {
		return f.err
	}
	if f.missing[opts.ReleaseName] {
		return helm.ErrReleaseNotFound
	}
	return nil
}

// Closes L6 — see reviews/2026-05-15T195833Z-review.md#l6.
// fakeKube is now exercised by parallel goroutines (dropNamespaces uses an
// errgroup-style worker pool); all shared state goes through a mutex so the
// race detector stays clean.
type fakeKube struct {
	mu             sync.Mutex
	existing       map[string]bool
	deletes        []string
	waits          []string
	timeouts       map[string]bool // namespaces that should timeout once
	finalized      []string
	delValidating  []string
	delMutating    []string
	failOnDeleteNS string
	// L3 — what the label-selector list methods return. Tests can preload
	// these to exercise the union behavior; default-empty matches the
	// hardcoded-list-only behavior the original tests asserted.
	labeledNamespaces []string
	labeledValidating []string
	labeledMutating   []string
	// labelListErr, when set, makes every List*ByLabel method fail — used
	// to exercise the cleaner's non-fatal fallback to the static lists.
	labelListErr error
}

func (f *fakeKube) NamespaceExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existing[name], nil
}
func (f *fakeKube) DeleteNamespace(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	if f.failOnDeleteNS == name {
		return errors.New("delete boom")
	}
	delete(f.existing, name)
	return nil
}
func (f *fakeKube) WaitForNamespaceGone(_ context.Context, name string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, name)
	if f.timeouts[name] {
		f.timeouts[name] = false // clear so a follow-up call after finalize succeeds
		return kube.ErrTimeout
	}
	return nil
}
func (f *fakeKube) ForceFinalizeNamespace(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = append(f.finalized, name)
	return nil
}
func (f *fakeKube) DeleteValidatingWebhookConfiguration(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delValidating = append(f.delValidating, name)
	return nil
}
func (f *fakeKube) DeleteMutatingWebhookConfiguration(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delMutating = append(f.delMutating, name)
	return nil
}

// L3 surface — label-selector list methods. Tests can override via the
// labeledNamespaces / labeledValidating / labeledMutating fields if a test
// wants to assert the union behavior; default zero returns are fine for the
// existing tests because they use the hardcoded list as the source of truth.
func (f *fakeKube) ListNamespacesByLabel(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelListErr != nil {
		return nil, f.labelListErr
	}
	return append([]string{}, f.labeledNamespaces...), nil
}
func (f *fakeKube) ListValidatingWebhooksByLabel(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelListErr != nil {
		return nil, f.labelListErr
	}
	return append([]string{}, f.labeledValidating...), nil
}
func (f *fakeKube) ListMutatingWebhooksByLabel(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelListErr != nil {
		return nil, f.labelListErr
	}
	return append([]string{}, f.labeledMutating...), nil
}

// Validation surface — not exercised by the cleaner; here only so fakeKube
// satisfies kube.Client. Returning zero values keeps the cleaner tests focused.
func (f *fakeKube) WaitForPodsReady(_ context.Context, _ string, _ time.Duration) (kube.PodSummary, error) {
	return kube.PodSummary{}, nil
}
func (f *fakeKube) GetIngressHost(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeKube) GetServiceNodePort(context.Context, string, string, string) (int32, error) {
	return 0, nil
}

func okCluster() *config.ClusterConfig {
	return &config.ClusterConfig{Clusters: []config.Cluster{{
		Name: "local", Kubeconfig: "/tmp/kc", Context: "local", Namespace: "mural-system",
	}}}
}
func okLoader(_ string) (*config.ClusterConfig, error) { return okCluster(), nil }

func newCleaner(h *fakeUninstaller, k *fakeKube) *cleaner.Cleaner {
	c := cleaner.New(h, nil)
	c.ConfigLoader = okLoader
	c.Kube = func(_, _ string) (kube.Client, error) { return k, nil }
	return c
}

// ---- tests ----------------------------------------------------------------

func TestCleanup_LevelUninstall_DropsAllReleases(t *testing.T) {
	h := &fakeUninstaller{}
	c := newCleaner(h, &fakeKube{})

	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local",
		Level:       cleaner.LevelUninstall,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	want := []string{"mural", "mural-crds", "queue", "cert-manager"}
	if got := releaseNames(h.calls); !equal(got, want) {
		t.Fatalf("uninstall order/list wrong:\n got %v\nwant %v", got, want)
	}
	// Closes H3 — see reviews/2026-05-15T195833Z-review.md#h3.
	// Only `mural` should have DisableHooks=true; other releases preserve
	// their hooks for clean teardown.
	for _, c := range h.calls {
		want := c.ReleaseName == "mural"
		if c.DisableHooks != want {
			t.Errorf("release %q: DisableHooks=%v want %v", c.ReleaseName, c.DisableHooks, want)
		}
	}
}

func TestCleanup_LevelUninstall_SkipsMissingReleases(t *testing.T) {
	h := &fakeUninstaller{missing: map[string]bool{"queue": true}}
	c := newCleaner(h, &fakeKube{})
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(h.calls) != 4 {
		t.Fatalf("expected all 4 (mural, mural-crds, queue, cert-manager) to be attempted, got %v", h.calls)
	}
}

func TestCleanup_LevelUninstall_PropagatesNonNotFoundError(t *testing.T) {
	h := &fakeUninstaller{err: errors.New("helm boom")}
	c := newCleaner(h, &fakeKube{})
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_LevelFull_DeletesNamespacesAndClearsWebhooks(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing: map[string]bool{
			"mural-system": true, "messaging": true, "cert-manager": true,
		},
	}
	c := newCleaner(h, k)

	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
		NamespaceWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(k.deletes) != 3 {
		t.Fatalf("expected 3 namespace deletes (mural-system, messaging, cert-manager), got %v", k.deletes)
	}
	if len(k.delValidating) == 0 || len(k.delMutating) == 0 {
		t.Fatalf("expected stale webhook deletes: validating=%v mutating=%v",
			k.delValidating, k.delMutating)
	}
}

func TestCleanup_LevelFull_DefaultsToLevelFullWhenLevelEmpty(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{existing: map[string]bool{"mural-system": true}}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{ClusterName: "local"}) // no Level
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(k.delValidating) == 0 {
		t.Fatal("default level should run webhook cleanup")
	}
}

// Closes H2 — see reviews/2026-05-15T195833Z-review.md#h2.
// LevelFull WITHOUT --force-finalize-stuck must NOT silently force-finalize.
func TestCleanup_LevelFull_StuckNamespaceErrorsWithoutForceFlag(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing: map[string]bool{"mural-system": true},
		timeouts: map[string]bool{"mural-system": true},
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
		NamespaceWait: 10 * time.Millisecond,
		// ForceFinalizeStuck intentionally NOT set.
	})
	if err == nil {
		t.Fatal("expected error from stuck namespace without --force-finalize-stuck")
	}
	if len(k.finalized) != 0 {
		t.Fatalf("force-finalize should NOT have fired: %v", k.finalized)
	}
}

// Closes H2 — see reviews/2026-05-15T195833Z-review.md#h2.
// LevelFull WITH --force-finalize-stuck opts back into the old behavior.
func TestCleanup_LevelFull_ForceFinalizeOptIn(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing: map[string]bool{"mural-system": true},
		timeouts: map[string]bool{"mural-system": true},
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
		NamespaceWait:      10 * time.Millisecond,
		ForceFinalizeStuck: true,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(k.finalized) != 1 || k.finalized[0] != "mural-system" {
		t.Fatalf("force-finalize not invoked: %v", k.finalized)
	}
}

// Closes H2 — see reviews/2026-05-15T195833Z-review.md#h2.
// LevelReset implicitly enables force-finalize (no flag required).
func TestCleanup_LevelReset_ForceFinalizesImplicitly(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing: map[string]bool{"mural-system": true},
		timeouts: map[string]bool{"mural-system": true},
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelReset,
		NamespaceWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(k.finalized) != 1 {
		t.Fatalf("reset should force-finalize implicitly: %v", k.finalized)
	}
}

func TestCleanup_LevelReset_SkipsHelm(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{existing: map[string]bool{"mural-system": true}}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelReset,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("LevelReset should skip helm, got %v", h.calls)
	}
}

func TestCleanup_KeepFlagsHonored(t *testing.T) {
	h := &fakeUninstaller{}
	c := newCleaner(h, &fakeKube{})
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
		KeepCertManager: true, KeepQueue: true,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, c := range h.calls {
		if c.ReleaseName == "cert-manager" || c.ReleaseName == "queue" {
			t.Fatalf("kept release was still uninstalled: %s", c.ReleaseName)
		}
	}
}

func TestCleanup_EmptyClusterName(t *testing.T) {
	if err := cleaner.New(&fakeUninstaller{}, nil).Cleanup(context.Background(),
		cleaner.Request{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_InvalidLevel(t *testing.T) {
	c := newCleaner(&fakeUninstaller{}, &fakeKube{})
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.Level("nope"),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_ConfigLoaderError(t *testing.T) {
	c := cleaner.New(&fakeUninstaller{}, nil)
	c.ConfigLoader = func(string) (*config.ClusterConfig, error) { return nil, errors.New("boom") }
	err := c.Cleanup(context.Background(), cleaner.Request{ClusterName: "local"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_NilConfigLoader(t *testing.T) {
	c := cleaner.New(&fakeUninstaller{}, nil)
	c.ConfigLoader = nil
	if err := c.Cleanup(context.Background(), cleaner.Request{ClusterName: "local"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_ClusterNotFound(t *testing.T) {
	c := newCleaner(&fakeUninstaller{}, &fakeKube{})
	if err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "missing", Level: cleaner.LevelUninstall,
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_NilHelm(t *testing.T) {
	c := cleaner.New(nil, nil)
	c.ConfigLoader = okLoader
	if err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_NilKubeBuilder(t *testing.T) {
	c := cleaner.New(&fakeUninstaller{}, nil)
	c.ConfigLoader = okLoader
	c.Kube = nil
	if err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_KubeBuilderError(t *testing.T) {
	c := cleaner.New(&fakeUninstaller{}, nil)
	c.ConfigLoader = okLoader
	c.Kube = func(_, _ string) (kube.Client, error) { return nil, errors.New("kube boom") }
	if err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanup_LogIsHonored(t *testing.T) {
	var seen []string
	c := cleaner.New(&fakeUninstaller{}, func(format string, _ ...any) {
		seen = append(seen, format)
	})
	c.ConfigLoader = okLoader
	c.Kube = func(_, _ string) (kube.Client, error) { return &fakeKube{}, nil }
	_ = c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
	})
	if len(seen) == 0 {
		t.Fatal("expected Log to be invoked")
	}
}

func TestAllLevels_Stable(t *testing.T) {
	levels := cleaner.AllLevels()
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %v", levels)
	}
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// Label-discovered namespaces are unioned with the static ancillary list.
func TestCleanup_LevelFull_LabelDiscoveredNamespacesAreSwept(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing: map[string]bool{
			"mural-system": true, "discovered-by-label": true,
		},
		labeledNamespaces: []string{"discovered-by-label"},
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
		NamespaceWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// The label-discovered namespace must have been deleted (in addition to
	// the static ones).
	var sawDiscovered bool
	k.mu.Lock()
	for _, ns := range k.deletes {
		if ns == "discovered-by-label" {
			sawDiscovered = true
		}
	}
	k.mu.Unlock()
	if !sawDiscovered {
		t.Fatalf("label-discovered namespace not deleted: %v", k.deletes)
	}
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// Label-discovered webhooks merge with the static stale list.
func TestCleanup_LevelFull_LabelDiscoveredWebhooksAreSwept(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		labeledValidating: []string{"extra-validator"},
		labeledMutating:   []string{"extra-mutator"},
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	hasName := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if !hasName(k.delValidating, "extra-validator") {
		t.Fatalf("extra-validator not deleted: %v", k.delValidating)
	}
	if !hasName(k.delMutating, "extra-mutator") {
		t.Fatalf("extra-mutator not deleted: %v", k.delMutating)
	}
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// A failing label sweep must NOT abort teardown — the cleaner falls back to
// the static hardcoded lists.
func TestCleanup_LevelFull_LabelSweepErrorFallsBackToStaticLists(t *testing.T) {
	h := &fakeUninstaller{}
	k := &fakeKube{
		existing:     map[string]bool{"mural-system": true},
		labelListErr: errors.New("label query boom"),
	}
	c := newCleaner(h, k)
	err := c.Cleanup(context.Background(), cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelFull,
		NamespaceWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("label sweep failure should be non-fatal, got %v", err)
	}
	// Static webhook list must still have been swept despite the label error.
	if len(k.delValidating) == 0 || len(k.delMutating) == 0 {
		t.Fatalf("static webhook lists not swept on label-error fallback: v=%v m=%v",
			k.delValidating, k.delMutating)
	}
}

// Closes C2 — see reviews/2026-05-15T195833Z-review.md#c2.
// A cancelled context must abort before the next helm uninstall fires.
func TestCleanup_CancelledContextStopsBetweenReleases(t *testing.T) {
	h := &fakeUninstaller{}
	c := newCleaner(h, &fakeKube{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Cleanup(ctx, cleaner.Request{
		ClusterName: "local", Level: cleaner.LevelUninstall,
	})
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if len(h.calls) != 0 {
		t.Fatalf("no helm calls should have fired before ctx-check, got %v", h.calls)
	}
}

// helpers --------------------------------------------------------------------

func releaseNames(calls []helm.UninstallOptions) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.ReleaseName)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
