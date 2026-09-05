package mcp

// ConfigureGitCredentialsOverSSH is ConfigureGitCredentials for a
// KonturSandboxes-style remote VM instead of a local directory: unlike
// NewSandboxTools' root (a plain directory HOME gets pinned to,
// runCommandTool), NewSSHSandboxTools has no filesystem of its own to
// write into -- SSH, over runner, is the only path to the remote host's
// filesystem at all, the same reason ssh_tools.go's own read/write/edit
// handlers run `cat`/`dd`/`mkdir` remotely instead of calling os.*
// directly. The two files are written with relative paths, the same way
// read_file/write_file/edit_file's own file_path already does (ssh_tools_
// test.go's own doc comment: "the directory a real SSH session starts in
// (normally the login user's $HOME)") -- so they land in whatever
// directory sshd itself chdirs a fresh session into, without this package
// needing to name or expand $HOME itself.

import (
	"context"
	"fmt"
)

// ConfigureGitCredentialsOverSSH writes .gitconfig and .git-credentials
// into runner's remote SSH session's starting directory so that any git
// command a later run_command call executes there authenticates through
// remoteURL's host as sandbox, using token as the password half of HTTP
// Basic auth -- the same content ConfigureGitCredentials writes locally,
// just placed over runner (sshWriteRemote, ssh_tools.go) instead of
// os.WriteFile.
func ConfigureGitCredentialsOverSSH(runner remoteRunner, remoteURL, token string, identity GitIdentity) error {
	credentials, gitconfig, err := gitCredentialFiles(remoteURL, token, identity)
	if err != nil {
		return err
	}

	ctx := context.Background()
	for _, f := range []struct{ path, content string }{
		{".git-credentials", credentials},
		{".gitconfig", gitconfig},
	} {
		if result := sshWriteRemote(ctx, runner, f.path, f.content); result.IsError {
			return fmt.Errorf("gitcredentials: writing remote %s: %s", f.path, result.Text)
		}
		if _, stderr, exitCode := runner.Run(ctx, []string{"chmod", "600", "--", f.path}, ""); exitCode != 0 {
			return fmt.Errorf("gitcredentials: chmod remote %s: %s", f.path, stderr)
		}
	}
	return nil
}
