package helm_test

import (
	"errors"
	"testing"
	"time"

	"palette-ai-instance/internal/helm"
)

func TestInstallOptions_Validate(t *testing.T) {
	good := helm.InstallOptions{
		ReleaseName: "mural",
		Namespace:   "mural-system",
		ChartURI:    "oci://example/mural",
		Kubeconfig:  "/tmp/kc",
	}

	tt := []struct {
		name      string
		mutate    func(*helm.InstallOptions)
		wantError bool
	}{
		{name: "happy", mutate: func(*helm.InstallOptions) {}},
		{name: "no release", mutate: func(o *helm.InstallOptions) { o.ReleaseName = "" }, wantError: true},
		{name: "no namespace", mutate: func(o *helm.InstallOptions) { o.Namespace = "" }, wantError: true},
		{name: "no chart", mutate: func(o *helm.InstallOptions) { o.ChartURI = "" }, wantError: true},
		{name: "no kubeconfig", mutate: func(o *helm.InstallOptions) { o.Kubeconfig = "" }, wantError: true},
		{
			name: "wait with no timeout",
			mutate: func(o *helm.InstallOptions) {
				o.Wait = true
				o.Timeout = 0
			},
			wantError: true,
		},
		{
			name: "wait with positive timeout",
			mutate: func(o *helm.InstallOptions) {
				o.Wait = true
				o.Timeout = 30 * time.Second
			},
		},
	}

	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.mutate(&o)
			err := o.Validate()
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !errors.Is(err, helm.ErrInvalidOptions) {
					t.Fatalf("error is not ErrInvalidOptions: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
