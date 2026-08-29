package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun_NoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "konturctl setup") {
		t.Errorf("usage not printed to stderr:\n%s", stderr.String())
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q, want it to name the unknown command", stderr.String())
	}
}

func TestRun_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "konturctl vm create") {
		t.Errorf("usage not printed to stdout:\n%s", stdout.String())
	}
}

func TestRun_VMErrorSetsExitCode1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"vm", "delete"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (vm delete with no name should fail)", code)
	}
}
