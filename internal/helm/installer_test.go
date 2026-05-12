package helm

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	helmcli "helm.sh/helm/v3/pkg/cli"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

const testChartPath = "testdata/test-chart"

// inMemoryConfig builds an action.Configuration backed by an in-memory release
// store and helm's printing kube fake — no real cluster required.
func inMemoryConfig() (*action.Configuration, *helmcli.EnvSettings, error) {
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...any) {},
	}
	return cfg, helmcli.New(), nil
}

func TestApply_InstallThenUpgrade(t *testing.T) {
	captured := []string{}
	// Persist a single in-memory config across both Apply calls so the
	// release written by the install is visible to the upgrade.
	cfg, env, _ := inMemoryConfig()
	inst := &SDKInstaller{
		Log: func(format string, args ...any) {
			captured = append(captured, format)
		},
		factory: func(_ InstallOptions, _ LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return cfg, env, nil
		},
	}
	opts := InstallOptions{
		ReleaseName: "demo",
		Namespace:   "demo-ns",
		ChartURI:    testChartPath,
		Kubeconfig:  "/dev/null",
		CreateNS:    true,
	}

	rel, err := inst.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if rel == nil || rel.Name != "demo" || rel.Revision != 1 {
		t.Fatalf("first apply result wrong: %+v", rel)
	}

	rel2, err := inst.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if rel2.Revision != 2 {
		t.Fatalf("expected revision=2 on upgrade, got %d", rel2.Revision)
	}

	if len(captured) == 0 {
		t.Fatalf("expected Log to be invoked")
	}
}

func TestApply_ValidateError(t *testing.T) {
	inst := NewSDKInstaller(nil)
	_, err := inst.Apply(context.Background(), InstallOptions{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("error is not ErrInvalidOptions: %v", err)
	}
}

func TestApply_FactoryError(t *testing.T) {
	wantErr := errors.New("boom")
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return nil, nil, wantErr
		},
	}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "x", Namespace: "ns", ChartURI: "./does-not-matter", Kubeconfig: "/tmp/x",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error %v should wrap %v", err, wantErr)
	}
}

func TestApply_LoadChartError(t *testing.T) {
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return inMemoryConfig()
		},
	}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "x", Namespace: "ns",
		ChartURI:   "testdata/does-not-exist",
		Kubeconfig: "/dev/null",
	})
	if err == nil {
		t.Fatal("expected locateAndLoad error")
	}
}

func TestApply_LoadValuesError(t *testing.T) {
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return inMemoryConfig()
		},
	}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "x", Namespace: "ns",
		ChartURI:   testChartPath,
		ValuesFile: "testdata/does-not-exist.yaml",
		Kubeconfig: "/dev/null",
	})
	if err == nil {
		t.Fatal("expected loadValues error")
	}
}

func TestApply_NilFactoryFallsBackToDefault(t *testing.T) {
	// With the production factory and a bogus kubeconfig, Apply will fail
	// at config init — but it proves the nil-factory path is reached.
	inst := &SDKInstaller{factory: nil}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "x", Namespace: "ns",
		ChartURI:   testChartPath,
		Kubeconfig: "/dev/null/not-a-real-file",
	})
	if err == nil {
		t.Fatal("expected error from production factory against invalid kubeconfig")
	}
}

func TestApply_LogNilIsSilent(t *testing.T) {
	inst := &SDKInstaller{
		Log: nil,
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return inMemoryConfig()
		},
	}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "demo", Namespace: "demo-ns",
		ChartURI: testChartPath, Kubeconfig: "/dev/null", CreateNS: true,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestApply_WithValuesFileAndContext(t *testing.T) {
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return inMemoryConfig()
		},
	}
	_, err := inst.Apply(context.Background(), InstallOptions{
		ReleaseName: "v", Namespace: "v-ns",
		ChartURI:   testChartPath,
		ValuesFile: "testdata/values-override.yaml",
		Kubeconfig: "/dev/null",
		Context:    "some-context",
		CreateNS:   true,
		Wait:       true,
		Timeout:    1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestDefaultConfigFactory_BuildsLazilyWithoutError(t *testing.T) {
	// action.Configuration.Init is lazy — it never reads the kubeconfig
	// during construction. So we only assert the factory wires fields
	// correctly without panicking and surfaces a log line.
	var logged []string
	log := LogFunc(func(format string, _ ...any) { logged = append(logged, format) })

	cfg, env, err := defaultConfigFactory(InstallOptions{
		Namespace:  "ns",
		Kubeconfig: "/dev/null",
		Context:    "some-context",
	}, log)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if cfg == nil || env == nil {
		t.Fatal("expected non-nil cfg/env")
	}
	if env.KubeContext != "some-context" {
		t.Fatalf("kube context not propagated: %q", env.KubeContext)
	}
}

func TestDefaultConfigFactory_NilLogDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil log: %v", r)
		}
	}()
	cfg, env, err := defaultConfigFactory(InstallOptions{
		Namespace:  "ns",
		Kubeconfig: "/dev/null",
	}, nil)
	if err != nil || cfg == nil || env == nil {
		t.Fatalf("factory: err=%v cfg=%v env=%v", err, cfg, env)
	}
}

func TestDefaultConfigFactory_HelmLogForwards(t *testing.T) {
	// Exercises the inner helmLog closure inside defaultConfigFactory. We
	// snake a log line through helm's Configuration.Log to confirm the
	// "helm: " prefix is applied and the user-supplied log fires.
	var seen []string
	log := LogFunc(func(format string, _ ...any) { seen = append(seen, format) })

	cfg, _, err := defaultConfigFactory(InstallOptions{
		Namespace:  "ns",
		Kubeconfig: "/dev/null",
	}, log)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	cfg.Log("hello %s", "world")
	if len(seen) == 0 || seen[0] != "helm: hello %s" {
		t.Fatalf("helm log not forwarded with prefix: %v", seen)
	}
}

func TestLoadValues_EmptyPathAndNoSetsReturnsEmpty(t *testing.T) {
	v, err := loadValues("", nil, helmcli.New())
	if err != nil {
		t.Fatalf("loadValues: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("expected empty map, got %v", v)
	}
}

func TestLoadValues_MissingFile(t *testing.T) {
	_, err := loadValues("testdata/does-not-exist.yaml", nil, helmcli.New())
	if err == nil {
		t.Fatal("expected error on missing values file")
	}
}

func TestLoadValues_Valid(t *testing.T) {
	v, err := loadValues("testdata/values-override.yaml", nil, helmcli.New())
	if err != nil {
		t.Fatalf("loadValues: %v", err)
	}
	if v["greeting"] != "hola" {
		t.Fatalf("unexpected values: %v", v)
	}
}

func TestLoadValues_SetOverridesFile(t *testing.T) {
	v, err := loadValues("testdata/values-override.yaml",
		[]string{"greeting=bonjour", "nested.key=42"}, helmcli.New())
	if err != nil {
		t.Fatalf("loadValues: %v", err)
	}
	if v["greeting"] != "bonjour" {
		t.Fatalf("--set should override file: got %v", v["greeting"])
	}
	nested, ok := v["nested"].(map[string]any)
	if !ok || nested["key"] != int64(42) {
		t.Fatalf("dot-path --set not parsed: %v", v["nested"])
	}
}

func TestLoadValues_SetOnlyWithoutFile(t *testing.T) {
	v, err := loadValues("", []string{"only=set"}, helmcli.New())
	if err != nil {
		t.Fatalf("loadValues: %v", err)
	}
	if v["only"] != "set" {
		t.Fatalf("--set only: %v", v)
	}
}

func TestToRelease(t *testing.T) {
	if toRelease(nil) != nil {
		t.Fatal("nil in should be nil out")
	}

	out := toRelease(&release.Release{Name: "x", Namespace: "ns", Version: 7})
	if out.Name != "x" || out.Namespace != "ns" || out.Revision != 7 {
		t.Fatalf("toRelease wrong: %+v", out)
	}
	if out.Status != "" {
		t.Fatalf("expected empty status when Info is nil, got %q", out.Status)
	}

	withInfo := toRelease(&release.Release{
		Name: "y", Version: 1,
		Info: &release.Info{Status: release.StatusDeployed},
	})
	if withInfo.Status != "deployed" {
		t.Fatalf("status wrong: %q", withInfo.Status)
	}
}

func TestStrPtr(t *testing.T) {
	p := strPtr("hello")
	if p == nil || *p != "hello" {
		t.Fatalf("strPtr broken")
	}
}

func TestNewSDKInstaller(t *testing.T) {
	inst := NewSDKInstaller(nil)
	if inst == nil {
		t.Fatal("NewSDKInstaller returned nil")
	}
	if inst.factory == nil {
		t.Fatal("factory not set")
	}
}

func TestSDKInstaller_LogNilIsSilent(t *testing.T) {
	inst := &SDKInstaller{}
	inst.logf("ignored %s", "msg")
}

// Sanity check: our test chart is a valid Chart object.
func TestTestChartLoads(t *testing.T) {
	ch, _ := locateAndLoadForTest(t)
	if ch == nil || ch.Name() != "test-chart" {
		t.Fatalf("unexpected chart: %+v", ch)
	}
}

func locateAndLoadForTest(t *testing.T) (*chart.Chart, *helmcli.EnvSettings) {
	t.Helper()
	cfg, env, _ := inMemoryConfig()
	ins := action.NewInstall(cfg)
	ch, err := locateAndLoad(&ins.ChartPathOptions, env, testChartPath)
	if err != nil {
		t.Fatalf("locateAndLoad: %v", err)
	}
	return ch, env
}

func TestLocateAndLoad_NotFound(t *testing.T) {
	cfg, env, _ := inMemoryConfig()
	ins := action.NewInstall(cfg)
	_, err := locateAndLoad(&ins.ChartPathOptions, env, "testdata/missing")
	if err == nil {
		t.Fatal("expected error from locateAndLoad on missing path")
	}
}
