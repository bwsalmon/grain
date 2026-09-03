package main

// Against a real embedded SQLite store, same discipline as pkg/ui's own
// tests (pkg/ui/client_test.go: "Nothing here is a fake standing in for
// the store").

import (
	"context"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestSeedDemoCoversEveryState(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	cfg := ui.Config{
		Actor:         ui.DefaultActor("demo-operator"),
		DefaultTarget: &repo,
		Capabilities:  ui.OfferedCapabilities(),
	}
	if err := seedDemo(ctx, store, cfg); err != nil {
		t.Fatalf("seedDemo: %v", err)
	}

	client := ui.NewClient(cfg, store)
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		t.Fatalf("listing seeded tasks: %v", err)
	}

	want := []model.State{
		model.StateProposed, model.StateQueued, model.StateRunning,
		model.StateAwaitingReply, model.StateCompleted, model.StateClosed,
	}
	got := map[model.State]int{}
	for _, task := range tasks {
		got[task.State]++
		if task.Repo != repo.String() {
			t.Errorf("task %s repo = %q, want %q", task.ID, task.Repo, repo.String())
		}
	}
	for _, state := range want {
		if got[state] == 0 {
			t.Errorf("no seeded task reads state %q; every state should have one so the UI can show its card", state)
		}
	}

	var awaiting ui.Task
	for _, task := range tasks {
		if task.State == model.StateAwaitingReply {
			awaiting = task
		}
	}
	detail, err := client.GetTask(ctx, awaiting.ID)
	if err != nil {
		t.Fatalf("getting the awaiting-reply task: %v", err)
	}
	if len(detail.Comments) == 0 {
		t.Error("awaiting-reply task has no comments; the pending question should be visible in the conversation")
	}
}
