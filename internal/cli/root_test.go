package cli

import (
	"bytes"
	"testing"
)

func TestNewRootCmd_HasDeployAndUsesOut(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	root := NewRootCmd(out, errOut)
	if root.Use != "palette-ai-instance" {
		t.Fatalf("unexpected root.Use: %q", root.Use)
	}

	var deployFound bool
	for _, c := range root.Commands() {
		if c.Use == "deploy" {
			deployFound = true
			break
		}
	}
	if !deployFound {
		t.Fatal("deploy subcommand not registered")
	}

	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help should succeed: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected help text on the configured out writer")
	}
}
