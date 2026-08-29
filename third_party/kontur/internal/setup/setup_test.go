package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeAssets stands in for staticconfig.FS: a real install.sh would need
// root, apt, and systemd to run to completion, none of which are available
// in a test environment. This fake instead records what it was invoked
// with (working directory contents and environment) so Install's wiring --
// extracting assets, setting env vars, running from the right directory --
// can be verified without any of that.
func fakeAssets(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"install.sh": &fstest.MapFile{Data: []byte(`#!/usr/bin/env bash
set -euo pipefail
echo "KUBELET_VERSION=${KUBELET_VERSION:-unset}"
echo "STATIC_POD_PATH=${STATIC_POD_PATH:-unset}"
[ -f containerd-config.toml ] && echo "found containerd-config.toml"
[ -f kubelet-config.yaml ] && echo "found kubelet-config.yaml"
[ -f kubelet.service ] && echo "found kubelet.service"
[ -f cni/10-kontur.conflist ] && echo "found cni/10-kontur.conflist"
`)},
		"containerd-config.toml": &fstest.MapFile{Data: []byte("# containerd\n")},
		"kubelet-config.yaml":    &fstest.MapFile{Data: []byte("# kubelet\n")},
		"kubelet.service":        &fstest.MapFile{Data: []byte("# unit\n")},
		"cni/10-kontur.conflist": &fstest.MapFile{Data: []byte("{}\n")},
	}
}

func TestInstall_RequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the guard this test exercises doesn't trigger")
	}

	var stdout, stderr bytes.Buffer
	err := Install(context.Background(), fakeAssets(t), Options{}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("Install() as non-root = nil error, want a root-required error")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("Install() error = %v, want it to mention needing root", err)
	}
}

func TestInstall_ExtractsAndRunsWithEnv(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root, same as install.sh itself (see TestInstall_RequiresRoot for the non-root path)")
	}

	var stdout, stderr bytes.Buffer
	err := Install(context.Background(), fakeAssets(t), Options{
		KubeletVersion: "v9.9.9",
		StaticPodPath:  "/tmp/kontur-static-pods",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Install() error = %v, stderr = %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"KUBELET_VERSION=v9.9.9",
		"STATIC_POD_PATH=/tmp/kontur-static-pods",
		"found containerd-config.toml",
		"found kubelet-config.yaml",
		"found kubelet.service",
		"found cni/10-kontur.conflist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Install() stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestExtract_PreservesLayoutAndMakesInstallExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := extract(fakeAssets(t), dir); err != nil {
		t.Fatalf("extract() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatalf("stat install.sh: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("install.sh mode = %v, want executable", info.Mode())
	}

	data, err := os.ReadFile(filepath.Join(dir, "cni", "10-kontur.conflist"))
	if err != nil {
		t.Fatalf("cni/10-kontur.conflist not extracted: %v", err)
	}
	if string(data) != "{}\n" {
		t.Errorf("cni/10-kontur.conflist content = %q, want %q", data, "{}\n")
	}
}
