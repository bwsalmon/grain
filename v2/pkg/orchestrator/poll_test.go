package orchestrator_test

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func taskRepoCfg() orchestrator.Config {
	return orchestrator.Config{
		TaskRepo:     model.RepoRef{Owner: "acme", Name: "tasks"},
		TriggerLabel: "grain-agent",
	}
}

func TestPollIssuesFilesAQueuedTaskFromADirective(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "tasks", "main")
	sim.Issues[1] = &githubsim.Issue{
		Title: "fix the thing", Body: "please fix it\n\n/repo acme/widgets\n",
		Author: "alice", Labels: map[string]struct{}{"grain-agent": {}},
	}
	cfg := taskRepoCfg()

	if err := orchestrator.PollIssues(ctx, store, client, cfg, baseTime); err != nil {
		t.Fatalf("PollIssues: %v", err)
	}

	id := orchestrator.TaskID(cfg.TaskRepo, 1)
	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("expected a task to have been filed")
	}
	if task.Target == nil || *task.Target != (model.RepoRef{Owner: "acme", Name: "widgets"}) {
		t.Fatalf("Target = %+v, want acme/widgets", task.Target)
	}
	if task.Approval == nil || task.Approval.Actor.ID != "alice" {
		t.Fatalf("Approval = %+v, want attributed to alice", task.Approval)
	}
	st, err := store.State(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateQueued {
		t.Fatalf("state = %q, want queued", st)
	}

	// The trigger label comes off once the task is durably filed.
	issue, err := client.GetIssue("acme", "tasks", 1)
	if err != nil {
		t.Fatal(err)
	}
	if issue.HasLabel("grain-agent") {
		t.Fatal("expected the trigger label to be removed")
	}
}

func TestPollIssuesParksAnIssueWithNoRepoDirective(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "tasks", "main")
	sim.Issues[1] = &githubsim.Issue{
		Title: "vague request", Body: "do something", Author: "alice",
		Labels: map[string]struct{}{"grain-agent": {}},
	}
	cfg := taskRepoCfg()

	if err := orchestrator.PollIssues(ctx, store, client, cfg, baseTime); err != nil {
		t.Fatalf("PollIssues: %v", err)
	}

	id := orchestrator.TaskID(cfg.TaskRepo, 1)
	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("expected no task to be filed for an unresolvable target, got %+v", task)
	}

	comments, err := client.ListComments("acme", "tasks", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected one parking comment, got %+v", comments)
	}

	issue, err := client.GetIssue("acme", "tasks", 1)
	if err != nil {
		t.Fatal(err)
	}
	if issue.HasLabel("grain-agent") {
		t.Fatal("expected the trigger label to still be removed even when parked")
	}
}

func TestPollIssuesUsesDefaultTargetWhenNoDirectiveIsGiven(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "tasks", "main")
	sim.Issues[1] = &githubsim.Issue{
		Title: "fix it", Body: "no directive here", Author: "alice",
		Labels: map[string]struct{}{"grain-agent": {}},
	}
	target := model.RepoRef{Owner: "acme", Name: "widgets"}
	cfg := taskRepoCfg()
	cfg.DefaultTarget = &target

	if err := orchestrator.PollIssues(ctx, store, client, cfg, baseTime); err != nil {
		t.Fatalf("PollIssues: %v", err)
	}

	task, err := store.GetTask(ctx, orchestrator.TaskID(cfg.TaskRepo, 1))
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.Target == nil || *task.Target != target {
		t.Fatalf("task = %+v, want target %+v", task, target)
	}
}

func TestPollIssuesRequeuesATaskAwaitingReplyWhenTheLabelReturns(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "tasks", "main")
	sim.Issues[1] = &githubsim.Issue{Title: "t", Body: "b", Author: "alice", Labels: map[string]struct{}{}}
	cfg := taskRepoCfg()
	id := orchestrator.TaskID(cfg.TaskRepo, 1)

	target := model.RepoRef{Owner: "acme", Name: "widgets"}
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "t", Body: "b",
		Origin:      model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval:    &model.Attribution{Actor: human},
		Target:      &target,
		Binding:     model.BindingDirective,
		ExternalRef: "acme/tasks#1",
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	pending := int64(42)
	if err := store.Observe(ctx, model.Observation{TaskID: id, PendingQuestionCommentID: &pending}); err != nil {
		t.Fatal(err)
	}
	st, err := store.State(ctx, id)
	if err != nil || st != model.StateAwaitingReply {
		t.Fatalf("state = %q err=%v, want awaiting_reply", st, err)
	}

	if err := client.AddLabel("acme", "tasks", 1, "grain-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateComment("acme", "tasks", 1, "here's more info"); err != nil {
		t.Fatal(err)
	}

	if err := orchestrator.PollIssues(ctx, store, client, cfg, baseTime); err != nil {
		t.Fatalf("PollIssues: %v", err)
	}

	st, err = store.State(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateQueued {
		t.Fatalf("state = %q, want queued once the label returned", st)
	}
}
