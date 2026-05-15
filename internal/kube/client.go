// Package kube is a thin wrapper around client-go for the cluster-side
// cleanup operations the CLI needs: deleting namespaces (including the
// force-finalize escape hatch that mirrors the Ansible role's
// `cleanup_namespace.yml`), and pruning stale admission webhook
// configurations. The interface is small on purpose so cleaner tests can
// substitute a fake.
package kube

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Client is the surface the cleaner + validator depend on.
type Client interface {
	NamespaceExists(ctx context.Context, name string) (bool, error)
	DeleteNamespace(ctx context.Context, name string) error
	WaitForNamespaceGone(ctx context.Context, name string, timeout time.Duration) error
	ForceFinalizeNamespace(ctx context.Context, name string) error
	DeleteValidatingWebhookConfiguration(ctx context.Context, name string) error
	DeleteMutatingWebhookConfiguration(ctx context.Context, name string) error

	// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
	// List-by-label methods feed the cleaner's label-selector sweeps, which
	// replace the hardcoded ancillaryNamespaces + staleWebhooks lists. The
	// hardcoded lists drift every time the chart adds or renames a resource;
	// label selectors let the cleaner discover what to clean.
	ListNamespacesByLabel(ctx context.Context, selector string) ([]string, error)
	ListValidatingWebhooksByLabel(ctx context.Context, selector string) ([]string, error)
	ListMutatingWebhooksByLabel(ctx context.Context, selector string) ([]string, error)

	// Validation surface — used by deployer's --validate path. None of these
	// mutate cluster state; they're read-only / wait-only.
	WaitForPodsReady(ctx context.Context, namespace string, timeout time.Duration) (PodSummary, error)
	GetIngressHost(ctx context.Context, namespace, name string) (string, error)
	GetServiceNodePort(ctx context.Context, namespace, serviceName, portName string) (int32, error)
}

// PodSummary is the shape returned by WaitForPodsReady. Ready, Total, and
// NotReady make it easy to print a "12/13 Ready (1 not ready: …)" line.
type PodSummary struct {
	Ready    int
	Total    int
	NotReady []string
}

// ErrTimeout is returned when WaitForNamespaceGone exceeds the deadline.
var ErrTimeout = errors.New("kube: timed out waiting for resource")

// NewClient builds a Client from a kubeconfig path + optional context.
// An empty context uses the kubeconfig's current-context. The clientset is
// constructed eagerly so callers get a fast failure on misconfiguration.
func NewClient(kubeconfig, kubeContext string) (Client, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("kube: kubeconfig path is required")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: load client config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kube: new clientset: %w", err)
	}
	return &clientsetWrapper{cs: cs}, nil
}

// clientsetWrapper is the production Client. It's the only place client-go
// types leak in — everything else talks to the Client interface.
type clientsetWrapper struct {
	cs kubernetes.Interface
}

func (c *clientsetWrapper) NamespaceExists(ctx context.Context, name string) (bool, error) {
	_, err := c.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("kube: get namespace %q: %w", name, err)
	}
}

func (c *clientsetWrapper) DeleteNamespace(ctx context.Context, name string) error {
	err := c.cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: delete namespace %q: %w", name, err)
	}
	return nil
}

// WaitForNamespaceGone polls until Get returns NotFound or the deadline hits.
//
// Closes H4 — see reviews/2026-05-15T195833Z-review.md#h4.
// The derived ctx upper-bounds every API call (including the first), so a
// hung apiserver can't outrun the overall timeout.
func (c *clientsetWrapper) WaitForNamespaceGone(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		exists, err := c.NamespaceExists(ctx, name)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w: namespace %q still present after %s", ErrTimeout, name, timeout)
			}
			return err
		}
		if !exists {
			return nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w: namespace %q still present after %s", ErrTimeout, name, timeout)
			}
			return ctx.Err()
		}
	}
}

// ForceFinalizeNamespace clears spec.finalizers on a stuck Terminating
// namespace by calling the /finalize subresource — same workaround as
// ansible/paletteai/roles/paletteai/tasks/cleanup_namespace.yml. Use only
// when a Delete + Wait have already failed; finalizers exist for a reason
// and ripping them out can orphan cluster-scoped resources.
func (c *clientsetWrapper) ForceFinalizeNamespace(ctx context.Context, name string) error {
	ns, err := c.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("kube: get namespace %q: %w", name, err)
	}
	if len(ns.Spec.Finalizers) == 0 {
		return nil
	}

	// Round-trip through the typed Finalize() call (POST /finalize) with an
	// empty finalizer list. The typed method is fake-clientset-aware, which
	// the raw RESTClient().Put(...).AbsPath() approach isn't.
	patched := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: ns.ResourceVersion},
		Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{}},
	}
	if _, err := c.cs.CoreV1().Namespaces().Finalize(ctx, patched, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: force-finalize namespace %q: %w", name, err)
	}
	return nil
}

func (c *clientsetWrapper) DeleteValidatingWebhookConfiguration(ctx context.Context, name string) error {
	err := c.cs.AdmissionregistrationV1().
		ValidatingWebhookConfigurations().
		Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: delete validatingwebhookconfiguration %q: %w", name, err)
	}
	return nil
}

// WaitForPodsReady polls the namespace until every non-terminal pod reports
// Ready or the deadline elapses.
//
// Closes H1 + H4 — see reviews/2026-05-15T195833Z-review.md#h1, …#h4.
// The derived ctx upper-bounds every API call (including the first), so a
// hung apiserver can't outrun the overall timeout (H4).
//
// "Ready" follows kubectl wait --for=condition=Ready for running pods, and
// additionally treats pods in PodSucceeded / PodFailed phase as done — they
// won't satisfy a PodReady=True condition but they also aren't pending. This
// matters during day-1 helm installs where pre/post-install Hook Jobs leave
// behind Succeeded pods that would otherwise wedge the wait (H1).
func (c *clientsetWrapper) WaitForPodsReady(ctx context.Context, namespace string, timeout time.Duration) (PodSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last PodSummary
	for {
		pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return last, fmt.Errorf("%w: %d/%d pods Ready in %q after %s",
					ErrTimeout, last.Ready, last.Total, namespace, timeout)
			}
			return last, fmt.Errorf("kube: list pods in %q: %w", namespace, err)
		}
		last = summarizePods(pods.Items)
		if last.Total > 0 && len(last.NotReady) == 0 {
			return last, nil
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return last, fmt.Errorf("%w: %d/%d pods Ready in %q after %s",
					ErrTimeout, last.Ready, last.Total, namespace, timeout)
			}
			return last, ctx.Err()
		}
	}
}

func summarizePods(pods []corev1.Pod) PodSummary {
	out := PodSummary{Total: len(pods)}
	for _, p := range pods {
		if isPodReady(&p) {
			out.Ready++
		} else {
			out.NotReady = append(out.NotReady, p.Name)
		}
	}
	return out
}

func isPodReady(p *corev1.Pod) bool {
	// Terminal phases (Succeeded / Failed) don't gate readiness — the pod
	// has finished its work. Helm pre/post-install Hook Jobs commonly leave
	// behind Succeeded pods with no PodReady condition; treating them as
	// not-ready wedges the validate() wait forever.
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// GetIngressHost returns the host of the first rule on the named Ingress.
// Used by the validator to derive the public URL after a deploy without
// asking the operator to repeat the domain on the command line.
func (c *clientsetWrapper) GetIngressHost(ctx context.Context, namespace, name string) (string, error) {
	ing, err := c.cs.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("kube: get ingress %s/%s: %w", namespace, name, err)
	}
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			return r.Host, nil
		}
	}
	return "", fmt.Errorf("kube: ingress %s/%s has no host rules", namespace, name)
}

// GetServiceNodePort returns the NodePort of the named port on a Service.
// Errors when the port isn't found, the Service isn't of type NodePort, or
// the NodePort field is 0 (Service still being provisioned).
func (c *clientsetWrapper) GetServiceNodePort(ctx context.Context, namespace, serviceName, portName string) (int32, error) {
	svc, err := c.cs.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("kube: get service %s/%s: %w", namespace, serviceName, err)
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == portName {
			if p.NodePort == 0 {
				return 0, fmt.Errorf("kube: service %s/%s port %q has no NodePort assigned", namespace, serviceName, portName)
			}
			return p.NodePort, nil
		}
	}
	return 0, fmt.Errorf("kube: service %s/%s has no port named %q", namespace, serviceName, portName)
}

func (c *clientsetWrapper) DeleteMutatingWebhookConfiguration(ctx context.Context, name string) error {
	err := c.cs.AdmissionregistrationV1().
		MutatingWebhookConfigurations().
		Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: delete mutatingwebhookconfiguration %q: %w", name, err)
	}
	return nil
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
// ListNamespacesByLabel returns namespace names matching the given label
// selector (e.g. "app.kubernetes.io/part-of=mural"). Empty selector returns
// every namespace, which the cleaner intentionally avoids — callers should
// always pass a non-empty selector.
func (c *clientsetWrapper) ListNamespacesByLabel(ctx context.Context, selector string) ([]string, error) {
	out, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("kube: list namespaces by label %q: %w", selector, err)
	}
	names := make([]string, 0, len(out.Items))
	for _, ns := range out.Items {
		names = append(names, ns.Name)
	}
	return names, nil
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
func (c *clientsetWrapper) ListValidatingWebhooksByLabel(ctx context.Context, selector string) ([]string, error) {
	out, err := c.cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("kube: list validatingwebhookconfigurations by label %q: %w", selector, err)
	}
	names := make([]string, 0, len(out.Items))
	for _, w := range out.Items {
		names = append(names, w.Name)
	}
	return names, nil
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
func (c *clientsetWrapper) ListMutatingWebhooksByLabel(ctx context.Context, selector string) ([]string, error) {
	out, err := c.cs.AdmissionregistrationV1().MutatingWebhookConfigurations().
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("kube: list mutatingwebhookconfigurations by label %q: %w", selector, err)
	}
	names := make([]string, 0, len(out.Items))
	for _, w := range out.Items {
		names = append(names, w.Name)
	}
	return names, nil
}
