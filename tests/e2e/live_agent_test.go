package e2e

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// buildGrainBinary compiles cmd/grain into t.TempDir() so a live run's
// agy can fork its "mcpserver" subcommand to reach the sandbox -- the
// same binary a real deployment builds once and reuses across every
// dispatch.
//
// Only the live tests in this package need it. A scripted run
// (antigravity.NewForTest) stands up the same MCP tools in-process and
// forks nothing, which is why the rest of this suite needs no binary on
// disk at all.
func buildGrainBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "grain")
	cmd := exec.Command("go", "build", "-o", out, "github.com/bwsalmon/grain/cmd/grain")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the grain binary for agy's MCP server: %v\n%s", err, output)
	}
	return out
}
