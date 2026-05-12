package postrender_test

import (
	"bytes"
	"strings"
	"testing"

	"palette-ai-instance/internal/postrender"
)

func run(t *testing.T, in string) string {
	t.Helper()
	out, err := postrender.FixNilValues{}.Run(bytes.NewBufferString(in))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.String()
}

func TestFixNilValues_QuotesEmptyStringDataFields(t *testing.T) {
	in := `
stringData:
  certSecretName:
  proxySecretName:
  basicAuthSecretName:
`
	got := run(t, in)
	for _, f := range []string{"certSecretName", "proxySecretName", "basicAuthSecretName"} {
		want := f + `: ""`
		if !strings.Contains(got, want) {
			t.Errorf("missing rewrite for %s; got:\n%s", f, got)
		}
	}
}

func TestFixNilValues_LeavesPopulatedStringDataAlone(t *testing.T) {
	in := `
stringData:
  certSecretName: my-cert
`
	if got := run(t, in); !strings.Contains(got, "certSecretName: my-cert") {
		t.Fatalf("populated field was rewritten:\n%s", got)
	}
	if strings.Contains(run(t, in), `certSecretName: ""`) {
		t.Fatalf("populated field was overwritten with empty")
	}
}

func TestFixNilValues_LeavesTrailingWhitespaceFieldsAlone(t *testing.T) {
	// Only "empty value" lines get rewritten — a value with trailing
	// whitespace and content still must NOT become `: ""`.
	in := "certSecretName: real-value  \n"
	got := run(t, in)
	if !strings.Contains(got, "certSecretName: real-value") {
		t.Fatalf("real value lost:\n%s", got)
	}
}

func TestFixNilValues_InjectsZotServiceName(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: zot
spec:
  replicas: 1
`
	got := run(t, in)
	if !strings.Contains(got, "  serviceName: zot\n  replicas: 1") {
		t.Fatalf("serviceName not injected:\n%s", got)
	}
}

func TestFixNilValues_SkipsNonZotStatefulSet(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: other
spec:
  replicas: 1
`
	got := run(t, in)
	if strings.Contains(got, "serviceName: zot") {
		t.Fatalf("serviceName injected into wrong StatefulSet:\n%s", got)
	}
}

func TestFixNilValues_LeavesServiceNameAloneIfPresent(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: zot
spec:
  serviceName: zot-headless
  replicas: 1
`
	got := run(t, in)
	occurrences := strings.Count(got, "serviceName:")
	if occurrences != 1 {
		t.Fatalf("expected exactly 1 serviceName, got %d:\n%s", occurrences, got)
	}
}

func TestFixNilValues_HandlesEmptyInput(t *testing.T) {
	got := run(t, "")
	if got != "" {
		t.Fatalf("empty input should produce empty output, got %q", got)
	}
}

func TestFixNilValues_HandlesNonZotStatefulSetAlongsideZot(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: another
spec:
  replicas: 2
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: zot
spec:
  replicas: 1
`
	got := run(t, in)
	if !strings.Contains(got, "  serviceName: zot\n  replicas: 1") {
		t.Fatalf("zot serviceName not injected:\n%s", got)
	}
	// The "another" statefulset must not get a serviceName.
	otherIdx := strings.Index(got, "name: another")
	zotIdx := strings.Index(got, "name: zot")
	if otherIdx < 0 || zotIdx < 0 {
		t.Fatalf("test docs scrambled: %s", got)
	}
	between := got[otherIdx:zotIdx]
	if strings.Contains(between, "serviceName:") {
		t.Fatalf("serviceName leaked into non-zot StatefulSet:\n%s", between)
	}
}
