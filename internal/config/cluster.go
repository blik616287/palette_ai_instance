// Package config loads `cluster_config.yaml` — the list of clusters that the
// `deploy` command can target. Relative paths inside the file are resolved
// against the file's own directory so configs are portable.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Cluster is a single target the CLI can deploy to. The Deploy block carries
// per-cluster defaults for `deploy`'s optional flags; the CLI layer overrides
// any of them with a flag that was explicitly set on the command line.
//
// Cleanup intentionally has no per-cluster defaults — destructive operations
// must be re-specified on every invocation so they can't surprise an operator
// who happens to run `palette-ai-instance cleanup --config …`.
type Cluster struct {
	Name       string         `yaml:"name"`
	Kubeconfig string         `yaml:"kubeconfig"`
	Context    string         `yaml:"context"`
	Namespace  string         `yaml:"namespace"`
	Deploy     DeployDefaults `yaml:"deploy,omitempty"`
}

// DeployDefaults mirrors the optional flags of `palette-ai-instance deploy`.
// Keys use the kebab-case flag names so the YAML stays self-documenting.
type DeployDefaults struct {
	Version                        string        `yaml:"version,omitempty"`
	ValuesFile                     string        `yaml:"values-file,omitempty"`
	ChartURI                       string        `yaml:"chart-uri,omitempty"`
	CRDsChartURI                   string        `yaml:"crds-chart-uri,omitempty"`
	CRDsVersion                    string        `yaml:"crds-version,omitempty"`
	CertManagerChartURI            string        `yaml:"cert-manager-chart-uri,omitempty"`
	CertManagerVersion             string        `yaml:"cert-manager-version,omitempty"`
	CertManagerValuesFile          string        `yaml:"cert-manager-values-file,omitempty"`
	CertManagerSkipCreateNamespace bool          `yaml:"cert-manager-no-create-ns,omitempty"`
	FluxChartURI                   string        `yaml:"flux-chart-uri,omitempty"`
	FluxVersion                    string        `yaml:"flux-version,omitempty"`
	FluxValuesFile                 string        `yaml:"flux-values-file,omitempty"`
	QueueChartURI                  string        `yaml:"queue-chart-uri,omitempty"`
	QueueVersion                   string        `yaml:"queue-version,omitempty"`
	QueueValuesFile                string        `yaml:"queue-values-file,omitempty"`
	HelmTimeout                    time.Duration `yaml:"helm-timeout,omitempty"`
	SetValues                      []string      `yaml:"set,omitempty"`
	Validate                       bool          `yaml:"validate,omitempty"`
	ValidateWait                   time.Duration `yaml:"validate-wait,omitempty"`
}

// ClusterConfig is the top-level shape of cluster_config.yaml.
type ClusterConfig struct {
	Clusters []Cluster `yaml:"clusters"`
}

// ErrClusterNotFound is returned by Get when no cluster matches the given name.
var ErrClusterNotFound = errors.New("cluster not found in config")

// Load reads and parses cluster_config.yaml at path. Relative kubeconfig
// paths inside the file are resolved against path's directory.
func Load(path string) (*ClusterConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cluster config %q: %w", path, err)
	}

	var cfg ClusterConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cluster config %q: %w", path, err)
	}

	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("cluster config %q has no clusters", path)
	}

	baseDir := filepath.Dir(path)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Kubeconfig = resolvePath(baseDir, cfg.Clusters[i].Kubeconfig)
		resolveDeployPaths(baseDir, &cfg.Clusters[i].Deploy)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Get returns the cluster with the given name, or ErrClusterNotFound.
func (c *ClusterConfig) Get(name string) (*Cluster, error) {
	for i := range c.Clusters {
		if c.Clusters[i].Name == name {
			return &c.Clusters[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrClusterNotFound, name)
}

// Names returns the cluster names in declaration order — useful for help text.
func (c *ClusterConfig) Names() []string {
	out := make([]string, 0, len(c.Clusters))
	for i := range c.Clusters {
		out = append(out, c.Clusters[i].Name)
	}
	return out
}

// validate enforces that each cluster has the fields we actually need.
func (c *ClusterConfig) validate() error {
	seen := make(map[string]struct{}, len(c.Clusters))
	for i, cl := range c.Clusters {
		switch {
		case cl.Name == "":
			return fmt.Errorf("cluster at index %d is missing name", i)
		case cl.Kubeconfig == "":
			return fmt.Errorf("cluster %q is missing kubeconfig", cl.Name)
		case cl.Namespace == "":
			return fmt.Errorf("cluster %q is missing namespace", cl.Name)
		}
		if _, dup := seen[cl.Name]; dup {
			return fmt.Errorf("duplicate cluster name %q", cl.Name)
		}
		seen[cl.Name] = struct{}{}
	}
	return nil
}

// resolvePath leaves absolute paths and URI schemes alone, and resolves
// relative paths against baseDir. An empty input returns empty (caller
// validates downstream).
func resolvePath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) || isURIScheme(p) {
		return p
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

// isURIScheme returns true for refs that helm/k8s consume directly (oci://,
// https://, http://). We never want to mangle these into local paths.
func isURIScheme(s string) bool {
	for _, prefix := range []string{"oci://", "https://", "http://", "file://"} {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// resolveDeployPaths rewrites every path-bearing field on DeployDefaults so
// it points at a real file relative to cluster_config.yaml. URIs (oci://,
// https://) pass through untouched.
func resolveDeployPaths(baseDir string, d *DeployDefaults) {
	d.ValuesFile = resolvePath(baseDir, d.ValuesFile)
	d.ChartURI = resolvePath(baseDir, d.ChartURI)
	d.CRDsChartURI = resolvePath(baseDir, d.CRDsChartURI)
	d.CertManagerChartURI = resolvePath(baseDir, d.CertManagerChartURI)
	d.CertManagerValuesFile = resolvePath(baseDir, d.CertManagerValuesFile)
	d.FluxChartURI = resolvePath(baseDir, d.FluxChartURI)
	d.FluxValuesFile = resolvePath(baseDir, d.FluxValuesFile)
	d.QueueChartURI = resolvePath(baseDir, d.QueueChartURI)
	d.QueueValuesFile = resolvePath(baseDir, d.QueueValuesFile)
}
