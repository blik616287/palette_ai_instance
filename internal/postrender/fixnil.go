// Package postrender holds helm post-renderers — small filters that transform
// the rendered chart manifests before they are sent to the cluster. Today the
// only one is FixNilValues, which works around two known bugs in the mural
// chart that the Ansible playbook handles via a Python script. See
// ansible/paletteai/fix-nil-values.sh.
package postrender

import (
	"bytes"
	"regexp"
	"strings"
)

// FixNilValues patches rendered mural manifests in flight:
//
//  1. Empty `stringData` fields (certSecretName / proxySecretName /
//     basicAuthSecretName) render as YAML nil which the Kubernetes API
//     server rejects. We re-quote them as empty strings.
//  2. The zot StatefulSet omits the required `serviceName` field when
//     serviceHeadless is disabled. We inject `serviceName: zot` before the
//     `replicas:` key.
type FixNilValues struct{}

// Run implements helm's postrender.PostRenderer.
func (FixNilValues) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	patched := quoteEmptyStringData(in.String())
	patched = injectZotServiceName(patched)
	return bytes.NewBufferString(patched), nil
}

// quoteEmptyStringData rewrites lines that look like `  certSecretName:` (and
// the two sibling fields) into `  certSecretName: ""`. The trailing-space-only
// case is intentional: real values are left alone.
var emptyStringDataField = regexp.MustCompile(
	`(?m)^(\s*(?:certSecretName|proxySecretName|basicAuthSecretName)):[ \t]*$`,
)

func quoteEmptyStringData(content string) string {
	return emptyStringDataField.ReplaceAllString(content, `$1: ""`)
}

// injectZotServiceName scans the rendered YAML for the zot StatefulSet and
// inserts `serviceName: zot` immediately before the `replicas:` line if one
// isn't already present within a small window. Mirrors fix-nil-values.sh
// line-for-line so the Ansible and Go behaviours stay in sync.
func injectZotServiceName(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))

	inZotStatefulSet := false

	for i, line := range lines {
		stripped := strings.TrimSpace(line)
		if stripped == "kind: StatefulSet" {
			inZotStatefulSet = isZotStatefulSet(lines, i)
		}
		if inZotStatefulSet && strings.HasPrefix(stripped, "replicas:") {
			if !hasServiceNameNearby(lines, i) {
				indent := strings.Repeat(" ", leadingSpaces(line))
				out = append(out, indent+"serviceName: zot")
			}
			inZotStatefulSet = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// isZotStatefulSet returns true when the StatefulSet block starting at idx is
// named "zot". The lookahead window matches the python script's 10-line scan.
func isZotStatefulSet(lines []string, idx int) bool {
	end := idx + 10
	if end > len(lines) {
		end = len(lines)
	}
	for j := idx + 1; j < end; j++ {
		s := strings.TrimSpace(lines[j])
		if s == "name: zot" {
			return true
		}
		if strings.HasPrefix(s, "kind:") {
			return false
		}
	}
	return false
}

// hasServiceNameNearby checks a 5-line window around idx for a `serviceName:`
// key — same window as fix-nil-values.sh.
func hasServiceNameNearby(lines []string, idx int) bool {
	start := idx - 5
	if start < 0 {
		start = 0
	}
	end := idx + 5
	if end > len(lines) {
		end = len(lines)
	}
	for k := start; k < end; k++ {
		if strings.Contains(lines[k], "serviceName:") {
			return true
		}
	}
	return false
}

func leadingSpaces(s string) int {
	for i, r := range s {
		if r != ' ' {
			return i
		}
	}
	return len(s)
}
