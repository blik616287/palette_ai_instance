package deployer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"palette-ai-instance/internal/config"
	"palette-ai-instance/internal/deployer"
	"palette-ai-instance/internal/helm"
	"palette-ai-instance/internal/kube"
)

// fakeKube implements just enough of kube.Client for the validate tests.
type fakeKube struct {
	summary    kube.PodSummary
	waitErr    error
	host       string
	hostErr    error
	port       int32
	portErr    error
	calledWait bool
}

func (f *fakeKube) NamespaceExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeKube) DeleteNamespace(context.Context, string) error         { return nil }
func (f *fakeKube) WaitForNamespaceGone(context.Context, string, time.Duration) error {
	return nil
}
func (f *fakeKube) ForceFinalizeNamespace(context.Context, string) error               { return nil }
func (f *fakeKube) DeleteValidatingWebhookConfiguration(context.Context, string) error { return nil }
func (f *fakeKube) DeleteMutatingWebhookConfiguration(context.Context, string) error   { return nil }
func (f *fakeKube) WaitForPodsReady(_ context.Context, _ string, _ time.Duration) (kube.PodSummary, error) {
	f.calledWait = true
	return f.summary, f.waitErr
}
func (f *fakeKube) GetIngressHost(context.Context, string, string) (string, error) {
	return f.host, f.hostErr
}
func (f *fakeKube) GetServiceNodePort(context.Context, string, string, string) (int32, error) {
	return f.port, f.portErr
}

// L3 — list-by-label methods, no-op in deployer tests.
func (f *fakeKube) ListNamespacesByLabel(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeKube) ListValidatingWebhooksByLabel(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeKube) ListMutatingWebhooksByLabel(context.Context, string) ([]string, error) {
	return nil, nil
}

// ---- fakes ----------------------------------------------------------------

type fakeInstaller struct {
	calls   []helm.InstallOptions
	err     error
	errOnce string // when set, returns err only for the matching ReleaseName
}

func (f *fakeInstaller) Apply(_ context.Context, opts helm.InstallOptions) (*helm.Release, error) {
	f.calls = append(f.calls, opts)
	if f.err != nil && (f.errOnce == "" || f.errOnce == opts.ReleaseName) {
		return nil, f.err
	}
	return &helm.Release{Name: opts.ReleaseName, Namespace: opts.Namespace, Revision: 1, Status: "deployed"}, nil
}

func okCluster() *config.ClusterConfig {
	return &config.ClusterConfig{Clusters: []config.Cluster{{
		Name: "local", Kubeconfig: "/tmp/kc", Context: "local", Namespace: "mural-system",
	}}}
}

func okLoader(_ string) (*config.ClusterConfig, error) { return okCluster(), nil }

// ---- tests ----------------------------------------------------------------

func TestDeploy_HappyWithCRDs(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:  "local",
		ChartURI:     "oci://example/mural",
		CRDsChartURI: "oci://example/mural-crds",
		MuralVersion: "1.0.7",
		HelmTimeout:  5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(h.calls) != 2 || h.calls[0].ReleaseName != "mural-crds" || h.calls[1].ReleaseName != "mural" {
		t.Fatalf("install ordering wrong: %+v", h.calls)
	}
	if h.calls[1].Version != "1.0.7" {
		t.Fatalf("mural version not forwarded: %q", h.calls[1].Version)
	}
	if h.calls[1].PostRenderer == nil {
		t.Fatalf("mural install should have a post-renderer attached")
	}
}

func TestDeploy_CertManagerSkipCreateNamespace(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:                    "local",
		ChartURI:                       "oci://example/mural",
		CertManagerChartURI:            "oci://example/cert-manager",
		CertManagerSkipCreateNamespace: true,
		HelmTimeout:                    5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(h.calls) < 1 {
		t.Fatal("no installs recorded")
	}
	if h.calls[0].ReleaseName != "cert-manager" || h.calls[0].CreateNS {
		t.Fatalf("cert-manager step should skip CreateNS: %+v", h.calls[0])
	}
}

func TestDeploy_HappyWithCertManagerAndCRDs(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:           "local",
		ChartURI:              "oci://example/mural",
		CRDsChartURI:          "oci://example/mural-crds",
		CertManagerChartURI:   "oci://example/cert-manager",
		CertManagerValuesFile: "cm-values.yaml",
		HelmTimeout:           5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(h.calls) != 3 {
		t.Fatalf("expected 3 helm installs (cert-manager, crds, mural), got %d: %+v", len(h.calls), h.calls)
	}
	if h.calls[0].ReleaseName != "cert-manager" || h.calls[0].Namespace != "cert-manager" {
		t.Fatalf("cert-manager install wrong: %+v", h.calls[0])
	}
	if h.calls[0].ValuesFile != "cm-values.yaml" {
		t.Fatalf("cert-manager values file not forwarded: %q", h.calls[0].ValuesFile)
	}
	if h.calls[1].ReleaseName != "mural-crds" || h.calls[2].ReleaseName != "mural" {
		t.Fatalf("install ordering wrong: %v %v",
			h.calls[1].ReleaseName, h.calls[2].ReleaseName)
	}
}

func TestDeploy_HappyFullStack_CertManagerQueueCRDsMural(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:         "local",
		ChartURI:            "oci://example/mural",
		CRDsChartURI:        "oci://example/mural-crds",
		CRDsVersion:         "0.7.1",
		CertManagerChartURI: "oci://example/cert-manager",
		CertManagerVersion:  "1.19.3",
		QueueChartURI:       "oci://example/rabbitmq",
		QueueVersion:        "14.6.6",
		QueueValuesFile:     "queue-values.yaml",
		HelmTimeout:         5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	wantOrder := []struct{ name, ns string }{
		{"cert-manager", "cert-manager"},
		{"queue", "messaging"},
		{"mural-crds", "mural-system"},
		{"mural", "mural-system"},
	}
	if len(h.calls) != len(wantOrder) {
		t.Fatalf("got %d installs, want %d: %+v", len(h.calls), len(wantOrder), h.calls)
	}
	for i, w := range wantOrder {
		if h.calls[i].ReleaseName != w.name || h.calls[i].Namespace != w.ns {
			t.Fatalf("step %d wrong: got %s/%s want %s/%s",
				i, h.calls[i].ReleaseName, h.calls[i].Namespace, w.name, w.ns)
		}
	}
	wantVersion := map[string]string{
		"cert-manager": "1.19.3",
		"queue":        "14.6.6",
		"mural-crds":   "0.7.1",
	}
	for _, c := range h.calls {
		if want, ok := wantVersion[c.ReleaseName]; ok && c.Version != want {
			t.Errorf("release %s version: got %q want %q", c.ReleaseName, c.Version, want)
		}
	}
}

func TestDeploy_QueueErrorStopsBeforeMural(t *testing.T) {
	h := &fakeInstaller{err: errors.New("queue boom"), errOnce: "queue"}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:   "local",
		ChartURI:      "oci://example/mural",
		QueueChartURI: "oci://example/rabbitmq",
		HelmTimeout:   5 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected error from queue install")
	}
	// queue install is the only one before mural — so exactly 1 call.
	if len(h.calls) != 1 {
		t.Fatalf("expected 1 call (queue only), got %d: %+v", len(h.calls), h.calls)
	}
}

func TestDeploy_CertManagerErrorStopsBeforeCRDs(t *testing.T) {
	h := &fakeInstaller{err: errors.New("cm boom"), errOnce: "cert-manager"}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName:         "local",
		ChartURI:            "oci://example/mural",
		CRDsChartURI:        "oci://example/mural-crds",
		CertManagerChartURI: "oci://example/cert-manager",
		HelmTimeout:         5 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected error from cert-manager install")
	}
	if len(h.calls) != 1 {
		t.Fatalf("expected install to stop after cert-manager, got %d calls", len(h.calls))
	}
}

func TestDeploy_HappyNoCRDs(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, func(string, ...any) {})
	d.ConfigLoader = okLoader

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local",
		ChartURI:    "oci://example/mural",
		HelmTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(h.calls) != 1 || h.calls[0].ReleaseName != "mural" {
		t.Fatalf("wrong installs: %+v", h.calls)
	}
}

func TestDeploy_EmptyClusterName(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	err := d.Deploy(context.Background(), deployer.Request{ChartURI: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeploy_EmptyChartURI(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	err := d.Deploy(context.Background(), deployer.Request{ClusterName: "local"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeploy_NilConfigLoader(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = nil
	err := d.Deploy(context.Background(), deployer.Request{ClusterName: "local", ChartURI: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeploy_ConfigLoaderError(t *testing.T) {
	want := errors.New("boom")
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = func(string) (*config.ClusterConfig, error) { return nil, want }
	err := d.Deploy(context.Background(), deployer.Request{ClusterName: "local", ChartURI: "x"})
	if !errors.Is(err, want) {
		t.Fatalf("want wraps %v, got %v", want, err)
	}
}

func TestDeploy_ClusterNotFound(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{ClusterName: "missing", ChartURI: "x"})
	if !errors.Is(err, config.ErrClusterNotFound) {
		t.Fatalf("want ErrClusterNotFound, got %v", err)
	}
}

func TestDeploy_NilHelm(t *testing.T) {
	d := deployer.New(nil, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{ClusterName: "local", ChartURI: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeploy_HelmErrorOnCRDs(t *testing.T) {
	want := errors.New("crd boom")
	h := &fakeInstaller{err: want, errOnce: "mural-crds"}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", CRDsChartURI: "y",
		HelmTimeout: 5 * time.Minute,
	})
	if !errors.Is(err, want) {
		t.Fatalf("err: %v", err)
	}
	if len(h.calls) != 1 {
		t.Fatalf("expected to stop after CRDs failure, got %d calls", len(h.calls))
	}
}

func TestDeploy_HelmErrorOnMural(t *testing.T) {
	want := errors.New("mural boom")
	h := &fakeInstaller{err: want, errOnce: "mural"}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute,
	})
	if !errors.Is(err, want) {
		t.Fatalf("err: %v", err)
	}
}

func TestDeploy_ValidateHappyPath(t *testing.T) {
	h := &fakeInstaller{}
	k := &fakeKube{
		summary: kube.PodSummary{Ready: 13, Total: 13},
		host:    "192.168.49.2.sslip.io",
		port:    31443,
	}
	var lines []string
	d := deployer.New(h, func(format string, args ...any) {
		lines = append(lines, format)
	})
	d.ConfigLoader = okLoader
	d.Kube = func(_, _ string) (kube.Client, error) { return k, nil }

	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x",
		HelmTimeout: 5 * time.Minute,
		Validate:    true, ValidateWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !k.calledWait {
		t.Fatal("validate should wait for pods")
	}
	// At least one log line should mention the derived URL.
	var sawURL bool
	for _, l := range lines {
		if strings.Contains(l, "Canvas UI") {
			sawURL = true
		}
	}
	if !sawURL {
		t.Fatalf("connection details not printed: %v", lines)
	}
}

func TestDeploy_ValidateWaitError(t *testing.T) {
	h := &fakeInstaller{}
	k := &fakeKube{waitErr: errors.New("timeout")}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	d.Kube = func(_, _ string) (kube.Client, error) { return k, nil }
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute, Validate: true,
	})
	if err == nil {
		t.Fatal("expected error from wait failure")
	}
}

func TestDeploy_ValidateNilKubeBuilder(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = okLoader
	d.Kube = nil
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute, Validate: true,
	})
	if err == nil {
		t.Fatal("expected error when Kube builder is nil")
	}
}

func TestDeploy_ValidateKubeBuilderError(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = okLoader
	d.Kube = func(_, _ string) (kube.Client, error) { return nil, errors.New("kube boom") }
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute, Validate: true,
	})
	if err == nil {
		t.Fatal("expected error from kube builder")
	}
}

func TestDeploy_ValidateIngressLookupError(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = okLoader
	d.Kube = func(_, _ string) (kube.Client, error) {
		return &fakeKube{
			summary: kube.PodSummary{Ready: 1, Total: 1},
			hostErr: errors.New("no ingress"),
		}, nil
	}
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute, Validate: true,
	})
	if err == nil {
		t.Fatal("expected error from ingress lookup")
	}
}

func TestDeploy_ValidateServiceLookupError(t *testing.T) {
	d := deployer.New(&fakeInstaller{}, nil)
	d.ConfigLoader = okLoader
	d.Kube = func(_, _ string) (kube.Client, error) {
		return &fakeKube{
			summary: kube.PodSummary{Ready: 1, Total: 1},
			host:    "x.local",
			portErr: errors.New("no nodeport"),
		}, nil
	}
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 5 * time.Minute, Validate: true,
	})
	if err == nil {
		t.Fatal("expected error from service lookup")
	}
}

func TestDeploy_RejectsZeroHelmTimeout(t *testing.T) {
	// Previously the deployer silently substituted a 20m default when
	// HelmTimeout <= 0. That made `--helm-timeout 0` mean "20m" — surprising.
	// Now the deployer rejects, leaving the cobra default of 20m as the
	// only "no override" path.
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 0,
	})
	if err == nil {
		t.Fatal("expected error for HelmTimeout=0")
	}
	if len(h.calls) != 0 {
		t.Fatalf("no helm calls should have been made, got %v", h.calls)
	}
}

func TestDeploy_PropagatesPositiveTimeout(t *testing.T) {
	h := &fakeInstaller{}
	d := deployer.New(h, nil)
	d.ConfigLoader = okLoader
	err := d.Deploy(context.Background(), deployer.Request{
		ClusterName: "local", ChartURI: "x", HelmTimeout: 7 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if h.calls[0].Timeout != 7*time.Minute {
		t.Fatalf("got timeout %s, want 7m", h.calls[0].Timeout)
	}
}
