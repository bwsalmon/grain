package selfrepair

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
)

func openStore(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
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
	return store, ctx
}

func putTask(t *testing.T, store *model.Store, ctx context.Context) string {
	t.Helper()
	id, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatalf("NewTaskID: %v", err)
	}
	if err := store.PutTask(ctx, model.Task{ID: id, Interactive: true}); err != nil {
		t.Fatalf("PutTask: %v", err)
	}
	return id
}

func TestSpecIsAGrantCapabilityNamedSelfRepair(t *testing.T) {
	spec := New().Spec()
	if CapabilityName != "self-repair" || spec.Name != CapabilityName {
		t.Fatalf("Name = %q, want %q", spec.Name, "self-repair")
	}
	if spec.Provision != model.ProvisionGrant {
		t.Fatalf("Provision = %q, want %q", spec.Provision, model.ProvisionGrant)
	}
}

func humanReply(t *testing.T, store *model.Store, ctx context.Context, taskID, body string) {
	t.Helper()
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    taskID,
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "operator"}},
		Body:      body,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
}

func TestConfirmApprovedRunsAfterAHumanApproves(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	done := make(chan struct{})
	var approved bool
	var reply string
	go func() {
		approved, reply, _ = Confirm(ctx, store, taskID, "run rm -rf /tmp/x?", 20*time.Millisecond, time.Second)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	humanReply(t, store, ctx, taskID, "approve, go ahead")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return in time")
	}
	if !approved {
		t.Fatalf("approved = false, reply = %q, want true", reply)
	}
}

func TestConfirmDeniedAfterAHumanDenies(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	done := make(chan struct{})
	var approved bool
	var reply string
	go func() {
		approved, reply, _ = Confirm(ctx, store, taskID, "run rm -rf /tmp/x?", 20*time.Millisecond, time.Second)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	humanReply(t, store, ctx, taskID, "deny, that looks dangerous")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return in time")
	}
	if approved {
		t.Fatal("approved = true, want false")
	}
	if !strings.Contains(reply, "dangerous") {
		t.Fatalf("reply = %q, want it to carry the human's own words", reply)
	}
}

func TestConfirmDeniesOnTimeoutWithNoReply(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	approved, reply, err := Confirm(ctx, store, taskID, "run something?", 10*time.Millisecond, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if approved {
		t.Fatal("approved = true, want false on timeout")
	}
	if !strings.Contains(reply, "timeout") {
		t.Fatalf("reply = %q, want it to mention the timeout", reply)
	}

	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs != nil && obs.PendingQuestionCommentID != nil {
		t.Fatal("PendingQuestionCommentID left set after a timeout, want it cleared")
	}
}

func TestConfirmIgnoresNonHumanCommentsAndItsOwnQuestion(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	// The timeout is generous, unlike TestConfirmDeniesOnTimeoutWithNoReply's
	// own: what this test asserts is that Confirm keeps waiting through a
	// non-human comment, so a deadline anywhere near the ~170ms of sleeps
	// below would make a loaded machine (`go test -race ./...` runs this
	// package alongside every other) report a timeout as a missed
	// approval.
	done := make(chan struct{})
	var approved bool
	go func() {
		approved, _, _ = Confirm(ctx, store, taskID, "run something?", 10*time.Millisecond, 30*time.Second)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	agentPrincipal := model.Principal{Kind: model.PrincipalAgent, ID: taskID}
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID: taskID,
		Author: model.Attribution{
			Actor:      model.Principal{Kind: model.PrincipalAutomation, ID: "grain"},
			OnBehalfOf: &agentPrincipal,
		},
		Body:      "approve -- said by the agent itself, not a human",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	select {
	case <-done:
		t.Fatal("Confirm returned before a human replied -- it must not trust a non-human comment")
	case <-time.After(150 * time.Millisecond):
	}
	humanReply(t, store, ctx, taskID, "approve")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Confirm did not return after the human's own approve")
	}
	if !approved {
		t.Fatal("approved = false after the human's own approve, want true")
	}
}

func TestHostCommandToolRunsTheCommandOnceApproved(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	tools := HostCommandTools(store, taskID, 10*time.Millisecond, time.Second)
	if len(tools) != 1 || tools[0].Name != "run_host_command" {
		t.Fatalf("tools = %+v, want exactly one run_host_command tool", tools)
	}

	resultCh := make(chan struct {
		text    string
		isError bool
	}, 1)
	go func() {
		res := tools[0].Handler(ctx, map[string]any{"command": "echo hello-from-configuration-mode"})
		resultCh <- struct {
			text    string
			isError bool
		}{res.Text, res.IsError}
	}()

	time.Sleep(50 * time.Millisecond)
	humanReply(t, store, ctx, taskID, "approve")

	select {
	case res := <-resultCh:
		if res.isError {
			t.Fatalf("run_host_command errored: %s", res.text)
		}
		if !strings.Contains(res.text, "hello-from-configuration-mode") {
			t.Fatalf("Text = %q, want it to contain the command's own stdout", res.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run_host_command did not return in time")
	}
}

func TestHostCommandToolNeverRunsWhenDenied(t *testing.T) {
	store, ctx := openStore(t)
	taskID := putTask(t, store, ctx)

	tools := HostCommandTools(store, taskID, 10*time.Millisecond, time.Second)

	resultCh := make(chan struct {
		text    string
		isError bool
	}, 1)
	go func() {
		res := tools[0].Handler(ctx, map[string]any{"command": "touch /tmp/should-never-exist-selfrepair-test"})
		resultCh <- struct {
			text    string
			isError bool
		}{res.Text, res.IsError}
	}()

	time.Sleep(50 * time.Millisecond)
	humanReply(t, store, ctx, taskID, "deny")

	select {
	case res := <-resultCh:
		if !res.isError {
			t.Fatalf("expected an error result for a denied command, got: %s", res.text)
		}
		if !strings.Contains(res.text, "denied") {
			t.Fatalf("Text = %q, want it to say denied", res.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run_host_command did not return in time")
	}
}
