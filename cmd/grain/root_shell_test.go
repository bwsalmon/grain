package main

// The gate on the UI's root shell (grain/task-13). What is decidable
// here rather than in pkg/rootshell is the one thing this file exists
// for: a daemon that was not told where a host-side responder is
// listening hands the UI no root shell at all, so the route 404s and the
// tab says so -- rather than being wired to a directory nothing watches,
// where every command would hang for two minutes before failing.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNoRootShellUnlessAControlDirectoryIsNamed(t *testing.T) {
	if rootShell("") != nil {
		t.Fatal("a daemon with no -root-shell-control-dir still handed the UI a root shell")
	}
}

func TestARootShellIsWiredToTheControlDirectory(t *testing.T) {
	dir := t.TempDir()
	shell := rootShell(dir)
	if shell == nil {
		t.Fatal("-root-shell-control-dir named a directory and no root shell came back")
	}

	// Nothing is watching this directory, which is the deployment the
	// error message is written for -- a host installed before the
	// responder existed. The wait is cut short by the context rather
	// than by pkg/rootshell's own two-minute default.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := shell(ctx, "echo hello")
	if err == nil {
		t.Fatal("a command answered with nothing watching the control directory")
	}
	if !strings.Contains(err.Error(), "setup.sh") {
		t.Errorf("error is %q, want it to name what installs the missing responder", err)
	}
}
