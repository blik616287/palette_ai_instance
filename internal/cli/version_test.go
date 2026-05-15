// Closes M5 — see reviews/2026-05-15T195833Z-review.md#m5.
package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRegisterVersion_Subcommand(t *testing.T) {
	root := &cobra.Command{Use: "palette-ai-instance"}
	RegisterVersion(root, VersionInfo{
		Version:   "v9.9.9",
		GitSHA:    "deadbeef",
		BuildTime: "2026-05-15T19:58:33Z",
	})

	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute version: %v", err)
	}
	got := out.String()
	for _, want := range []string{"v9.9.9", "deadbeef", "2026-05-15T19:58:33Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected version output to contain %q, got:\n%s", want, got)
		}
	}
}

func TestRegisterVersion_RootFlagAndTemplate(t *testing.T) {
	root := &cobra.Command{Use: "palette-ai-instance"}
	RegisterVersion(root, VersionInfo{Version: "v1", GitSHA: "abc", BuildTime: "t"})
	if !strings.Contains(root.Version, "v1") {
		t.Fatalf("Version field not set: %q", root.Version)
	}
	// --version flag should print the template, not error.
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--version Execute: %v", err)
	}
	if !strings.Contains(out.String(), "palette-ai-instance") {
		t.Fatalf("template didn't render: %q", out.String())
	}
}
