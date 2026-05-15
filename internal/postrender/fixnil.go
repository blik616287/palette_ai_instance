// Package postrender holds helm post-renderers — small filters that transform
// the rendered chart manifests before they are sent to the cluster. Today the
// only one is FixNilValues, which works around two known bugs in the mural
// chart that the Ansible playbook handles via a Python script. See
// ansible/paletteai/fix-nil-values.sh.
package postrender

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// FixNilValues patches rendered mural manifests in flight:
//
//  1. Empty `stringData` fields (certSecretName / proxySecretName /
//     basicAuthSecretName) render as YAML nil which the Kubernetes API
//     server rejects. We re-quote them as empty strings.
//  2. The zot StatefulSet omits the required `serviceName` field when
//     serviceHeadless is disabled. We inject `serviceName: zot`.
//
// Closes L2 — see reviews/2026-05-15T195833Z-review.md#l2.
// This is a YAML-AST walker (gopkg.in/yaml.v3 in Node mode). The previous
// implementation was line-by-line regex matching, which broke if the chart
// reordered metadata keys (e.g. labels/annotations before `name`) or dropped
// the `replicas` field the scan keyed on. The AST walk locates resources
// structurally instead. Node mode preserves comments, key order, and scalar
// styles, so re-encoding is faithful — only the targeted fields change.
type FixNilValues struct{}

// emptyNilFields are the keys whose null value the API server rejects. The
// fixer rewrites any of these to an empty string wherever it appears with a
// null value (mirrors the original regex, which was not scoped to a parent).
var emptyNilFields = map[string]struct{}{
	"certSecretName":      {},
	"proxySecretName":     {},
	"basicAuthSecretName": {},
}

// Run implements helm's postrender.PostRenderer.
func (FixNilValues) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	// Decode every document up front so the empty-stream case can return
	// without ever touching the encoder (Close on an unused encoder errors
	// with "expected STREAM-START").
	dec := yaml.NewDecoder(bytes.NewReader(in.Bytes()))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("postrender: parse rendered manifest stream: %w", err)
		}
		if doc.Kind == 0 {
			// Empty document (e.g. a stray `---` from a template that
			// rendered nothing). Skip without emitting.
			continue
		}
		quoteEmptyNilFields(&doc)
		injectZotServiceName(&doc)
		d := doc
		docs = append(docs, &d)
	}
	if len(docs) == 0 {
		// Preserve the empty-in → empty-out contract.
		return bytes.NewBuffer(nil), nil
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2) // k8s manifest convention; also keeps output diff-friendly
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("postrender: re-encode manifest: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("postrender: flush encoder: %w", err)
	}
	return &out, nil
}

// quoteEmptyNilFields walks the node tree and rewrites any mapping value whose
// key is one of emptyNilFields and whose value is YAML null into an explicit
// empty string. Recurses through documents, sequences, and mappings.
func quoteEmptyNilFields(n *yaml.Node) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			quoteEmptyNilFields(c)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if _, want := emptyNilFields[key.Value]; want && isNull(val) {
				val.Kind = yaml.ScalarNode
				val.Tag = "!!str"
				val.Value = ""
				val.Style = yaml.DoubleQuotedStyle
			}
			quoteEmptyNilFields(val)
		}
	}
}

// isNull reports whether n is a YAML null scalar (`key:`, `~`, or `null`).
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// injectZotServiceName adds `serviceName: zot` to the zot StatefulSet's spec
// when it's missing. It locates the resource structurally — kind == StatefulSet
// AND metadata.name == zot — so reordered or extra metadata keys don't fool it.
func injectZotServiceName(doc *yaml.Node) {
	root := doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return
	}
	if kind := mapValue(root, "kind"); kind == nil || kind.Value != "StatefulSet" {
		return
	}
	if name := mapValue(mapValue(root, "metadata"), "name"); name == nil || name.Value != "zot" {
		return
	}
	spec := mapValue(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return
	}
	if mapValue(spec, "serviceName") != nil {
		return // already set — leave it
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "serviceName"}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "zot"}

	// Insert immediately before `replicas` to match the field ordering the
	// original fix-nil-values.sh produced; append if there's no replicas key.
	insertAt := len(spec.Content)
	for i := 0; i+1 < len(spec.Content); i += 2 {
		if spec.Content[i].Value == "replicas" {
			insertAt = i
			break
		}
	}
	merged := make([]*yaml.Node, 0, len(spec.Content)+2)
	merged = append(merged, spec.Content[:insertAt]...)
	merged = append(merged, keyNode, valNode)
	merged = append(merged, spec.Content[insertAt:]...)
	spec.Content = merged
}

// mapValue returns the value node for key in a mapping node, or nil when the
// node isn't a mapping or the key is absent.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
