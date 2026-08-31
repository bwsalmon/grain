package selfdebug

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestSpecIsAGrantCapabilityNamedSelfDebug(t *testing.T) {
	spec := New().Spec()
	if CapabilityName != "self-debug" || spec.Name != CapabilityName {
		t.Fatalf("Name = %q, want %q", spec.Name, "self-debug")
	}
	if spec.Provision != model.ProvisionGrant {
		t.Fatalf("Provision = %q, want %q", spec.Provision, model.ProvisionGrant)
	}
}

func TestResolveAlwaysHonours(t *testing.T) {
	res, err := New().Resolve(context.Background(), model.CapabilityContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Refused {
		t.Fatalf("Resolve refused: %+v", res)
	}
}

func setupTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func callTool(t *testing.T, tools []mcp.Tool, name string, args map[string]any) mcp.Result {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Handler(context.Background(), args)
		}
	}
	t.Fatalf("no tool named %q", name)
	return mcp.Result{}
}

func TestReadGrainSourceReadsAFile(t *testing.T) {
	dir := setupTree(t)
	tools := SourceTools(dir)
	res := callTool(t, tools, "read_grain_source", map[string]any{"file_path": "main.go"})
	if res.IsError {
		t.Fatalf("read_grain_source errored: %s", res.Text)
	}
	if strings.TrimSpace(res.Text) != "package main" {
		t.Fatalf("Text = %q, want %q", res.Text, "package main")
	}
}

func TestReadGrainSourceRejectsEscapingPaths(t *testing.T) {
	dir := setupTree(t)
	tools := SourceTools(dir)
	res := callTool(t, tools, "read_grain_source", map[string]any{"file_path": "../../etc/passwd"})
	if !res.IsError {
		t.Fatalf("expected an error for an escaping path, got: %s", res.Text)
	}
}

func TestListGrainSourceListsEntries(t *testing.T) {
	dir := setupTree(t)
	tools := SourceTools(dir)
	res := callTool(t, tools, "list_grain_source", map[string]any{})
	if res.IsError {
		t.Fatalf("list_grain_source errored: %s", res.Text)
	}
	if !strings.Contains(res.Text, "main.go") || !strings.Contains(res.Text, "sub/") {
		t.Fatalf("Text = %q, want it to list main.go and sub/", res.Text)
	}
}

func TestListGrainSourceRejectsEscapingPaths(t *testing.T) {
	dir := setupTree(t)
	tools := SourceTools(dir)
	res := callTool(t, tools, "list_grain_source", map[string]any{"dir_path": ".."})
	if !res.IsError {
		t.Fatalf("expected an error for an escaping path, got: %s", res.Text)
	}
}
