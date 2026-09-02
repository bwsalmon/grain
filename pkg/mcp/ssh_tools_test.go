package mcp

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// localExecRunner implements remoteRunner by running argv on this machine
// instead of inside a sandbox guest -- exactly the same coreutils
// (cat/dd/mkdir/bash) ssh_tools.go's handlers send a real
// DockerExecRunner, just executed locally against a temp directory
// standing in for "the remote host's filesystem". DockerExecRunner's own
// command construction (flags, quoting, stdin plumbing) is covered
// separately in docker_exec_runner_test.go; this is
// what proves NewSSHSandboxTools sends the *right* remote commands to
// implement run_command/read_file/edit_file/write_file, the same
// coverage newTestClient's local NewSandboxTools gets in server_test.go.
//
// dir stands in for the directory a real SSH session starts in (normally
// the login user's $HOME) -- read_file/write_file/edit_file's relative
// file_paths resolve against it exactly the way mcp_server.py's own
// read_file/write_file/edit_file rely on the SSH session's own default
// directory rather than cd-ing anywhere themselves (only run_command
// does, via workspace).
type localExecRunner struct {
	dir string
}

func (r localExecRunner) Run(ctx context.Context, argv []string, stdin string) (stdout, stderr string, exitCode int) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return outBuf.String(), errBuf.String(), exitErr.ExitCode()
		}
		return outBuf.String(), err.Error(), -1
	}
	return outBuf.String(), errBuf.String(), 0
}

func newSSHTestClient(t *testing.T, workspace string) *Client {
	t.Helper()
	registry := NewRegistry()
	registry.Register(NewSSHSandboxTools(localExecRunner{dir: workspace}, workspace)...)
	client := NewInProcess(context.Background(), registry)
	t.Cleanup(func() { client.Close() })
	return client
}

func TestSSHSandboxToolsAdvertiseSameFourNamesAsLocal(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run_command", "read_file", "edit_file", "write_file"}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d: %+v", len(tools), len(want), tools)
	}
	for i, name := range want {
		if tools[i].Name != name {
			t.Errorf("tool %d: got %q, want %q", i, tools[i].Name, name)
		}
	}
}

func TestSSHRunCommandRunsInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	client := newSSHTestClient(t, workspace)

	res, err := client.CallTool(context.Background(), "run_command", map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run_command reported error: %s", res.Text())
	}
	if !strings.Contains(res.Text(), workspace) {
		t.Errorf("run_command output %q does not mention workspace %q", res.Text(), workspace)
	}
}

func TestSSHRunCommandReportsNonZeroExit(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())

	res, err := client.CallTool(context.Background(), "run_command", map[string]any{"command": "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("run_command with exit 3: want IsError true")
	}
	if !strings.Contains(res.Text(), "exit=3") {
		t.Errorf("run_command output %q does not report exit=3", res.Text())
	}
}

// TestSSHRunCommandAppliesDefaultTimeoutWhenNoneGiven is ssh_tools.go's
// side of bwsalmon/agents#575's regression coverage: unlike the local
// run_command (TestRunCommandAppliesDefaultTimeoutWhenNoneGiven), this
// path enforces its timeout with the remote `timeout` coreutil rather
// than ctx cancellation, so it needs its own proof that omitting
// "timeout" still gets wrapped in one rather than running unbounded on
// the remote side.
func TestSSHRunCommandAppliesDefaultTimeoutWhenNoneGiven(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not installed")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout not installed")
	}
	old := defaultRunCommandTimeout
	defaultRunCommandTimeout = time.Second
	t.Cleanup(func() { defaultRunCommandTimeout = old })

	client := newSSHTestClient(t, t.TempDir())
	start := time.Now()
	res, err := client.CallTool(context.Background(), "run_command", map[string]any{"command": "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("run_command with no timeout took %s, want near-instant given the shrunk default, not close to the full 30s sleep", elapsed)
	}
	if !res.IsError {
		t.Error("run_command killed by the default timeout: want IsError true")
	}
}

func TestSSHWriteThenReadFileRoundTrips(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())
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

	res, err = client.CallTool(ctx, "read_file", map[string]any{"file_path": "sub/dir/hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := "     1\tline one\n     2\tline two"
	if res.Text() != want {
		t.Errorf("read_file text = %q, want %q", res.Text(), want)
	}
}

func TestSSHReadFileMissingIsError(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())
	res, err := client.CallTool(context.Background(), "read_file", map[string]any{"file_path": "nope.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("read_file on a missing file: want IsError true")
	}
}

func TestSSHEditFileReplacesUniqueMatch(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "f.txt", "content": "hello world\n",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := client.CallTool(ctx, "edit_file", map[string]any{
		"file_path": "f.txt", "old_string": "world", "new_string": "there",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edit_file reported error: %s", res.Text())
	}

	res, err = client.CallTool(ctx, "read_file", map[string]any{"file_path": "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "     1\thello there"; res.Text() != want {
		t.Errorf("read_file after edit = %q, want %q", res.Text(), want)
	}
}

func TestSSHEditFileAmbiguousMatchRequiresReplaceAll(t *testing.T) {
	client := newSSHTestClient(t, t.TempDir())
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "write_file", map[string]any{
		"file_path": "f.txt", "content": "aa aa aa\n",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := client.CallTool(ctx, "edit_file", map[string]any{
		"file_path": "f.txt", "old_string": "aa", "new_string": "bb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("edit_file with an ambiguous match and no replace_all: want IsError true")
	}

	res, err = client.CallTool(ctx, "edit_file", map[string]any{
		"file_path": "f.txt", "old_string": "aa", "new_string": "bb", "replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edit_file with replace_all reported error: %s", res.Text())
	}

	res, err = client.CallTool(ctx, "read_file", map[string]any{"file_path": "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "     1\tbb bb bb"; res.Text() != want {
		t.Errorf("read_file after replace_all = %q, want %q", res.Text(), want)
	}
}
