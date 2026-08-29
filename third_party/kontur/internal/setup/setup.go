// Package setup drives deploy/static-kubelet/install.sh -- the script that
// turns a bare Debian node into a standalone kubelet running kontur's
// static pods (see that directory's README.md) -- from a single "konturctl
// setup" invocation. install.sh and the config files it installs are
// embedded (see deploy/static-kubelet/assets.go) rather than reimplemented
// here, so this package stays a thin, testable wrapper around logic that's
// already validated on its own, and `konturctl` can set up a node from just
// the compiled binary, without a checkout of this repo alongside it.
package setup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Options configures Install. A zero value runs install.sh with its own
// built-in defaults (KUBELET_VERSION=v1.31.0,
// STATIC_POD_PATH=/etc/kubernetes/manifests).
type Options struct {
	KubeletVersion string
	StaticPodPath  string
}

// Install extracts the embedded static-kubelet assets to a temporary
// directory and runs install.sh from there, streaming its output to
// stdout/stderr. It must run as root, same as install.sh itself. assets is
// normally staticconfig.FS (deploy/static-kubelet/assets.go); tests can
// substitute a fake install.sh to exercise this package without root,
// apt, or systemd.
func Install(ctx context.Context, assets fs.FS, opts Options, stdout, stderr io.Writer) error {
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		return fmt.Errorf("setup must run as root")
	}

	dir, err := os.MkdirTemp("", "kontur-setup-*")
	if err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := extract(assets, dir); err != nil {
		return fmt.Errorf("extracting install assets: %w", err)
	}

	cmd := exec.CommandContext(ctx, "bash", "install.sh")
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if opts.KubeletVersion != "" {
		cmd.Env = append(cmd.Env, "KUBELET_VERSION="+opts.KubeletVersion)
	}
	if opts.StaticPodPath != "" {
		cmd.Env = append(cmd.Env, "STATIC_POD_PATH="+opts.StaticPodPath)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install.sh: %w", err)
	}
	return nil
}

// extract copies every file in assets into dir, preserving relative paths.
// install.sh itself is made executable; everything else (the config files
// it installs verbatim) is written read-only-enough to install, i.e. 0644.
func extract(assets fs.FS, dir string) error {
	return fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if filepath.Base(path) == "install.sh" {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, mode)
	})
}
