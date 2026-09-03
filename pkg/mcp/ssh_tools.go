package mcp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// runCommandKillGrace is how long the guest-side `timeout` waits between
// the SIGTERM it sends at the bound and the SIGKILL that follows
// (`--kill-after`). Without it the "bound" is not one: measured on a real
// grain guest, `timeout 2 ./ignores-sigterm.sh` against a command
// trapping TERM waits for that command to finish of its own accord --
// sixty seconds, for a sixty-second sleep -- and only then reports 124,
// so a run_command whose command ignores SIGTERM held the tool call open
// for as long as it liked with nothing to stop it.
//
// Five seconds is room for a well-behaved process to flush and exit on
// the signal it was sent, and short enough that a badly-behaved one
// cannot spend a meaningful part of a run's budget refusing to.
//
// This is *not* accompanied by `--foreground`, which the fix for this was
// first written as. `--foreground` means what its own documentation says:
// "children of COMMAND will not be timed out". Plain `timeout` runs the
// command in its own process group and signals that whole group, so it
// already reaches everything the command forked -- confirmed on a real
// guest, where `timeout 2 bash -c './gc.sh & sleep 10'` leaves no
// surviving gc.sh -- and it is procgroup.Prepare's guarantee for the
// local transport arriving by a different route. Adding `--foreground`
// would have removed the only process-group discipline this path has.
const runCommandKillGrace = 5 * time.Second

// sshRunCommandGrace is how long past the guest-side bound this side
// waits for the guest to answer at all before abandoning the call. It
// covers the SIGTERM-to-SIGKILL escalation (runCommandKillGrace) plus a
// slow `docker exec`/ssh teardown, and it exists for what the guest-side
// bound structurally cannot cover: the command being bounded says nothing
// about the *call* returning.
//
// The one shape suspected of hitting it -- a command that backgrounds
// something inheriting the channel's stdout, which is enough to hang an
// OpenSSH client waiting for EOF -- turns out not to, measured on a real
// guest: `./bgtest.sh &` returns in about two milliseconds, because the
// Go ssh client "kontur exec" uses returns on the exit-status message
// rather than waiting for the streams to close. So this is a backstop for
// a transport that has stopped answering, not the everyday case.
const sshRunCommandGrace = 30 * time.Second

// remoteRunner is the interface NewSSHSandboxTools' handlers need out of
// their transport -- kept separate from the concrete type that supplies it
// (DockerExecRunner) so a test can supply an in-process double instead of
// exec'ing into a real container (ssh_tools_test.go's localExecRunner);
// docker_exec_runner_test.go is what actually exercises DockerExecRunner's
// own command construction.
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
		Handler: func(ctx context.Context, args map[string]any) Result {
			command, ok := argString(args, "command")
			if !ok || command == "" {
				return Result{Text: "command is required", IsError: true}
			}

			// timeout (milliseconds, matching native Bash's own unit) is
			// applied with the `timeout` coreutil on the remote side,
			// exactly as mcp_server.py's run_command does, rather than
			// bounding the local ssh subprocess: SSH has no per-command
			// timeout of its own to hook, and a remote `timeout` bounds
			// the thing that's actually slow. Applied unconditionally,
			// not only when the caller passes its own "timeout", so an
			// omitted one still gets runCommandTimeout's own default
			// rather than running with no server-side bound at all
			// (bwsalmon/agents#575).
			bound := resolveRunCommandBound(args)
			shell := fmt.Sprintf("cd %s && %s", shellQuote(workspace), command)
			// The trailing `exit $?` is not decoration. bash replaces
			// itself with the last simple command in a -c string, so
			// without it the process the transport is watching *is*
			// `timeout` -- and since `timeout` signals the process group
			// it made itself the leader of, the SIGKILL escalation here
			// kills the very process whose status is the answer. Death
			// by signal is not an exit status either os/exec or an SSH
			// exit-status message can carry (it arrives as -1), so the
			// 137 that says the bound ended this would be lost exactly
			// when it matters. Leaving a shell alive to report brings it
			// back as a status.
			shell = fmt.Sprintf("timeout --kill-after=%ds %d bash -c %s; exit $?",
				int(runCommandKillGrace.Seconds()), bound.seconds(), shellQuote(shell))

			// ...and bounded again from this side, well after the guest's
			// own bound should have ended it. The remote `timeout` bounds
			// the command, not the call: nothing here can make the guest
			// answer, so without this a wedged transport (an ssh channel
			// that never closes, a guest that stops responding) holds the
			// tool call open until the run's own wall clock, and a run
			// only advances a turn when its current call returns.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, bound.d+sshRunCommandGrace)
			defer cancel()
			started := time.Now()

			stdout, stderr, exitCode := runner.Run(ctx, []string{"bash", "-c", shell}, "")

			// `timeout`'s own reserved statuses, which are how the guest
			// reports that the bound, not the command, ended this: 124
			// for the command stopping on the SIGTERM it was sent, 137
			// (128+SIGKILL) for it having to be killed --kill-after
			// later. Both used to arrive as a bare number.
			notice := ""
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded) && time.Since(started) >= bound.d:
				notice = bound.transportStalledNotice()
			case exitCode == 124:
				notice = bound.timedOutNotice()
			case exitCode == 137:
				notice = bound.killedNotice()
			}
			text := formatRunCommandResult(exitCode, stdout, stderr, notice)
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
		Handler: func(ctx context.Context, args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			if fp == "" {
				return Result{Text: "file_path is required", IsError: true}
			}
			content, errResult := sshReadRemote(ctx, runner, fp)
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
		Handler: func(ctx context.Context, args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			content, ok := argString(args, "content")
			if !ok {
				return Result{Text: "content is required", IsError: true}
			}
			if fp == "" {
				return Result{Text: "file_path is required", IsError: true}
			}

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
		Handler: func(ctx context.Context, args map[string]any) Result {
			fp, _ := argString(args, "file_path")
			oldStr, hasOld := argString(args, "old_string")
			newStr, hasNew := argString(args, "new_string")
			if !hasOld || !hasNew {
				return Result{Text: "old_string and new_string are required", IsError: true}
			}
			replaceAll, _ := argBool(args, "replace_all")

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
