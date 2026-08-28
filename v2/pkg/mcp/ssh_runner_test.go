package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeSSH installs a shell script named "ssh" on PATH (a temp
// directory prepended ahead of the real PATH, restored on cleanup) that
// prints each of its own arguments on its own line to stderr (so it's
// visible however the real ssh's stdout/stderr get captured) and then, if
// -- is followed by a remote command, execs that command through the
// local shell -- close enough to a real remote shell's own re-parsing of
// OpenSSH's unquoted rejoin (see SSHRunner's doc comment) to prove
// shellJoin/shellQuote round-trip correctly, without a real sshd anywhere.
func writeFakeSSH(t *testing.T) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script is POSIX shell only")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
: > %q
remote=""
after_dashdash=0
for arg in "$@"; do
  printf '%%s\n' "$arg" >> %q
  if [ "$after_dashdash" = "1" ]; then
    remote="$arg"
  fi
  if [ "$arg" = "--" ]; then
    after_dashdash=1
  fi
done
sh -c "$remote"
`, argsFile, argsFile)
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestSSHRunnerBuildsExpectedFlags(t *testing.T) {
	argsFile := writeFakeSSH(t)
	runner := &SSHRunner{User: "debian", Host: "10.100.5.7", Port: 30080, KeyPath: "/home/debian/.ssh/id_ed25519"}

	stdout, stderr, exitCode := runner.Run(context.Background(), []string{"echo", "hi there"}, "")
	if exitCode != 0 {
		t.Fatalf("Run() exitCode = %d, stderr = %q", exitCode, stderr)
	}
	if strings.TrimSpace(stdout) != "hi there" {
		t.Errorf("Run() stdout = %q, want %q", stdout, "hi there")
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	wantContains := [][]string{
		{"-i", "/home/debian/.ssh/id_ed25519"},
		{"-p", "30080"},
		{"-o", "BatchMode=yes"},
		{"-o", "StrictHostKeyChecking=accept-new"},
		{"-o", "UserKnownHostsFile=/dev/null"},
		{"-o", "IdentityAgent=none"},
	}
	for _, pair := range wantContains {
		if !containsAdjacentPair(args, pair[0], pair[1]) {
			t.Errorf("Run() args %v missing adjacent pair %v", args, pair)
		}
	}
	if !contains(args, "debian@10.100.5.7") {
		t.Errorf("Run() args %v missing user@host", args)
	}
	if last := args[len(args)-1]; last != `echo 'hi there'` {
		t.Errorf("Run() remote command = %q, want %q", last, `echo 'hi there'`)
	}
}

func TestSSHRunnerOmitsPortFlagWhenZero(t *testing.T) {
	argsFile := writeFakeSSH(t)
	runner := &SSHRunner{User: "debian", Host: "10.100.5.7", KeyPath: "/key"}

	if _, _, exitCode := runner.Run(context.Background(), []string{"true"}, ""); exitCode != 0 {
		t.Fatalf("Run() exitCode = %d", exitCode)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if contains(args, "-p") {
		t.Errorf("Run() args %v should not contain -p when Port is 0", args)
	}
}

func TestSSHRunnerRoundTripsArgvWithSpecialCharacters(t *testing.T) {
	writeFakeSSH(t)
	runner := &SSHRunner{User: "debian", Host: "10.100.5.7", KeyPath: "/key"}

	argv := []string{"bash", "-c", `echo "it's a test" && printf 'a\tb\n'`}
	stdout, stderr, exitCode := runner.Run(context.Background(), argv, "")
	if exitCode != 0 {
		t.Fatalf("Run() exitCode = %d, stderr = %q", exitCode, stderr)
	}
	if want := "it's a test\na\tb\n"; stdout != want {
		t.Errorf("Run() stdout = %q, want %q", stdout, want)
	}
}

func TestSSHRunnerPipesStdin(t *testing.T) {
	writeFakeSSH(t)
	runner := &SSHRunner{User: "debian", Host: "10.100.5.7", KeyPath: "/key"}

	stdout, stderr, exitCode := runner.Run(context.Background(), []string{"cat"}, "hello from stdin")
	if exitCode != 0 {
		t.Fatalf("Run() exitCode = %d, stderr = %q", exitCode, stderr)
	}
	if stdout != "hello from stdin" {
		t.Errorf("Run() stdout = %q, want %q", stdout, "hello from stdin")
	}
}

func TestSSHRunnerReportsNonZeroExit(t *testing.T) {
	writeFakeSSH(t)
	runner := &SSHRunner{User: "debian", Host: "10.100.5.7", KeyPath: "/key"}

	_, _, exitCode := runner.Run(context.Background(), []string{"bash", "-c", "exit 7"}, "")
	if exitCode != 7 {
		t.Errorf("Run() exitCode = %d, want 7", exitCode)
	}
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}

func containsAdjacentPair(items []string, a, b string) bool {
	for i := 0; i+1 < len(items); i++ {
		if items[i] == a && items[i+1] == b {
			return true
		}
	}
	return false
}
