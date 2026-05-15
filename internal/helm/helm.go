// Package helm wraps helm.sh/helm/v3 behind a small Installer interface so
// the deployer can be unit-tested without a real cluster.
package helm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/postrender"
)

// LogFunc is the optional progress sink used by SDKInstaller. nil silences.
type LogFunc func(format string, args ...any)

// InstallOptions describes one helm release the caller wants on the cluster.
type InstallOptions struct {
	ReleaseName string
	Namespace   string
	ChartURI    string
	Version     string
	ValuesFile  string
	// SetValues mirrors helm's `--set key=value` flag — applied on top of
	// ValuesFile. Use dot-paths for nested keys (e.g. "dex.config.issuer").
	SetValues  []string
	Kubeconfig string
	Context    string
	Wait       bool
	Timeout    time.Duration
	CreateNS   bool
	// PostRenderer, when non-nil, is applied to the rendered manifests
	// before they're sent to the cluster (helm's `--post-renderer` flag).
	PostRenderer postrender.PostRenderer
}

// Validate enforces the fields the installer actually needs before we even
// touch the cluster. Returns a wrapped ErrInvalidOptions on failure so the
// caller can use errors.Is in tests and error reporting.
func (o InstallOptions) Validate() error {
	switch {
	case o.ReleaseName == "":
		return fmt.Errorf("%w: release name is required", ErrInvalidOptions)
	case o.Namespace == "":
		return fmt.Errorf("%w: namespace is required", ErrInvalidOptions)
	case o.ChartURI == "":
		return fmt.Errorf("%w: chart URI is required", ErrInvalidOptions)
	case o.Kubeconfig == "":
		return fmt.Errorf("%w: kubeconfig is required", ErrInvalidOptions)
	case o.Wait && o.Timeout <= 0:
		return fmt.Errorf("%w: timeout must be > 0 when wait=true", ErrInvalidOptions)
	}
	return nil
}

// Release is the trimmed-down result we surface back to callers. The full
// release.Release type from helm has many fields we don't use.
type Release struct {
	Name      string
	Namespace string
	Revision  int
	Status    string
}

// Installer is the interface the deployer depends on. SDKInstaller is the
// real implementation; tests use their own fakes.
type Installer interface {
	Apply(ctx context.Context, opts InstallOptions) (*Release, error)
}

// UninstallOptions is the minimum input for a helm uninstall. ReleaseName +
// Namespace identify the release; Kubeconfig + Context route helm to the
// right cluster. Timeout is helm's per-step deadline (hook + delete waits).
type UninstallOptions struct {
	ReleaseName string
	Namespace   string
	Kubeconfig  string
	Context     string
	Timeout     time.Duration
	// KeepHistory leaves the release record in place so a subsequent
	// `helm history` can see it. Defaults to false (release fully removed).
	KeepHistory bool
	// DisableHooks matches helm's `--no-hooks` flag (action.Uninstall.DisableHooks).
	//
	// Closes H3 — see reviews/2026-05-15T195833Z-review.md#h3.
	// Per-release toggle. The mural chart's pre-delete hook Job sometimes
	// hangs when the install was already partially broken, so cleaner sets
	// this true for the `mural` release only. Other releases (cert-manager,
	// queue, mural-crds) want hooks intact for clean teardown.
	DisableHooks bool
}

// Validate enforces the fields required for a useful uninstall call.
func (o UninstallOptions) Validate() error {
	switch {
	case o.ReleaseName == "":
		return fmt.Errorf("%w: release name is required", ErrInvalidOptions)
	case o.Namespace == "":
		return fmt.Errorf("%w: namespace is required", ErrInvalidOptions)
	case o.Kubeconfig == "":
		return fmt.Errorf("%w: kubeconfig is required", ErrInvalidOptions)
	}
	return nil
}

// Uninstaller removes a helm release. The cleaner uses this to reverse what
// the deployer's Apply set up. SDKInstaller satisfies it alongside Installer.
type Uninstaller interface {
	Uninstall(ctx context.Context, opts UninstallOptions) error
}

// ErrReleaseNotFound is returned by SDKInstaller.Uninstall when the named
// release doesn't exist (so the cleaner can treat it as a no-op rather than
// a hard failure — uninstall is idempotent).
var ErrReleaseNotFound = errors.New("helm release not found")

// ErrInvalidOptions sentinel for InstallOptions.Validate failures.
var ErrInvalidOptions = errors.New("invalid install options")
