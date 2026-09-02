package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFakeDockerExec installs a shell script named "docker" on PATH (a
// temp directory prepended ahead of the real PATH, restored on cleanup)
// that records each of its own arguments on its own line and then behaves
// as the script body given -- enough to prove DockerExecRunner's own
// invocation and exit-code handling without a docker daemon anywhere.
func writeFakeDockerExec(t *testing.T, body string) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker script is POSIX shell only")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
: > %q
for arg in "$@"; do
  printf '%%s\n' "$arg" >> %q
done
%s
`, argsFile, argsFile, body)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func readArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// The guest command has to arrive as a real argv after "kontur exec --",
// not shell-quoted into one string the way an `ssh host <command>` call
// would have to quote it: "kontur exec" does that join itself (see Run's
// own doc comment), so quoting here too would double-quote every argument
// that needed it.
func TestDockerExecRunnerPassesArgvThroughUnquoted(t *testing.T) {
	argsFile := writeFakeDockerExec(t, "exit 0")

	r := &DockerExecRunner{
		Container:      "kontur-vm-sandbox-1",
		User:           "debian",
		KeyPath:        "/images/kontur_id_ed25519",
		ConnectTimeout: 90 * time.Second,
	}
	if _, _, code := r.Run(context.Background(), []string{"sh", "-c", "echo hello world"}, ""); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	got := readArgs(t, argsFile)
	want := []string{
		"exec",
		"-e", "KONTUR_EXEC_USER=debian",
		"-e", "KONTUR_EXEC_KEY=/images/kontur_id_ed25519",
		"-e", "KONTUR_EXEC_CONNECT_TIMEOUT=1m30s",
		"kontur-vm-sandbox-1", "kontur", "exec", "--",
		"sh", "-c", "echo hello world",
	}
	if len(got) != len(want) {
		t.Fatalf("docker args = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("docker arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Every one of the three -e flags is omitted when its field is unset, so
// an unset field leaves guestexec's own default in place rather than
// overriding it with an empty value (KONTUR_EXEC_USER="" would not fall
// back to "root" -- guestexec's getEnvDefault treats set-but-empty the
// same as unset, but only because it checks for it; passing nothing is
// what actually expresses "leave the default alone").
func TestDockerExecRunnerOmitsUnsetEnvFlags(t *testing.T) {
	argsFile := writeFakeDockerExec(t, "exit 0")

	r := &DockerExecRunner{Container: "kontur-vm-sandbox-1"}
	r.Run(context.Background(), []string{"true"}, "")

	got := strings.Join(readArgs(t, argsFile), " ")
	if strings.Contains(got, "KONTUR_EXEC_") {
		t.Errorf("docker args = %q, want no KONTUR_EXEC_* flags for a zero-valued runner", got)
	}
	if want := "exec kontur-vm-sandbox-1 kontur exec -- true"; got != want {
		t.Errorf("docker args = %q, want %q", got, want)
	}
}

// -i is what gives the exec'd process a stdin to read at all, so it has
// to be there exactly when there is stdin to pipe -- and not otherwise.
func TestDockerExecRunnerPassesStdinOnlyWhenThereIsSome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin string
		wantI bool
	}{
		{name: "with stdin", stdin: "payload", wantI: true},
		{name: "without stdin", stdin: "", wantI: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := writeFakeDockerExec(t, "cat")

			r := &DockerExecRunner{Container: "kontur-vm-sandbox-1"}
			stdout, _, code := r.Run(context.Background(), []string{"cat"}, tc.stdin)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if gotI := readArgs(t, argsFile)[1] == "-i"; gotI != tc.wantI {
				t.Errorf("-i present = %v, want %v (args %q)", gotI, tc.wantI, readArgs(t, argsFile))
			}
			if stdout != tc.stdin {
				t.Errorf("stdout = %q, want the piped stdin %q", stdout, tc.stdin)
			}
		})
	}
}

// A guest command's own non-zero exit has to arrive intact rather than
// being flattened into the -1 that means "it never ran" -- including 1
// itself, which is exactly the status both of the pre-guest failures
// below also exit with.
func TestDockerExecRunnerReportsTheGuestCommandsOwnExitCode(t *testing.T) {
	for _, code := range []int{1, 2, 127} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			writeFakeDockerExec(t, fmt.Sprintf("echo 'no such file' >&2; exit %d", code))

			r := &DockerExecRunner{Container: "kontur-vm-sandbox-1"}
			_, stderr, got := r.Run(context.Background(), []string{"false"}, "")
			if got != code {
				t.Errorf("exit code = %d, want %d (stderr %q)", got, code, stderr)
			}
		})
	}
}

// The two failures that happen before the guest command ever runs both
// exit 1, so only their stderr tells them apart from a guest command that
// exited 1 on its own -- and both have to come back as -1, the report Run
// documents for a guest command that never ran at all.
func TestDockerExecRunnerReportsMinusOneWhenTheGuestCommandNeverRan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
	}{
		{
			name:   "kontur could not reach the guest",
			stderr: "kontur: exec: dialing 169.254.100.2:22: connection refused",
		},
		{
			name:   "no such container",
			stderr: "Error: No such container: kontur-vm-sandbox-1",
		},
		{
			name:   "docker daemon not answering",
			stderr: "Error response from daemon: dial unix /var/run/docker.sock: connect: no such file or directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFakeDockerExec(t, fmt.Sprintf("printf '%%s\\n' %q >&2; exit 1", tc.stderr))

			r := &DockerExecRunner{Container: "kontur-vm-sandbox-1"}
			_, stderr, code := r.Run(context.Background(), []string{"true"}, "")
			if code != -1 {
				t.Errorf("exit code = %d, want -1", code)
			}
			if !strings.Contains(stderr, tc.stderr) {
				t.Errorf("stderr = %q, want it to carry %q", stderr, tc.stderr)
			}
		})
	}
}

// DockerExecRunner has to satisfy the same interface NewSSHSandboxTools
// and ConfigureGitCredentialsOverSSH take, since serving those two call
// sites is the whole point of it.
func TestDockerExecRunnerIsARemoteRunner(t *testing.T) {
	var _ remoteRunner = &DockerExecRunner{}
}
