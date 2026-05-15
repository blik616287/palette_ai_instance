package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func newWithFake() (*clientsetWrapper, *fake.Clientset) {
	cs := fake.NewSimpleClientset()
	return &clientsetWrapper{cs: cs}, cs
}

func TestNamespaceExists(t *testing.T) {
	w, cs := newWithFake()
	exists, err := w.NamespaceExists(context.Background(), "missing")
	if err != nil || exists {
		t.Fatalf("expected missing=false, got exists=%v err=%v", exists, err)
	}
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mural-system"}},
		metav1.CreateOptions{})
	exists, err = w.NamespaceExists(context.Background(), "mural-system")
	if err != nil || !exists {
		t.Fatalf("expected existing=true, got exists=%v err=%v", exists, err)
	}
}

func TestNamespaceExists_PropagatesNonNotFoundError(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("get", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
	_, err := w.NamespaceExists(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteNamespace_IgnoresNotFound(t *testing.T) {
	w, _ := newWithFake()
	if err := w.DeleteNamespace(context.Background(), "missing"); err != nil {
		t.Fatalf("delete missing ns should be a no-op, got %v", err)
	}
}

func TestDeleteNamespace_PropagatesOtherErrors(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("delete", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
	if err := w.DeleteNamespace(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestWaitForNamespaceGone_AlreadyGone(t *testing.T) {
	w, _ := newWithFake()
	if err := w.WaitForNamespaceGone(context.Background(), "missing", 100*time.Millisecond); err != nil {
		t.Fatalf("WaitForNamespaceGone: %v", err)
	}
}

func TestWaitForNamespaceGone_Timeout(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stuck"}},
		metav1.CreateOptions{})
	err := w.WaitForNamespaceGone(context.Background(), "stuck", 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestWaitForNamespaceGone_ContextCancel(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stuck"}},
		metav1.CreateOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.WaitForNamespaceGone(ctx, "stuck", 5*time.Second)
	if err == nil {
		t.Fatal("expected error from cancelled ctx")
	}
}

func TestWaitForNamespaceGone_PropagatesGetError(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("get", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
	err := w.WaitForNamespaceGone(context.Background(), "x", time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
}

// Closes H4 — see reviews/2026-05-15T195833Z-review.md#h4.
// When an apiserver call itself times out (returns context.DeadlineExceeded),
// the wait should surface ErrTimeout, not the raw ctx error.
func TestWaitForNamespaceGone_APIDeadlineExceededMapsToErrTimeout(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stuck"}}, metav1.CreateOptions{})
	cs.PrependReactor("get", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
	err := w.WaitForNamespaceGone(context.Background(), "stuck", 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout from DeadlineExceeded API error, got %v", err)
	}
}

func TestForceFinalizeNamespace_NoFinalizersIsNoop(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		metav1.CreateOptions{})
	if err := w.ForceFinalizeNamespace(context.Background(), "plain"); err != nil {
		t.Fatalf("ForceFinalizeNamespace: %v", err)
	}
}

func TestForceFinalizeNamespace_MissingIsNoop(t *testing.T) {
	w, _ := newWithFake()
	if err := w.ForceFinalizeNamespace(context.Background(), "missing"); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

func TestForceFinalizeNamespace_PropagatesGetError(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("get", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewBadRequest("boom")
		})
	if err := w.ForceFinalizeNamespace(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestForceFinalizeNamespace_FinalizersPresentClearsThem(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "stuck"},
			Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{"kubernetes"}},
		}, metav1.CreateOptions{})
	if err := w.ForceFinalizeNamespace(context.Background(), "stuck"); err != nil {
		t.Fatalf("ForceFinalizeNamespace: %v", err)
	}
	// Inspect the fake's action log: the typed Finalize() call records as
	// `update` on the `namespaces/finalize` subresource.
	var sawFinalize bool
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetSubresource() == "finalize" {
			sawFinalize = true
		}
	}
	if !sawFinalize {
		t.Fatalf("expected finalize subresource update; got %+v", cs.Actions())
	}
}

func TestForceFinalizeNamespace_PropagatesFinalizeError(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "stuck"},
			Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{"kubernetes"}},
		}, metav1.CreateOptions{})
	cs.PrependReactor("create", "namespaces",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() == "finalize" {
				return true, nil, errors.New("finalize boom")
			}
			return false, nil, nil
		})
	if err := w.ForceFinalizeNamespace(context.Background(), "stuck"); err == nil {
		t.Fatal("expected propagated error")
	}
}

func TestForceFinalizeNamespace_SwallowsFinalizeNotFound(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "stuck"},
			Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{"kubernetes"}},
		}, metav1.CreateOptions{})
	cs.PrependReactor("create", "namespaces",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() == "finalize" {
				return true, nil, apierrors.NewNotFound(
					schema.GroupResource{Resource: "namespaces"}, "stuck")
			}
			return false, nil, nil
		})
	if err := w.ForceFinalizeNamespace(context.Background(), "stuck"); err != nil {
		t.Fatalf("NotFound should be swallowed, got %v", err)
	}
}

func TestDeleteValidatingWebhookConfiguration(t *testing.T) {
	w, cs := newWithFake()
	if err := w.DeleteValidatingWebhookConfiguration(context.Background(), "missing"); err != nil {
		t.Fatalf("missing should be no-op, got %v", err)
	}
	cs.PrependReactor("delete", "validatingwebhookconfigurations",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
	if err := w.DeleteValidatingWebhookConfiguration(context.Background(), "x"); err == nil {
		t.Fatal("expected propagated error")
	}
}

func TestDeleteMutatingWebhookConfiguration(t *testing.T) {
	w, cs := newWithFake()
	if err := w.DeleteMutatingWebhookConfiguration(context.Background(), "missing"); err != nil {
		t.Fatalf("missing should be no-op, got %v", err)
	}
	cs.PrependReactor("delete", "mutatingwebhookconfigurations",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
	if err := w.DeleteMutatingWebhookConfiguration(context.Background(), "x"); err == nil {
		t.Fatal("expected propagated error")
	}
}

func TestWaitForPodsReady_PendingPodWithoutPodReadyConditionIsNotReady(t *testing.T) {
	// Covers the bottom return in isPodReady — a pending pod whose
	// Status.Conditions has no PodReady entry yet should be treated as
	// not-ready. Pods in terminal phases (Succeeded/Failed) are handled
	// separately and DO count as ready — see the SucceededPodCountsAsReady
	// test below.
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "no-condition", Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}, metav1.CreateOptions{})
	_, err := w.WaitForPodsReady(context.Background(), "ns", 150*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout when pending pod has no PodReady condition, got %v", err)
	}
}

func TestWaitForPodsReady_TerminalPodsCountAsReady(t *testing.T) {
	// Helm pre/post-install Hook Jobs leave behind Succeeded pods that
	// never have a PodReady=True condition. Before this fix, those pods
	// wedged validate() forever. Now they should count as ready.
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "hook-job-pod", Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		}, metav1.CreateOptions{})
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(),
		readyPod("running-pod"), metav1.CreateOptions{})
	sum, err := w.WaitForPodsReady(context.Background(), "ns", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForPodsReady: %v", err)
	}
	if sum.Ready != 2 || sum.Total != 2 {
		t.Fatalf("summary: %+v", sum)
	}
}

func TestWaitForPodsReady_FailedPodCountsAsReady(t *testing.T) {
	// Same as above for Failed-phase pods.
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "errored-pod", Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed},
		}, metav1.CreateOptions{})
	sum, err := w.WaitForPodsReady(context.Background(), "ns", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForPodsReady: %v", err)
	}
	if sum.Ready != 1 {
		t.Fatalf("summary: %+v", sum)
	}
}

func TestWaitForPodsReady_AllReady(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(), readyPod("a"), metav1.CreateOptions{})
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(), readyPod("b"), metav1.CreateOptions{})
	sum, err := w.WaitForPodsReady(context.Background(), "ns", time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if sum.Ready != 2 || sum.Total != 2 || len(sum.NotReady) != 0 {
		t.Fatalf("summary wrong: %+v", sum)
	}
}

func TestWaitForPodsReady_Timeout(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(), notReadyPod("stuck"), metav1.CreateOptions{})
	_, err := w.WaitForPodsReady(context.Background(), "ns", 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestWaitForPodsReady_EmptyNamespaceTimesOut(t *testing.T) {
	w, _ := newWithFake()
	// Zero pods means we never satisfy the "Total>0 && all Ready" check.
	_, err := w.WaitForPodsReady(context.Background(), "ns", 100*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout for empty ns, got %v", err)
	}
}

func TestWaitForPodsReady_ListError(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("list", "pods",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list boom")
		})
	_, err := w.WaitForPodsReady(context.Background(), "ns", time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
}

// Closes H4 — see reviews/2026-05-15T195833Z-review.md#h4.
// A List that itself returns context.DeadlineExceeded must surface as ErrTimeout.
func TestWaitForPodsReady_APIDeadlineExceededMapsToErrTimeout(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("list", "pods",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
	_, err := w.WaitForPodsReady(context.Background(), "ns", 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout from DeadlineExceeded list error, got %v", err)
	}
}

func TestWaitForPodsReady_ContextCancel(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Pods("ns").Create(context.Background(), notReadyPod("stuck"), metav1.CreateOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.WaitForPodsReady(ctx, "ns", 5*time.Second)
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestGetIngressHost(t *testing.T) {
	w, cs := newWithFake()
	if _, err := w.GetIngressHost(context.Background(), "ns", "missing"); err == nil {
		t.Fatal("expected error on missing ingress")
	}
	_, _ = cs.NetworkingV1().Ingresses("ns").Create(context.Background(),
		ingress("dex", "x.local"), metav1.CreateOptions{})
	host, err := w.GetIngressHost(context.Background(), "ns", "dex")
	if err != nil || host != "x.local" {
		t.Fatalf("got host=%q err=%v", host, err)
	}
}

func TestGetIngressHost_NoRules(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.NetworkingV1().Ingresses("ns").Create(context.Background(),
		ingress("dex", ""), metav1.CreateOptions{})
	if _, err := w.GetIngressHost(context.Background(), "ns", "dex"); err == nil {
		t.Fatal("expected error when no rule has a host")
	}
}

func TestGetServiceNodePort(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Services("ns").Create(context.Background(),
		service("ingress-nginx-controller", "https", 31443), metav1.CreateOptions{})
	port, err := w.GetServiceNodePort(context.Background(), "ns", "ingress-nginx-controller", "https")
	if err != nil || port != 31443 {
		t.Fatalf("got port=%d err=%v", port, err)
	}
}

func TestGetServiceNodePort_MissingService(t *testing.T) {
	w, _ := newWithFake()
	if _, err := w.GetServiceNodePort(context.Background(), "ns", "missing", "https"); err == nil {
		t.Fatal("expected error on missing svc")
	}
}

func TestGetServiceNodePort_WrongPortName(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Services("ns").Create(context.Background(),
		service("svc", "http", 31080), metav1.CreateOptions{})
	if _, err := w.GetServiceNodePort(context.Background(), "ns", "svc", "https"); err == nil {
		t.Fatal("expected error on unknown port name")
	}
}

func TestGetServiceNodePort_PortNotYetAssigned(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Services("ns").Create(context.Background(),
		service("svc", "https", 0), metav1.CreateOptions{})
	if _, err := w.GetServiceNodePort(context.Background(), "ns", "svc", "https"); err == nil {
		t.Fatal("expected error when NodePort is unassigned")
	}
}

// ---- tiny pod/svc/ingress builders for the tests above -----------------------

func readyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
}

func notReadyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}},
	}
}

func ingress(name, host string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	if host != "" {
		ing.Spec.Rules = []networkingv1.IngressRule{{Host: host}}
	} else {
		ing.Spec.Rules = []networkingv1.IngressRule{{}}
	}
	return ing
}

func service(name, portName string, nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Name: portName, NodePort: nodePort}},
		},
	}
}

// Closes L3 — see reviews/2026-05-15T195833Z-review.md#l3.
func TestListNamespacesByLabel(t *testing.T) {
	w, cs := newWithFake()
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tagged", Labels: map[string]string{"app": "mural"}}},
		metav1.CreateOptions{})
	_, _ = cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "untagged"}},
		metav1.CreateOptions{})
	got, err := w.ListNamespacesByLabel(context.Background(), "app=mural")
	if err != nil {
		t.Fatalf("ListNamespacesByLabel: %v", err)
	}
	if len(got) != 1 || got[0] != "tagged" {
		t.Fatalf("got %v, want [tagged]", got)
	}
}

func TestListNamespacesByLabel_PropagatesError(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("list", "namespaces",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list boom")
		})
	if _, err := w.ListNamespacesByLabel(context.Background(), "x=y"); err == nil {
		t.Fatal("expected error")
	}
}

func TestListValidatingAndMutatingWebhooksByLabel(t *testing.T) {
	w, cs := newWithFake()
	// Empty cluster — both should return empty slices, no error.
	v, err := w.ListValidatingWebhooksByLabel(context.Background(), "x=y")
	if err != nil || len(v) != 0 {
		t.Fatalf("validating: got %v err=%v", v, err)
	}
	m, err := w.ListMutatingWebhooksByLabel(context.Background(), "x=y")
	if err != nil || len(m) != 0 {
		t.Fatalf("mutating: got %v err=%v", m, err)
	}
	// Seed one of each and confirm names come back.
	_, _ = cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.Background(),
		&admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "vw"}},
		metav1.CreateOptions{})
	_, _ = cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.Background(),
		&admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "mw"}},
		metav1.CreateOptions{})
	if v, _ := w.ListValidatingWebhooksByLabel(context.Background(), ""); len(v) != 1 || v[0] != "vw" {
		t.Fatalf("validating list: %v", v)
	}
	if m, _ := w.ListMutatingWebhooksByLabel(context.Background(), ""); len(m) != 1 || m[0] != "mw" {
		t.Fatalf("mutating list: %v", m)
	}
}

func TestListWebhooksByLabel_PropagateErrors(t *testing.T) {
	w, cs := newWithFake()
	cs.PrependReactor("list", "validatingwebhookconfigurations",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("vboom")
		})
	cs.PrependReactor("list", "mutatingwebhookconfigurations",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("mboom")
		})
	if _, err := w.ListValidatingWebhooksByLabel(context.Background(), "x=y"); err == nil {
		t.Fatal("expected validating list error")
	}
	if _, err := w.ListMutatingWebhooksByLabel(context.Background(), "x=y"); err == nil {
		t.Fatal("expected mutating list error")
	}
}

func TestNewClient_RequiresKubeconfigPath(t *testing.T) {
	if _, err := NewClient("", ""); err == nil {
		t.Fatal("expected error with empty kubeconfig path")
	}
}

func TestNewClient_BogusKubeconfig(t *testing.T) {
	if _, err := NewClient("/proc/cmdline", ""); err == nil {
		t.Fatal("expected error loading non-kubeconfig file")
	}
}
