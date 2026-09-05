package ui_test

// bwsalmon/agents#444: the UI's System pane reads back core system
// logs -- these cover the API surface behind it, the same
// enabled-flag/404 conventions upgrade_test.go's own settings tests and
// server_test.go's secrets tests already exercise for their own optional
// panes.

import (
	"context"
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

type fakeLogSource struct {
	lines []string
}

func (f fakeLogSource) Tail(ctx context.Context, n int) ([]string, error) {
	if n >= len(f.lines) {
		return f.lines, nil
	}
	return f.lines[len(f.lines)-n:], nil
}

func TestListLogSourcesReportsUnavailableWithNoneConfigured(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/logs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestListLogSourcesNamesEveryConfiguredSource(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Logs = map[string]ui.LogSource{
		"daemon":          fakeLogSource{lines: []string{"a"}},
		"git-proxy-audit": fakeLogSource{lines: []string{"b"}},
	}

	rec := do(t, srv, http.MethodGet, "/api/logs", "")
	got := decode[struct {
		Enabled bool     `json:"enabled"`
		Sources []string `json:"sources"`
	}](t, rec)
	if !got.Enabled {
		t.Fatalf("enabled = false, want true")
	}
	want := []string{"daemon", "git-proxy-audit"}
	if len(got.Sources) != len(want) || got.Sources[0] != want[0] || got.Sources[1] != want[1] {
		t.Fatalf("sources = %v, want %v", got.Sources, want)
	}
}

func TestGetLogLinesReturnsTheSourcesLines(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Logs = map[string]ui.LogSource{
		"daemon": fakeLogSource{lines: []string{"one", "two", "three"}},
	}

	rec := do(t, srv, http.MethodGet, "/api/logs/daemon?lines=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[struct {
		Lines []string `json:"lines"`
	}](t, rec)
	want := []string{"two", "three"}
	if len(got.Lines) != len(want) || got.Lines[0] != want[0] || got.Lines[1] != want[1] {
		t.Fatalf("lines = %v, want %v", got.Lines, want)
	}
}

func TestGetLogLinesOfUnknownSourceIs404(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Logs = map[string]ui.LogSource{"daemon": fakeLogSource{lines: []string{"one"}}}

	rec := do(t, srv, http.MethodGet, "/api/logs/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestGetLogLinesRejectsANonPositiveLinesParam(t *testing.T) {
	srv, client := testServer(t)
	client.Config.Logs = map[string]ui.LogSource{"daemon": fakeLogSource{lines: []string{"one"}}}

	rec := do(t, srv, http.MethodGet, "/api/logs/daemon?lines=0", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
