package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
)

// list_test.go covers what taskQuery decides; this covers the wiring
// around it, against a real server -- that every flag reaches the field
// it names, that a tri-state flag can be asked in the negative, and that
// a value nobody can act on stops the command rather than printing an
// empty listing.
func listTestServer(t *testing.T) string {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	client := ui.NewClient(ui.Config{Actor: ui.DefaultActor("tester"), Capabilities: ui.OfferedCapabilities()}, store)
	if _, err := client.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "Widget work", Repo: "acme/widgets", Approved: true, AutoMerge: true,
	}); err != nil {
		t.Fatalf("creating the queued task: %v", err)
	}
	if _, err := client.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "Gadget work", NoRepo: true,
	}); err != nil {
		t.Fatalf("creating the proposed task: %v", err)
	}
	srv := httptest.NewServer(ui.NewServerWithClient(client))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestListFlagsNarrowTheListing(t *testing.T) {
	server := listTestServer(t)
	for _, tc := range []struct {
		name  string
		args  []string
		want  []string
		avoid []string
	}{
		{
			name: "no flags lists everything",
			args: []string{"list"},
			want: []string{"Widget work", "Gadget work"},
		},
		{
			name:  "state",
			args:  []string{"list", "-state", "queued"},
			want:  []string{"Widget work"},
			avoid: []string{"Gadget work"},
		},
		{
			name:  "repo",
			args:  []string{"list", "-repo", "acme/widgets"},
			want:  []string{"Widget work"},
			avoid: []string{"Gadget work"},
		},
		{
			name:  "the tasks with no repo at all",
			args:  []string{"list", "-repo", "none"},
			want:  []string{"Gadget work"},
			avoid: []string{"Widget work"},
		},
		{
			name:  "search",
			args:  []string{"list", "-search", "gadget"},
			want:  []string{"Gadget work"},
			avoid: []string{"Widget work"},
		},
		// A tri-state flag asked in the negative: the tasks that do not
		// merge themselves, which "-auto-merge" left off could not ask
		// for.
		{
			name:  "auto-merge, in the negative",
			args:  []string{"list", "-auto-merge=false"},
			want:  []string{"Gadget work"},
			avoid: []string{"Widget work"},
		},
		{
			name:  "auto-merge",
			args:  []string{"list", "-auto-merge"},
			want:  []string{"Widget work"},
			avoid: []string{"Gadget work"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := runCLI(append([]string{"-server", server}, tc.args...)); err != nil {
					t.Fatalf("grain %s: %v", strings.Join(tc.args, " "), err)
				}
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("grain %s printed %q, want it to list %q", strings.Join(tc.args, " "), out, want)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(out, avoid) {
					t.Errorf("grain %s printed %q, want it not to list %q", strings.Join(tc.args, " "), out, avoid)
				}
			}
		})
	}
}

func TestListSortsByTitle(t *testing.T) {
	server := listTestServer(t)
	out := captureStdout(t, func() {
		if err := runCLI([]string{"-server", server, "list", "-sort", "title"}); err != nil {
			t.Fatalf("grain list -sort title: %v", err)
		}
	})
	gadget, widget := strings.Index(out, "Gadget work"), strings.Index(out, "Widget work")
	if gadget < 0 || widget < 0 {
		t.Fatalf("grain list -sort title printed %q, want both tasks", out)
	}
	if gadget > widget {
		t.Errorf("grain list -sort title printed %q, want Gadget before Widget", out)
	}
}

func TestListRejectsAValueNobodyCanActOn(t *testing.T) {
	server := listTestServer(t)
	for _, args := range [][]string{
		{"list", "-state", "in_progress"},
		{"list", "-origin", "robot"},
		{"list", "-sort", "newest-first"},
	} {
		if err := runCLI(append([]string{"-server", server}, args...)); err == nil {
			t.Errorf("grain %s returned no error, want one naming the values that work", strings.Join(args, " "))
		}
	}
}
