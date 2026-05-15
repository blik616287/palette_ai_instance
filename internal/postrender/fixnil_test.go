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

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// The AST walker locates the zot StatefulSet structurally, so labels and
// annotations appearing before metadata.name no longer fool it (the old
// 10-line regex lookahead did break on this).
func TestFixNilValues_InjectsZotServiceName_NameAfterLabels(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  labels:
    app.kubernetes.io/name: zot
    app.kubernetes.io/instance: mural
  annotations:
    helm.sh/hook-weight: "0"
  name: zot
spec:
  replicas: 1
`
	got := run(t, in)
	if !strings.Contains(got, "serviceName: zot") {
		t.Fatalf("serviceName not injected when name follows labels/annotations:\n%s", got)
	}
}

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// A StatefulSet with no `replicas` key (valid — defaults to 1) still gets
// serviceName; the old regex keyed the insert point on `replicas:` and would
// silently skip such a manifest.
func TestFixNilValues_InjectsZotServiceName_NoReplicasKey(t *testing.T) {
	in := `
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: zot
spec:
  selector:
    matchLabels:
      app: zot
`
	got := run(t, in)
	if !strings.Contains(got, "serviceName: zot") {
		t.Fatalf("serviceName not injected into replicas-less StatefulSet:\n%s", got)
	}
}

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// Malformed YAML must surface as an error, not a silent pass-through.
func TestFixNilValues_RejectsMalformedYAML(t *testing.T) {
	_, err := postrender.FixNilValues{}.Run(bytes.NewBufferString("key: [unterminated\n"))
	if err == nil {
		t.Fatal("expected error on malformed YAML input")
	}
}

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// A stray empty document (bare `---`) between real docs is dropped, not
// re-emitted as a null.
func TestFixNilValues_SkipsEmptyDocuments(t *testing.T) {
	in := `
---
kind: ConfigMap
metadata:
  name: a
---
---
kind: ConfigMap
metadata:
  name: b
`
	got := run(t, in)
	if !strings.Contains(got, "name: a") || !strings.Contains(got, "name: b") {
		t.Fatalf("real docs lost:\n%s", got)
	}
	if strings.Contains(got, "null") {
		t.Fatalf("empty doc re-emitted as null:\n%s", got)
	}
}

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// A non-StatefulSet document (and a StatefulSet whose spec is a scalar) must
// pass through untouched without panicking the structural walk.
func TestFixNilValues_NonStatefulSetAndMalformedSpecPassThrough(t *testing.T) {
	in := `
kind: Service
metadata:
  name: zot
spec:
  type: ClusterIP
`
	got := run(t, in)
	if strings.Contains(got, "serviceName: zot") {
		t.Fatalf("serviceName injected into a Service:\n%s", got)
	}
	if !strings.Contains(got, "type: ClusterIP") {
		t.Fatalf("service spec lost:\n%s", got)
	}
}

// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// Explicit `null` / `~` values for the nil-fields are quoted too, not just
// the bare `key:` form.
func TestFixNilValues_QuotesExplicitNullForms(t *testing.T) {
	in := `
stringData:
  certSecretName: null
  proxySecretName: ~
`
	got := run(t, in)
	if !strings.Contains(got, `certSecretName: ""`) || !strings.Contains(got, `proxySecretName: ""`) {
		t.Fatalf("explicit null forms not quoted:\n%s", got)
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
