package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"palette-ai-instance/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster_config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_HappyResolvesRelativeKubeconfig(t *testing.T) {
	path := writeConfig(t, `
clusters:
  - name: local
    kubeconfig: kc.yaml
    context: local
    namespace: mural-system
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(filepath.Dir(path), "kc.yaml")
	if cfg.Clusters[0].Kubeconfig != want {
		t.Fatalf("kubeconfig not resolved: got %q want %q", cfg.Clusters[0].Kubeconfig, want)
	}
}

func TestLoad_AbsoluteKubeconfigPreserved(t *testing.T) {
	abs := "/tmp/kc.yaml"
	path := writeConfig(t, `
clusters:
  - name: local
    kubeconfig: `+abs+`
    namespace: mural-system
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Clusters[0].Kubeconfig != abs {
		t.Fatalf("absolute kubeconfig was rewritten: %q", cfg.Clusters[0].Kubeconfig)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected error on missing file")
	}
}

func TestLoad_BadYAML(t *testing.T) {
	path := writeConfig(t, "::: not yaml :::")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected YAML parse error")
	}
}

func TestLoad_Validation(t *testing.T) {
	t.Parallel()
	tt := []struct {
		name string
		body string
	}{
		{
			name: "no clusters",
			body: `clusters: []`,
		},
		{
			name: "missing name",
			body: `
clusters:
  - kubeconfig: /tmp/kc
    namespace: ns
`,
		},
		{
			name: "missing kubeconfig",
			body: `
clusters:
  - name: x
    namespace: ns
`,
		},
		{
			name: "missing namespace",
			body: `
clusters:
  - name: x
    kubeconfig: /tmp/kc
`,
		},
		{
			name: "duplicate name",
			body: `
clusters:
  - name: x
    kubeconfig: /tmp/kc
    namespace: ns
  - name: x
    kubeconfig: /tmp/kc2
    namespace: ns
`,
		},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, tc.body)
			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %q", tc.name)
			}
		})
	}
}

func TestClusterConfig_GetAndNames(t *testing.T) {
	path := writeConfig(t, `
clusters:
  - name: a
    kubeconfig: /tmp/a
    namespace: ns-a
  - name: b
    kubeconfig: /tmp/b
    namespace: ns-b
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, err := cfg.Get("b")
	if err != nil {
		t.Fatalf("Get(b): %v", err)
	}
	if got.Namespace != "ns-b" {
		t.Fatalf("Get returned wrong cluster: %+v", got)
	}

	if _, err := cfg.Get("missing"); !errors.Is(err, config.ErrClusterNotFound) {
		t.Fatalf("expected ErrClusterNotFound, got %v", err)
	}

	names := cfg.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("Names() = %v", names)
	}
}
