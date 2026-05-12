package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/deployer"
)

// Helper changedFn that emulates cmd.Flags().Changed for any subset.
func changedSet(names ...string) flagSetCheck {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return func(name string) bool { _, ok := m[name]; return ok }
}

func TestMergeDeployRequest_CLIWinsWhenChanged(t *testing.T) {
	f := &deployFlags{
		version:      "1.0.7",
		chartURI:     "oci://cli/mural",
		validate:     true,
		validateWait: 90 * time.Second,
		helmTimeout:  10 * time.Minute,
	}
	cluster := config.Cluster{
		Name: "local",
		Deploy: config.DeployDefaults{
			Version:      "0.9",
			ChartURI:     "oci://config/mural",
			Validate:     false,
			ValidateWait: 30 * time.Second,
			HelmTimeout:  5 * time.Minute,
		},
	}
	req := mergeDeployRequest(
		changedSet("version", "chart-uri", "validate", "validate-wait", "helm-timeout"),
		f, &cluster,
	)
	if req.MuralVersion != "1.0.7" || req.ChartURI != "oci://cli/mural" {
		t.Fatalf("CLI strings did not win: %+v", req)
	}
	if !req.Validate || req.ValidateWait != 90*time.Second || req.HelmTimeout != 10*time.Minute {
		t.Fatalf("CLI bool/duration did not win: %+v", req)
	}
}

func TestMergeDeployRequest_SetValuesCLIOverridesConfig(t *testing.T) {
	f := &deployFlags{setValues: []string{"global.dns.domain=cli.example"}}
	cluster := config.Cluster{
		Deploy: config.DeployDefaults{
			SetValues: []string{"global.dns.domain=cfg.example"},
		},
	}
	// CLI did set --set → its slice wins, config slice is ignored.
	cli := mergeDeployRequest(changedSet("set"), f, &cluster)
	if len(cli.SetValues) != 1 || cli.SetValues[0] != "global.dns.domain=cli.example" {
		t.Fatalf("CLI --set did not override config: %v", cli.SetValues)
	}
	// CLI did NOT set --set → config slice passes through.
	cfg := mergeDeployRequest(nil, f, &cluster)
	if len(cfg.SetValues) != 1 || cfg.SetValues[0] != "global.dns.domain=cfg.example" {
		t.Fatalf("config --set did not pass through: %v", cfg.SetValues)
	}
}

func TestMergeDeployRequest_ConfigFillsWhenFlagsUnset(t *testing.T) {
	f := &deployFlags{} // nothing set on CLI
	cluster := config.Cluster{
		Name: "local",
		Deploy: config.DeployDefaults{
			Version:      "0.9",
			ChartURI:     "oci://config/mural",
			Validate:     true,
			ValidateWait: 2 * time.Minute,
		},
	}
	req := mergeDeployRequest(nil, f, &cluster)
	if req.MuralVersion != "0.9" || req.ChartURI != "oci://config/mural" {
		t.Fatalf("config strings did not pass through: %+v", req)
	}
	if !req.Validate || req.ValidateWait != 2*time.Minute {
		t.Fatalf("config bool/duration did not pass through: %+v", req)
	}
	if req.ClusterName != "local" {
		t.Fatalf("cluster name not propagated: %q", req.ClusterName)
	}
}

func TestDeployCmd_RequiresConfig(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := newDeployCmd(out, errOut, func(context.Context, deployer.Request, io.Writer) error {
		t.Fatal("runner should not be invoked")
		return nil
	})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected --config required error")
	}
}

func TestDeployCmd_RunnerInvokedWithParsedRequest(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	var got deployer.Request
	cmd := newDeployCmd(out, errOut, func(_ context.Context, req deployer.Request, _ io.Writer) error {
		got = req
		return nil
	})
	cmd.SetArgs([]string{
		"--config", cfgPath,
		"--cluster-name", "local",
		"--chart-uri", "oci://example/mural",
		"--version", "1.0.7",
		"--values-file", "values.yaml",
		"--crds-chart-uri", "oci://example/mural-crds",
		"--cert-manager-chart-uri", "oci://example/cert-manager",
		"--cert-manager-values-file", "cm.yaml",
		"--flux-chart-uri", "oci://example/flux2",
		"--flux-version", "2.13.0",
		"--flux-values-file", "flux.yaml",
		"--queue-chart-uri", "oci://example/rabbitmq",
		"--queue-version", "14.6.6",
		"--queue-values-file", "queue.yaml",
		"--cert-manager-version", "1.19.3",
		"--crds-version", "0.7.1",
		"--validate",
		"--validate-wait", "90s",
		"--helm-timeout", "1m",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ClusterName != "local" || got.ChartURI != "oci://example/mural" {
		t.Fatalf("runner did not get expected request: %+v", got)
	}
	if got.CertManagerChartURI != "oci://example/cert-manager" || got.CertManagerValuesFile != "cm.yaml" {
		t.Fatalf("cert-manager flags not parsed: %+v", got)
	}
	if !got.Validate || got.ValidateWait != 90*time.Second {
		t.Fatalf("validate flags not parsed: %+v", got)
	}
}

func TestDeployCmd_OmittingClusterNameIteratesAllClusters(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, `
clusters:
  - name: a
    kubeconfig: /tmp/a
    namespace: ns-a
    deploy:
      chart-uri: oci://config/a/mural
  - name: b
    kubeconfig: /tmp/b
    namespace: ns-b
    deploy:
      chart-uri: oci://config/b/mural
`)
	var seen []string
	cmd := newDeployCmd(out, errOut, func(_ context.Context, req deployer.Request, _ io.Writer) error {
		seen = append(seen, req.ClusterName+":"+req.ChartURI)
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 2 ||
		seen[0] != "a:oci://config/a/mural" ||
		seen[1] != "b:oci://config/b/mural" {
		t.Fatalf("multi-cluster iteration wrong: %v", seen)
	}
}

func TestDeployCmd_PerClusterConfigDefaultsFillUnsetFlags(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, `
clusters:
  - name: local
    kubeconfig: /tmp/kc
    namespace: mural-system
    deploy:
      chart-uri: oci://config/mural
      version: 1.2.3
      validate: true
      validate-wait: 7m
`)
	var got deployer.Request
	cmd := newDeployCmd(out, errOut, func(_ context.Context, req deployer.Request, _ io.Writer) error {
		got = req
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--cluster-name", "local"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ChartURI != "oci://config/mural" || got.MuralVersion != "1.2.3" {
		t.Fatalf("config strings not used: %+v", got)
	}
	if !got.Validate || got.ValidateWait != 7*time.Minute {
		t.Fatalf("config bool/duration not used: %+v", got)
	}
}

func TestDeployCmd_CLIFlagOverridesConfigDefault(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, `
clusters:
  - name: local
    kubeconfig: /tmp/kc
    namespace: mural-system
    deploy:
      chart-uri: oci://config/mural
      validate: true
`)
	var got deployer.Request
	cmd := newDeployCmd(out, errOut, func(_ context.Context, req deployer.Request, _ io.Writer) error {
		got = req
		return nil
	})
	cmd.SetArgs([]string{
		"--config", cfgPath,
		"--cluster-name", "local",
		"--chart-uri", "oci://cli/mural", // override
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ChartURI != "oci://cli/mural" {
		t.Fatalf("CLI did not override config: %q", got.ChartURI)
	}
	if !got.Validate {
		t.Fatalf("unset CLI bool should fall through to config: %+v", got)
	}
}

func TestDeployCmd_RunnerErrorBubbles(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	want := errors.New("runner failed")
	cmd := newDeployCmd(out, errOut, func(context.Context, deployer.Request, io.Writer) error {
		return want
	})
	cmd.SetArgs([]string{
		"--config", cfgPath,
		"--cluster-name", "local",
		"--chart-uri", "oci://example/mural",
	})
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestDeployCmd_MissingClusterNameInConfig(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cfgPath := writeClusterConfig(t, minimalCluster)
	cmd := newDeployCmd(out, errOut, func(context.Context, deployer.Request, io.Writer) error {
		t.Fatal("runner should not be invoked")
		return nil
	})
	cmd.SetArgs([]string{"--config", cfgPath, "--cluster-name", "missing"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cluster-not-found error")
	}
}

func TestDefaultDeployRunner_PropagatesLoadError(t *testing.T) {
	// The production runner uses the real deployer + config loader; pointing
	// at a missing config file lets us drive it to an error without needing
	// a cluster.
	err := defaultDeployRunner(context.Background(), deployer.Request{
		ConfigPath:  "/does/not/exist.yaml",
		ClusterName: "local",
		ChartURI:    "oci://example/mural",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error from missing cluster config")
	}
}

func TestNewDeployCmd_UsesProductionRunner(t *testing.T) {
	cmd := NewDeployCmd(io.Discard, io.Discard)
	if cmd.Use != "deploy" {
		t.Fatalf("unexpected Use: %q", cmd.Use)
	}
	for _, name := range []string{
		"cluster-name", "version", "values-file", "chart-uri", "crds-chart-uri",
		"cert-manager-chart-uri", "cert-manager-version", "cert-manager-values-file",
		"cert-manager-no-create-ns",
		"crds-version",
		"flux-chart-uri", "flux-version", "flux-values-file",
		"queue-chart-uri", "queue-version", "queue-values-file",
		"helm-timeout", "config", "validate", "validate-wait",
	} {
		if cmd.Flag(name) == nil {
			t.Fatalf("flag %q not registered", name)
		}
	}
}
