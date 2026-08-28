package mcp

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// remoteRunner is the interface NewSSHSandboxTools' handlers need out of
// SSHRunner -- kept separate from that concrete type so a test can supply
// an in-process double instead of shelling out to a real ssh binary (see
// ssh_tools_test.go); ssh_runner_test.go is what actually exercises
// SSHRunner's own command construction.
type remoteRunner interface {
	Run(ctx context.Context, argv []string, stdin string) (stdout, stderr string, exitCode int)
}

// NewSSHSandboxTools returns the same four tools NewSandboxTools does --
// run_command, read_file, edit_file, write_file, same names, same input
// schemas, same output shapes -- but each one runs its work on runner's
// remote host instead of a local directory, which is what a kontur-managed
// sandbox VM needs (bwsalmon/agents#256): unlike NewSandboxTools' root,
// there is no local filesystem to call os.ReadFile/os.WriteFile against,
// so every read, write and edit here is itself a command sent over runner
// -- ported field for field from run_command/read_file/edit_file/
// write_file in grain/automation/mcp_server.py, which does the same thing
// against v1's SshRunner.
func NewSSHSandboxTools(runner remoteRunner, workspace string) []Tool {
	return []Tool{
		sshRunCommandTool(runner, workspace),
		sshReadFileTool(runner),
		sshEditFileTool(runner),
		sshWriteFileTool(runner),
	}
}

func sshRunCommandTool(runner remoteRunner, workspace string) Tool {
	return Tool{
		Name:        "run_command",
		Description: "Run a shell command in your assigned sandbox workspace.",
		InputSchema: runCommandInputSchema,
		Handler: func(args map[string]any) Result {
			command, ok := argString(args, "command")
			if !ok || command == "" {
				return Result{Text: "command is required", IsError: true}
			}

			// timeout (milliseconds, matching native Bash's own unit) is
			// applied with the `timeout` coreutil on the remote side,
			// exactly as mcp_server.py's run_command does, rather than
			// bounding the local ssh subprocess: SSH has no per-command
			// timeout of its own to hook, and a remote `timeout` bounds
			// the thing that's actually slow.
			shell := fmt.Sprintf("cd %s && %s", shellQuote(workspace), command)
			if ms, ok := argFloat(args, "timeout"); ok {
				seconds := int(ms / 1000)
				if seconds < 1 {
					seconds = 1
				}
				if seconds > 600 {
					seconds = 600
				}
				shell = fmt.Sprintf("timeout %d bash -c %s", seconds, shellQuote(shell))
			}

			stdout, stderr, exitCode := runner.Run(context.Background(), []string{"bash", "-c", shell}, "")
			text := fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s", exitCode, stdout, stderr)
			return Result{Text: text, IsError: exitCode != 0}
		},
	}
}

// sshReadRemote reads path off runner's remote host with `cat`, the same
// stand-in for a remote os.ReadFile mcp_server.py's own _read_remote uses.
func sshReadRemote(ctx context.Context, runner remoteRunner, path string) (string, *Result) {
	stdout, stderr, exitCode := runner.Run(ctx, []string{"cat", "--", path}, "")
	if exitCode != 0 {
		return "", &Result{Text: fmt.Sprintf("Error reading %s: %s", path, strings.TrimSpace(stderr)), IsError: true}
	}
	return stdout, nil
}

// sshWriteRemote writes content to path on runner's remote host with `dd`,
// piped over stdin rather than embedded in the command line -- content is
// arbitrary model-generated text, and a command-line argument has no safe
// way to carry it whole (length limits, shell interpretation) the way
// stdin does.
func sshWriteRemote(ctx context.Context, runner remoteRunner, path, content string) Result {
	_, stderr, exitCode := runner.Run(ctx, []string{"dd", "of=" + path, "status=none"}, content)
	if exitCode != 0 {
		return Result{Text: fmt.Sprintf("Error writing %s: %s", path, strings.TrimSpace(stderr)), IsError: true}
	}
	return Result{Text: fmt.Sprintf("Wrote %s", path)}
}

func sshReadFileTool(runner remoteRunner) Tool {
	return Tool{
		Name:        "read_file",
		Description: "Read a file from your assigned sandbox workspace.",
		InputSchema: readFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			if fp == "" {
				return Result{Text: "file_path is required", IsError: true}
			}
			content, errResult := sshReadRemote(context.Background(), runner, fp)
			if errResult != nil {
				return *errResult
			}
			return Result{Text: numberedRange(linesFromContent(content), args)}
		},
	}
}

func sshWriteFileTool(runner remoteRunner) Tool {
	return Tool{
		Name:        "write_file",
		Description: "Write (create or overwrite) a file in your assigned sandbox workspace.",
		InputSchema: writeFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			content, ok := argString(args, "content")
			if !ok {
				return Result{Text: "content is required", IsError: true}
			}
			if fp == "" {
				return Result{Text: "file_path is required", IsError: true}
			}

			ctx := context.Background()
			parent := path.Dir(fp)
			_, stderr, exitCode := runner.Run(ctx, []string{"mkdir", "-p", parent}, "")
			if exitCode != 0 {
				return Result{Text: fmt.Sprintf("Error creating %s: %s", parent, strings.TrimSpace(stderr)), IsError: true}
			}
			return sshWriteRemote(ctx, runner, fp, content)
		},
	}
}

func sshEditFileTool(runner remoteRunner) Tool {
	return Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a file in your assigned sandbox workspace.",
		InputSchema: editFileInputSchema,
		Handler: func(args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			oldStr, hasOld := argString(args, "old_string")
			newStr, hasNew := argString(args, "new_string")
			if !hasOld || !hasNew {
				return Result{Text: "old_string and new_string are required", IsError: true}
			}
			replaceAll, _ := argBool(args, "replace_all")

			ctx := context.Background()
			content, errResult := sshReadRemote(ctx, runner, fp)
			if errResult != nil {
				return *errResult
			}
			count := strings.Count(content, oldStr)
			if count == 0 {
				return Result{Text: fmt.Sprintf("String not found in file: %q", oldStr), IsError: true}
			}
			if count > 1 && !replaceAll {
				return Result{
					Text: fmt.Sprintf(
						"String appears %d times in the file. Use replace_all: true to replace "+
							"every occurrence, or provide more surrounding context to uniquely "+
							"identify the instance you mean.", count),
					IsError: true,
				}
			}
			n := 1
			if replaceAll {
				n = -1
			}
			newContent := strings.Replace(content, oldStr, newStr, n)
			return sshWriteRemote(ctx, runner, fp, newContent)
		},
	}
}
