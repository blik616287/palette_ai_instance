package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"palette-ai-instance/internal/cleaner"
	"palette-ai-instance/internal/config"
)

// writeClusterConfig drops a minimal cluster_config.yaml with a single
// "local" cluster + optional per-cluster cleanup/deploy defaults. The
// returned path goes straight into --config.
func writeClusterConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster_config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cluster config: %v", err)
	}
	return path
}

const minimalCluster = `
clusters:
  - name: local
    kubeconfig: /tmp/kc
    namespace: mural-system
`

func TestBuildCleanupRequest_NormalizesAndPropagatesAllFlags(t *testing.T) {
	f := &cleanupFlags{
		configPath:      "x",
		level:           "  RESET  ",
		keepCertManager: true,
		keepFlux:        true,
		keepQueue:       false,
		namespaceWait:   45 * time.Second,
	}
	cluster := config.Cluster{Name: "local"}
	req := buildCleanupRequest(f, &cluster)
	if req.Level != cleaner.LevelReset {
		t.Fatalf("level not normalized: %q", req.Level)
	}
	if !req.KeepCertManager || !req.KeepFlux || req.KeepQueue {
		t.Fatalf("keep-* flags not propagated: %+v", req)
	}
	if req.NamespaceWait != 45*time.Second {
		t.Fatalf("namespace-wait not propagated: %v", req.NamespaceWait)
	}
	if req.ClusterName != "local" {
		t.Fatalf("cluster name not propagated: %q", req.ClusterName)
	}
}

func TestCleanupCmd_RequiresConfig(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := newCleanupCmd(out, errOut, func(context.Context, cleaner.Request, io.Writer) error {
		t.Fatal("runner should not be invoked")
		return nil
	})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error from missing --config")
	}
}

func TestCleanupCmd_RunnerInvokedWithParsedRequest(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	var got cleaner.Request
	cmd := newCleanupCmd(out, errOut, func(_ context.Context, req cleaner.Request, _ io.Writer) error {
		got = req
		return nil
	})
	cmd.SetArgs([]string{
		"--config", cfgPath,
		"--cluster-name", "local",
		"--level", "reset",
		"--keep-cert-manager",
		"--keep-flux",
		"--keep-queue",
		"--namespace-wait", "30s",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ClusterName != "local" || got.Level != cleaner.LevelReset ||
		!got.KeepCertManager || !got.KeepFlux || !got.KeepQueue ||
		got.NamespaceWait != 30*time.Second {
		t.Fatalf("request not parsed correctly: %+v", got)
	}
}

func TestCleanupCmd_OmittingClusterNameIteratesAllClusters(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, `
clusters:
  - name: a
    kubeconfig: /tmp/a
    namespace: ns-a
  - name: b
    kubeconfig: /tmp/b
    namespace: ns-b
`)
	var seen []string
	cmd := newCleanupCmd(out, errOut, func(_ context.Context, req cleaner.Request, _ io.Writer) error {
		seen = append(seen, req.ClusterName)
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--level", "uninstall"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("expected both clusters in order, got %v", seen)
	}
}

func TestCleanupCmd_CleanupBlockInConfigIsIgnored(t *testing.T) {
	// If a cleanup: block sneaks into cluster_config.yaml, it must NOT
	// influence the run — destructive intent is CLI-only. The YAML parser
	// silently drops the unknown key (Go yaml.v3 doesn't error by default),
	// and the resulting request reflects only CLI / cobra defaults.
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, `
clusters:
  - name: local
    kubeconfig: /tmp/kc
    namespace: mural-system
    cleanup:
      level: reset
      keep-cert-manager: true
`)
	var got cleaner.Request
	cmd := newCleanupCmd(out, errOut, func(_ context.Context, req cleaner.Request, _ io.Writer) error {
		got = req
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--cluster-name", "local"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Level != cleaner.LevelFull {
		t.Fatalf("cleanup block in config must NOT override cobra default: %q", got.Level)
	}
	if got.KeepCertManager {
		t.Fatalf("cleanup block in config must NOT set keep-cert-manager: %+v", got)
	}
}

func TestCleanupCmd_RunnerErrorBubbles(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	want := errors.New("runner boom")
	cmd := newCleanupCmd(out, errOut, func(context.Context, cleaner.Request, io.Writer) error {
		return want
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--cluster-name", "local"})
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestCleanupCmd_MissingClusterNameInConfig(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	cmd := newCleanupCmd(out, errOut, func(context.Context, cleaner.Request, io.Writer) error {
		t.Fatal("runner should not run")
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--cluster-name", "missing"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cluster-not-found error")
	}
}

func TestDefaultCleanupRunner_PropagatesLoadError(t *testing.T) {
	err := defaultCleanupRunner(context.Background(), cleaner.Request{
		ConfigPath: "/does/not/exist.yaml", ClusterName: "local", Level: cleaner.LevelUninstall,
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error from missing config")
	}
}

func TestNewCleanupCmd_HasAllFlags(t *testing.T) {
	cmd := NewCleanupCmd(io.Discard, io.Discard)
	for _, name := range []string{
		"config", "cluster-name", "level",
		"keep-cert-manager", "keep-flux", "keep-queue", "namespace-wait",
	} {
		if cmd.Flag(name) == nil {
			t.Fatalf("flag %q not registered", name)
		}
	}
}

func TestRootCmd_HasCleanup(t *testing.T) {
	root := NewRootCmd(io.Discard, io.Discard)
	var found bool
	for _, c := range root.Commands() {
		if c.Use == "cleanup" {
			found = true
		}
	}
	if !found {
		t.Fatal("cleanup subcommand not registered on root")
	}
}
