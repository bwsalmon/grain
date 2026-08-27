package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newTestClient(t *testing.T, root string) *Client {
	t.Helper()
	registry := NewRegistry()
	registry.Register(NewSandboxTools(root)...)
	sink := &MockSink{}
	registry.Register(NewMockTools(sink)...)
	client := NewInProcess(registry)
	t.Cleanup(func() { client.Close() })
	return client
}

func TestListToolsAdvertisesAllEightInOrder(t *testing.T) {
	client := newTestClient(t, t.TempDir())
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run_command", "read_file", "edit_file", "write_file",
		"ask_question", "comment_on_issue", "propose_task", "add_review_comment",
	}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d: %+v", len(tools), len(want), tools)
	}
	for i, name := range want {
		if tools[i].Name != name {
			t.Errorf("tool %d: got %q, want %q", i, tools[i].Name, name)
		}
		if tools[i].Description == "" {
			t.Errorf("tool %q: empty description", name)
		}
	}
}

func TestWriteThenReadFileRoundTrips(t *testing.T) {
	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	res, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "sub/dir/hello.txt",
		"content":   "line one\nline two\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("write_file reported error: %s", res.Text())
	}

	on := filepath.Join(root, "sub", "dir", "hello.txt")
	if _, err := os.Stat(on); err != nil {
		t.Fatalf("file was not created at expected path: %v", err)
	}

	res, err = client.CallTool(ctx, "read_file", map[string]any{"file_path": "sub/dir/hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := "     1\tline one\n     2\tline two"
	if res.Text() != want {
		t.Errorf("read_file text = %q, want %q", res.Text(), want)
	}
}

func TestReadFileRespectsOffsetAndLimit(t *testing.T) {
	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "f.txt", "content": "a\nb\nc\nd\n",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := client.CallTool(ctx, "read_file", map[string]any{
		"file_path": "f.txt", "offset": float64(1), "limit": float64(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "     2\tb\n     3\tc"
	if res.Text() != want {
		t.Errorf("got %q, want %q", res.Text(), want)
	}
}

func TestEditFileRequiresUniqueMatchUnlessReplaceAll(t *testing.T) {
	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "f.txt", "content": "x x x",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := client.CallTool(ctx, "edit_file", map[string]any{
		"file_path": "f.txt", "old_string": "x", "new_string": "y",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected ambiguous edit to be an error, got: %s", res.Text())
	}

	res, err = client.CallTool(ctx, "edit_file", map[string]any{
		"file_path": "f.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("replace_all edit failed: %s", res.Text())
	}

	data, err := os.ReadFile(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "y y y" {
		t.Errorf("file content = %q, want %q", string(data), "y y y")
	}
}

func TestSandboxToolsRejectEscapingPaths(t *testing.T) {
	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{outside, "../../etc/passwd", "../escape.txt"}
	for _, fp := range cases {
		res, err := client.CallTool(ctx, "read_file", map[string]any{"file_path": fp})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("read_file(%q): expected escape to be rejected, got: %s", fp, res.Text())
		}
	}
}

func TestRunCommandRunsInsideSandboxRoot(t *testing.T) {
	root := t.TempDir()
	client := newTestClient(t, root)
	ctx := context.Background()

	res, err := client.CallTool(ctx, "run_command", map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run_command failed: %s", res.Text())
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "exit=0\nstdout:\n" + realRoot + "\n\nstderr:\n"
	if res.Text() != want {
		t.Errorf("run_command pwd output = %q, want %q", res.Text(), want)
	}
}

func TestMockToolsRecordWithoutAnyNetworkEffect(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewSandboxTools(root)...)
	sink := &MockSink{}
	registry.Register(NewMockTools(sink)...)
	client := NewInProcess(registry)
	defer client.Close()
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "ask_question", map[string]any{"question": "which branch?"}); err != nil {
		t.Fatal(err)
	}
	if got := sink.Question(); got != "which branch?" {
		t.Errorf("Question() = %q", got)
	}

	if _, err := client.CallTool(ctx, "comment_on_issue", map[string]any{"comment": "done"}); err != nil {
		t.Fatal(err)
	}
	if got := sink.Comment(); got != "done" {
		t.Errorf("Comment() = %q", got)
	}

	if _, err := client.CallTool(ctx, "propose_task", map[string]any{
		"title": "follow up", "body": "do the other thing", "id": "a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(ctx, "propose_task", map[string]any{
		"title": "second", "body": "depends on first", "depends_on": []any{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	tasks := sink.ProposedTasks()
	if len(tasks) != 2 {
		t.Fatalf("got %d proposed tasks, want 2", len(tasks))
	}
	if tasks[1].DependsOn[0] != "a" {
		t.Errorf("second task depends_on = %v, want [a]", tasks[1].DependsOn)
	}

	res, err := client.CallTool(ctx, "add_review_comment", map[string]any{"body": "nit", "path": "x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected path-without-line to be rejected, got: %s", res.Text())
	}
	if _, err := client.CallTool(ctx, "add_review_comment", map[string]any{
		"body": "nit", "path": "x.go", "line": float64(12),
	}); err != nil {
		t.Fatal(err)
	}
	comments := sink.ReviewComments()
	if len(comments) != 1 || comments[0].Line != 12 || comments[0].Path != "x.go" {
		t.Errorf("ReviewComments() = %+v", comments)
	}
}

func TestUnknownToolIsAnRPCError(t *testing.T) {
	client := newTestClient(t, t.TempDir())
	_, err := client.CallTool(context.Background(), "does_not_exist", nil)
	if err == nil {
		t.Fatal("expected an error calling an unregistered tool")
	}
}
