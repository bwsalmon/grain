// Package guestkey generates the SSH keypair "kontur exec" authenticates
// to the guest with, once per VM boot, and renders the public half as a
// kernel command line parameter the guest installs at boot.
//
// It replaces a keypair baked into the image at build time (the
// Dockerfile's old exec-keypair stage). That keypair had two problems, and
// publishing images makes both of them real:
//
//   - It was one secret shared by every VM ever booted from a given image
//     build, and it shipped inside the image. Anyone who could pull the
//     image held the key to every guest booted from it.
//   - The public half lives in the guest disk and the private half in the
//     runtime image, so the two only matched when both came out of the
//     same `docker build`. A guest image published separately from the
//     runtime image that boots it silently authorized a key nobody had.
//
// A generated key has neither property: it exists only in the VM's own
// container, only for as long as that guest is booted, and the two halves
// cannot disagree because they are made together.
//
// The public half reaches the guest on the kernel command line, which is
// the only channel that arrives before sshd starts and so before anything
// could authenticate to deliver it another way. That it is world-readable
// inside the guest (/proc/cmdline) costs nothing: it is a public key.
package guestkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	// AuthorizedKeyParam names the command line parameter carrying the
	// base64 of an authorized_keys line. Base64 because a key line
	// contains spaces (type, blob and comment) and the kernel command
	// line is space-separated.
	AuthorizedKeyParam = "kontur.authorized_key"

	// AuthorizedKeyUserParam names the command line parameter carrying
	// one extra account to authorize the key for, alongside root. It
	// exists because the account a caller execs as is the caller's
	// choice (KONTUR_EXEC_USER), while the account the image happens to
	// have created is the image's -- so an image with its own
	// unprivileged account can be reached without the image and the
	// caller having to agree on a name at build time.
	AuthorizedKeyUserParam = "kontur.authorized_key_user"
)

// Generate creates an ed25519 keypair, writes the private half to path in
// OpenSSH format with mode 0600 (creating its directory if needed), and
// returns the public half as an authorized_keys line.
func Generate(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generating ed25519 key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "kontur-exec")
	if err != nil {
		return "", fmt.Errorf("marshaling private key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// O_TRUNC rather than a plain WriteFile so a key left by a previous
	// boot is replaced rather than appended to, and 0600 because
	// golang.org/x/crypto/ssh, like OpenSSH, refuses a group- or
	// world-readable private key.
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshaling public key: %w", err)
	}
	// MarshalAuthorizedKey appends a newline; the caller embeds this in a
	// command line parameter, so trim it.
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), nil
}

// WithParams appends the parameters carrying authorizedKey (and user, if
// non-empty) to cmdline, leaving an existing value for either alone so a
// caller that set one explicitly keeps it -- the same shape as
// netshim.WithIPParam, which composes onto the same command line.
func WithParams(cmdline, authorizedKey, user string) string {
	cmdline = withParam(cmdline, AuthorizedKeyParam,
		base64.StdEncoding.EncodeToString([]byte(authorizedKey)))
	if user != "" {
		cmdline = withParam(cmdline, AuthorizedKeyUserParam, user)
	}
	return cmdline
}

func withParam(cmdline, name, value string) string {
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, name+"=") {
			return cmdline
		}
	}
	param := name + "=" + value
	if cmdline == "" {
		return param
	}
	return cmdline + " " + param
}
