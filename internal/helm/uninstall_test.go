package helm

import (
	"context"
	"errors"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	helmcli "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
)

func TestUninstallOptions_Validate(t *testing.T) {
	good := UninstallOptions{ReleaseName: "mural", Namespace: "mural-system", Kubeconfig: "/tmp/kc"}
	if err := good.Validate(); err != nil {
		t.Fatalf("happy validate: %v", err)
	}

	tt := []struct {
		name   string
		mutate func(*UninstallOptions)
	}{
		{"no release", func(o *UninstallOptions) { o.ReleaseName = "" }},
		{"no namespace", func(o *UninstallOptions) { o.Namespace = "" }},
		{"no kubeconfig", func(o *UninstallOptions) { o.Kubeconfig = "" }},
	}
	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.mutate(&o)
			if err := o.Validate(); err == nil || !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("expected ErrInvalidOptions, got %v", err)
			}
		})
	}
}

func TestUninstall_HappyPath(t *testing.T) {
	cfg, env, _ := inMemoryConfig()
	// Plant a release in the in-memory store so releaseExists returns true.
	mustStore(t, cfg, &release.Release{
		Name:      "mural",
		Namespace: "mural-system",
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart:     &chart.Chart{Metadata: &chart.Metadata{Name: "mural", Version: "1.0"}},
	})

	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return cfg, env, nil
		},
	}
	err := inst.Uninstall(context.Background(), UninstallOptions{
		ReleaseName: "mural",
		Namespace:   "mural-system",
		Kubeconfig:  "/dev/null",
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

func TestUninstall_ReleaseNotFound(t *testing.T) {
	cfg, env, _ := inMemoryConfig()
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return cfg, env, nil
		},
	}
	err := inst.Uninstall(context.Background(), UninstallOptions{
		ReleaseName: "missing",
		Namespace:   "mural-system",
		Kubeconfig:  "/dev/null",
	})
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("want ErrReleaseNotFound, got %v", err)
	}
}

func TestUninstall_ValidateError(t *testing.T) {
	err := (&SDKInstaller{}).Uninstall(context.Background(), UninstallOptions{})
	if err == nil || !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions, got %v", err)
	}
}

func TestUninstall_FactoryError(t *testing.T) {
	want := errors.New("boom")
	inst := &SDKInstaller{
		factory: func(InstallOptions, LogFunc) (*action.Configuration, *helmcli.EnvSettings, error) {
			return nil, nil, want
		},
	}
	err := inst.Uninstall(context.Background(), UninstallOptions{
		ReleaseName: "x", Namespace: "ns", Kubeconfig: "/dev/null",
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wraps %v", err, want)
	}
}

func TestUninstall_NilFactoryFallsBackToDefault(t *testing.T) {
	// Same as the Apply equivalent: the nil-factory branch should hit the
	// production factory, which errors out on a bogus kubeconfig path.
	inst := &SDKInstaller{factory: nil}
	err := inst.Uninstall(context.Background(), UninstallOptions{
		ReleaseName: "x", Namespace: "ns",
		// /dev/null/missing is intentionally invalid so the chain reaches
		// the SDK and errors before any cluster call.
		Kubeconfig: "/dev/null/missing",
	})
	if err == nil {
		t.Fatal("expected error from production factory against invalid kubeconfig")
	}
}

func mustStore(t *testing.T, cfg *action.Configuration, rel *release.Release) {
	t.Helper()
	if err := cfg.Releases.Create(rel); err != nil {
		t.Fatalf("seed release: %v", err)
	}
}
