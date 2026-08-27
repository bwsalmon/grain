package mcp

// ConfigureGitCredentials points a sandbox's git at its proxy token via a
// credential helper, so neither the clone URL nor any run_command call
// ever needs to carry it -- docs/design.md: "git consumes it via a
// credential helper, so agents never handle it." Same idea as
// grain/automation/dispatch.py's function of the same name, adapted to
// v2's stand-in for a sandbox: a plain directory (NewSandboxTools' root)
// rather than a whole VM, so "the sandbox's global git config" becomes
// root/.gitconfig with $HOME pinned to root (runCommandTool does that),
// instead of one real user's actual home directory.
//
// Also sets a fixed git identity (user.name/user.email): a fresh sandbox
// has none configured anywhere, which makes `git commit` fail outright.

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

const (
	gitIdentityName  = "grain agent"
	gitIdentityEmail = "grain-agent@localhost"
)

// ConfigureGitCredentials writes root/.gitconfig and root/.git-credentials
// so that any git command run_command later executes with HOME=root
// (see runCommandTool) authenticates through remoteURL's host as sandbox,
// using token as the password half of HTTP Basic auth.
func ConfigureGitCredentials(root, remoteURL, token string) error {
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return fmt.Errorf("gitcredentials: parsing %q: %w", remoteURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("gitcredentials: %q is not an absolute URL", remoteURL)
	}

	// git-credential-store matches on protocol+host, not path, so one
	// line covers every repo this sandbox might be pointed at through the
	// same proxy.
	line := fmt.Sprintf("%s://sandbox:%s@%s\n", parsed.Scheme, token, parsed.Host)
	credentialsPath := filepath.Join(root, ".git-credentials")
	if err := os.WriteFile(credentialsPath, []byte(line), 0o600); err != nil {
		return fmt.Errorf("gitcredentials: writing %s: %w", credentialsPath, err)
	}

	gitconfig := fmt.Sprintf(
		"[credential]\n\thelper = store\n[user]\n\tname = %s\n\temail = %s\n",
		gitIdentityName, gitIdentityEmail,
	)
	gitconfigPath := filepath.Join(root, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte(gitconfig), 0o600); err != nil {
		return fmt.Errorf("gitcredentials: writing %s: %w", gitconfigPath, err)
	}
	return nil
}
