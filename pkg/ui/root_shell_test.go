package ui_test

// POST /api/host/shell: the System overlay's Root shell tab
// (grain/task-13) -- one command, run as root on the machine the daemon
// runs on, and everything it printed. Same nil-means-unavailable
// convention host_test.go exercises for the Top tab beside it, with the
// difference that matters here being what a wired-up deployment is
// handing over: root, unrestricted.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestRootShellIsNotAvailableWithNoneConfigured(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/host/shell", `{"command":"id"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("body = %s, want it to say the deployment has no root shell", rec.Body)
	}
}

// GET /api/config carries the same fact, so the tab can say so without
// making a call that could only fail.
func TestConfigReportsWhetherTheRootShellIsAvailable(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/api/config", "")
	if got := decode[map[string]any](t, rec); got["rootShellEnabled"] != false {
		t.Fatalf("rootShellEnabled = %v with none configured, want false", got["rootShellEnabled"])
	}

	client, _, _ := testClient(t)
	client.Config.RootShell = func(context.Context, string) (ui.RootShellResult, error) {
		return ui.RootShellResult{}, nil
	}
	rec = do(t, ui.NewServerWithClient(client), http.MethodGet, "/api/config", "")
	if got := decode[map[string]any](t, rec); got["rootShellEnabled"] != true {
		t.Fatalf("rootShellEnabled = %v with one configured, want true", got["rootShellEnabled"])
	}
}

func TestRootShellRunsTheCommandAndReturnsWhatItPrinted(t *testing.T) {
	client, _, _ := testClient(t)
	var asked string
	client.Config.RootShell = func(_ context.Context, command string) (ui.RootShellResult, error) {
		asked = command
		return ui.RootShellResult{Output: "uid=0(root) gid=0(root)\n"}, nil
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodPost, "/api/host/shell", `{"command":"id && echo ok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	// Verbatim, pipelines and `&&` and all: what the pane sends is a
	// command line, not an argv, and rewriting it here would break the
	// half of shell syntax the pane is opened for.
	if asked != "id && echo ok" {
		t.Fatalf("the command reached the runner as %q", asked)
	}
	got := decode[struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}](t, rec)
	if !strings.Contains(got.Output, "uid=0(root)") {
		t.Fatalf("output = %q, want what the command printed", got.Output)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", got.ExitCode)
	}
}

// A command that failed is a 200 carrying its exit code: the call did
// what it was asked, and "exited 1" is the answer the operator wanted.
// Reporting it as an HTTP error would put the output in an error banner
// and lose the distinction between a failed command and a failed
// deployment.
func TestAFailedCommandIsStillAnAnswer(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.RootShell = func(context.Context, string) (ui.RootShellResult, error) {
		return ui.RootShellResult{Output: "no such unit\n", ExitCode: 5}, nil
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodPost, "/api/host/shell", `{"command":"systemctl status nope"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}](t, rec)
	if got.ExitCode != 5 || !strings.Contains(got.Output, "no such unit") {
		t.Fatalf("got %+v, want the command's own output and status", got)
	}
}

// The exchange failing is a different thing from the command failing --
// no responder installed on this host, a control directory the daemon
// cannot write -- and it is reported as an error, with what pkg/rootshell
// had to say about it intact: that message names the unit to install.
func TestARunnerErrorIsReportedAsOne(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.RootShell = func(context.Context, string) (ui.RootShellResult, error) {
		return ui.RootShellResult{}, errors.New("no answer from this host's root shell responder: grain-shell.service")
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodPost, "/api/host/shell", `{"command":"id"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "grain-shell.service") {
		t.Fatalf("body = %s, want the runner's own message", rec.Body)
	}
}

// An empty command never reaches the far end: a stray Enter in the pane
// should not wake a root shell on the host at all.
func TestAnEmptyCommandIsRefused(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.RootShell = func(context.Context, string) (ui.RootShellResult, error) {
		t.Error("the runner was called for a request that should have been rejected")
		return ui.RootShellResult{}, nil
	}
	srv := ui.NewServerWithClient(client)

	for _, body := range []string{`{"command":""}`, `{"command":"   "}`, `{}`} {
		rec := do(t, srv, http.MethodPost, "/api/host/shell", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400: %s", rec.Code, body, rec.Body)
		}
	}
}

func TestARidiculouslyLongCommandIsRefused(t *testing.T) {
	client, _, _ := testClient(t)
	client.Config.RootShell = func(context.Context, string) (ui.RootShellResult, error) {
		t.Error("the runner was called for a request that should have been rejected")
		return ui.RootShellResult{}, nil
	}
	srv := ui.NewServerWithClient(client)

	rec := do(t, srv, http.MethodPost, "/api/host/shell", `{"command":"`+strings.Repeat("x", 70<<10)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
