package sqlite_test

// The store against a real embedded SQLite database. Unlike the model
// tests, which check what grain generates, these prove SQLite accepts
// the DDL and that the views answer -- which is the thing that could not
// be verified at all while the store shelled out to a CLI that was not
// installed.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
)

var (
	now   = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	human = model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}
	bot   = model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}
)

func openStore(t *testing.T) (*model.Store, *sql.DB, context.Context) {
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
	return store, db, ctx
}

func task(id string, approved bool) model.Task {
	t := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "Rename the endpoint",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: human},
			Reason:      model.ReasonDirect,
		},
		Target:    &model.RepoRef{Owner: "owner", Name: "payments-api"},
		Binding:   model.BindingDirective,
		CreatedAt: &now,
	}
	if approved {
		t.Approval = &model.Attribution{Actor: human}
	}
	return t
}

func TestSchemaAppliesAndIsIdempotent(t *testing.T) {
	store, _, ctx := openStore(t)
	// Init runs on every start; a second one must be a no-op, not an error.
	if err := store.Init(ctx); err != nil {
		t.Fatalf("re-initialising: %v", err)
	}
}

func TestTaskRoundTripsWithEveryCollection(t *testing.T) {
	store, _, ctx := openStore(t)
	folder := model.FolderRef{Path: []string{"payments", "services/billing"}}
	want := task("a1b2", true)
	want.Body = "some body"
	want.Folder = &folder
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "gemini-key", Via: model.GrantByFolder, Folder: &folder}}
	want.Links = []model.Link{{Kind: model.LinkDependsOn, Target: "c3d4"}}
	want.Tags = []string{"nightly"}
	want.AutoMerge = true

	if err := store.PutTask(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Title != want.Title || got.Body != want.Body {
		t.Errorf("text did not survive: %q / %q", got.Title, got.Body)
	}
	if len(got.Reads) != 1 || got.Reads[0].Name != "shared-lib" {
		t.Errorf("reads: %+v", got.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Via != model.GrantByFolder {
		t.Errorf("grants: %+v", got.Grants)
	}
	if got.Grants[0].Folder == nil || got.Grants[0].Folder.String() != folder.String() {
		t.Errorf("grant folder: %+v", got.Grants[0].Folder)
	}
	if len(got.Links) != 1 || !got.Links[0].Kind.Blocks() {
		t.Errorf("links: %+v", got.Links)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "nightly" {
		t.Errorf("tags: %+v", got.Tags)
	}
	if got.Approval == nil || got.Approval.Actor.Kind != model.PrincipalHuman {
		t.Errorf("approval: %+v", got.Approval)
	}
	if !got.AutoMerge {
		t.Errorf("flags: %+v", got)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(now) {
		t.Errorf("created_at did not survive: %+v, want %v", got.CreatedAt, now)
	}
}

func TestPutTaskReplacesChildRowsRatherThanAccumulating(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Tags = []string{"one", "two"}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	tk.Tags = []string{"three"}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, "a1b2")
	// The row set equals the object, rather than being maintained.
	if len(got.Tags) != 1 || got.Tags[0] != "three" {
		t.Errorf("tags after replace: %+v", got.Tags)
	}
}

func TestGetTaskReturnsNilWhenAbsent(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetTask(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestUntrustedTextIsStoredNotInterpreted(t *testing.T) {
	// The reason bind parameters were worth a language change: this is a
	// value, and there is no rendering step that could make it anything
	// else.
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Title = "'; DROP TABLE `task`; --"
	tk.Body = "\\'; DELETE FROM `task_run`; -- \x00 🤖"
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("the table should still exist: %v", err)
	}
	if got.Title != tk.Title {
		t.Errorf("title round-trip: %q", got.Title)
	}
}

func TestStateIsDerivedThroughEveryTransition(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", false)); err != nil {
		t.Fatal(err)
	}
	assertState := func(want model.State) {
		t.Helper()
		got, err := store.State(ctx, "a1b2")
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if got != want {
			t.Fatalf("state = %q, want %q", got, want)
		}
	}
	assertState(model.StateProposed)

	// Approval is the whole difference between proposed and queued.
	if err := store.Approve(ctx, "a1b2", model.Attribution{Actor: bot, OnBehalfOf: &human}); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateQueued)

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-1", Sandbox: "sandbox-1",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateRunning)

	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateQueued)

	id := int64(99)
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", PendingQuestionCommentID: &id,
	}); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateAwaitingReply)

	done := now.Add(2 * time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", CompletedAt: &done}); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateCompleted)

	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", CompletedAt: &done, ClosedAt: &done,
	}); err != nil {
		t.Fatal(err)
	}
	// Precedence: closed outranks completed.
	assertState(model.StateClosed)
}

func TestReadyExcludesBlockedAndUnblocksItself(t *testing.T) {
	store, _, ctx := openStore(t)
	blocker := task("c3d4", true)
	blocker.Title = "the blocker"
	blocked := task("a1b2", true)
	blocked.Links = []model.Link{{Kind: model.LinkDependsOn, Target: "c3d4"}}
	if err := store.PutTask(ctx, blocker); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTask(ctx, blocked); err != nil {
		t.Fatal(err)
	}

	if n, _ := store.OpenBlockers(ctx, "a1b2"); n != 1 {
		t.Fatalf("open blockers = %d, want 1", n)
	}
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0] != "c3d4" {
		t.Fatalf("ready = %v, want [c3d4]", ready)
	}

	// Closing the dependency unblocks the task on the next read, with no
	// reply and nothing to re-apply — the reason this is re-evaluated
	// rather than pinned at dispatch.
	if err := store.Observe(ctx, model.Observation{TaskID: "c3d4", ClosedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.OpenBlockers(ctx, "a1b2"); n != 0 {
		t.Fatalf("open blockers after close = %d, want 0", n)
	}
	ready, _ = store.Ready(ctx)
	if len(ready) != 1 || ready[0] != "a1b2" {
		t.Fatalf("ready after close = %v, want [a1b2]", ready)
	}
}

func TestANonTaskLinkTargetNeverBlocks(t *testing.T) {
	// A link to a review thread or a pull request is not a task, so the
	// join finds nothing and it cannot block — a property of the schema
	// rather than a rule anybody wrote.
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Links = []model.Link{{Kind: model.LinkAddresses, Target: "owner/repo#7:thread-3"}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.OpenBlockers(ctx, "a1b2"); n != 0 {
		t.Errorf("open blockers = %d, want 0", n)
	}
}

func TestLeasesAreQueryableByMintingCredential(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(24 * time.Hour)
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "s1", Sandbox: "s1", Attempt: 1, StartedAt: now,
		Leases: []model.Lease{{
			Capability: "gemini-key", Resource: "projects/p/keys/k",
			MintedBy: model.CredentialRef{Name: "gcp-host-service-account"},
			IssuedAt: now, ExpiresAt: &expires,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// "What would rotating this credential break?" — a query, where
	// nothing could answer it before.
	live, err := store.LiveLeases(ctx, "gcp-host-service-account")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].TaskID != "a1b2" {
		t.Fatalf("live leases = %+v", live)
	}
	if none, _ := store.LiveLeases(ctx, "some-other-credential"); len(none) != 0 {
		t.Errorf("wrong credential returned %d leases", len(none))
	}

	// Finishing the run takes its leases out of the live view.
	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if live, _ = store.LiveLeases(ctx, ""); len(live) != 0 {
		t.Errorf("a finished run still holds leases: %+v", live)
	}
}

func TestDroppingALeaseTwiceIsNotAnError(t *testing.T) {
	// Release and the expiry reaper can both reach the same lease.
	store, _, ctx := openStore(t)
	for i := 0; i < 2; i++ {
		if err := store.DropLease(ctx, "r1", "gemini-key", "projects/p/keys/k"); err != nil {
			t.Fatalf("drop %d: %v", i, err)
		}
	}
}

func TestAttemptsCountsRuns(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		if err := store.StartRun(ctx, model.Run{
			ID: id, TaskID: "a1b2", Slot: "s1", Sandbox: "s1",
			Attempt: i + 1, StartedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, now.Add(time.Hour), "requeued", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Answerable because runs are rows; the records previously existed as
	// files that nothing aggregated.
	if n, err := store.Attempts(ctx, "a1b2"); err != nil || n != 3 {
		t.Fatalf("attempts = %d (%v), want 3", n, err)
	}
}

func TestGitScopeFollowsTheLiveRunOnASandbox(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true) // Target: owner/payments-api
	tk.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-0", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	target, reads, err := store.GitScope(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if target == nil || target.String() != "owner/payments-api" {
		t.Errorf("target = %+v", target)
	}
	if len(reads) != 1 || reads[0].String() != "owner/shared-lib" {
		t.Errorf("reads = %+v", reads)
	}
}

func TestGitScopeIsEmptyWithNoLiveRunOnTheSandbox(t *testing.T) {
	store, _, ctx := openStore(t)
	target, reads, err := store.GitScope(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if target != nil || len(reads) != 0 {
		t.Errorf("expected no scope for an idle sandbox, got target=%+v reads=%+v", target, reads)
	}
}

func TestGitScopeStopsFollowingASandboxOnceItsRunFinishes(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-0", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}

	target, _, err := store.GitScope(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if target != nil {
		t.Errorf("a finished run should no longer scope its sandbox, got %+v", target)
	}
}

func TestGitCredentialOverrideFollowsTheLiveRunOnASandbox(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Grants = []model.Grant{model.GitCredentialGrant("workflow")}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-0", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	name, ok, err := store.GitCredentialOverride(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "workflow" {
		t.Errorf("name=%q ok=%v, want %q true", name, ok, "workflow")
	}
}

func TestGitCredentialOverrideIsAbsentWithoutAGitCredentialGrant(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Grants = []model.Grant{{Capability: "gemini-key", Via: model.GrantByLabel}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-0", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := store.GitCredentialOverride(ctx, "sandbox-0"); err != nil || ok {
		t.Errorf("ok=%v err=%v, want false, nil -- an unrelated grant should not override", ok, err)
	}
}

func TestGitCredentialOverrideIsAbsentWithNoLiveRunOnTheSandbox(t *testing.T) {
	store, _, ctx := openStore(t)
	if _, ok, err := store.GitCredentialOverride(ctx, "sandbox-0"); err != nil || ok {
		t.Errorf("ok=%v err=%v, want false, nil for an idle sandbox", ok, err)
	}
}

func TestObservationBaselinesRoundTrip(t *testing.T) {
	store, _, ctx := openStore(t)
	id := int64(12345)
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", BaselineCommentID: &id, ObservedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation: %v", err)
	}
	if got.BaselineCommentID == nil || *got.BaselineCommentID != 12345 {
		t.Errorf("baseline did not survive: %+v", got.BaselineCommentID)
	}
}

// --- task identity and the conversation --------------------------------

func TestNewTaskIDAllocatesDistinctIncreasingIDs(t *testing.T) {
	store, _, ctx := openStore(t)

	seen := map[string]bool{}
	var previous int64
	for i := 0; i < 5; i++ {
		id, err := store.NewTaskID(ctx)
		if err != nil {
			t.Fatalf("allocating id %d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("id %q handed out twice", id)
		}
		seen[id] = true

		// The decimal shape is what makes `grain get 42` and the branch
		// grain/task-42 read the way they do, so it is worth pinning.
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatalf("id %q is not a decimal number: %v", id, err)
		}
		if n <= previous {
			t.Fatalf("id %d did not increase on %d", n, previous)
		}
		previous = n
	}
}

// A task can be created with nothing but a store -- the whole point of
// allocating identity here rather than borrowing a GitHub issue number.
func TestATaskCanBeFiledWithNoGitHubIssueAtAll(t *testing.T) {
	store, _, ctx := openStore(t)

	id, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutTask(ctx, task(id, true)); err != nil {
		t.Fatalf("filing a task: %v", err)
	}

	got, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("task was not stored")
	}
	state, err := store.State(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Fatalf("state = %q, want queued: an approved task needs no issue to be dispatchable", state)
	}
}

func TestCommentsRoundTripInWriteOrder(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("t1", true)); err != nil {
		t.Fatal(err)
	}

	agent := model.Principal{Kind: model.PrincipalAgent, ID: "run-1"}
	bodies := []model.Comment{
		{TaskID: "t1", Author: model.Attribution{Actor: human}, Body: "please do the thing", CreatedAt: now},
		// Grain relaying an agent's question: the case Attribution exists
		// for, and the one a signature substring used to stand in for.
		{TaskID: "t1", Author: model.Attribution{Actor: bot, OnBehalfOf: &agent}, Body: "which endpoint?", CreatedAt: now},
		{TaskID: "t1", Author: model.Attribution{Actor: human}, Body: "the second one", CreatedAt: now},
	}
	for i, c := range bodies {
		if _, err := store.AddComment(ctx, c); err != nil {
			t.Fatalf("adding comment %d: %v", i, err)
		}
	}

	got, err := store.Comments(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(bodies) {
		t.Fatalf("read back %d comments, want %d", len(got), len(bodies))
	}
	for i := range got {
		if got[i].Body != bodies[i].Body {
			t.Fatalf("comment %d body = %q, want %q (order is the write order)", i, got[i].Body, bodies[i].Body)
		}
		if got[i].Author.Actor != bodies[i].Author.Actor {
			t.Fatalf("comment %d actor = %+v, want %+v", i, got[i].Author.Actor, bodies[i].Author.Actor)
		}
	}
	relayed := got[1].Author.OnBehalfOf
	if relayed == nil || *relayed != agent {
		t.Fatalf("relayed comment's OnBehalfOf = %+v, want %+v", relayed, agent)
	}
	if got[0].Author.OnBehalfOf != nil {
		t.Fatalf("a human acting directly got an OnBehalfOf: %+v", got[0].Author.OnBehalfOf)
	}
}

func TestCommentsAreScopedToTheirTask(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, id := range []string{"t1", "t2"} {
		if err := store.PutTask(ctx, task(id, true)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddComment(ctx, model.Comment{
			TaskID: id, Author: model.Attribution{Actor: human}, Body: "on " + id, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.Comments(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "on t1" {
		t.Fatalf("comments on t1 = %+v, want just its own", got)
	}
}

// The id AddComment returns is the one an Observation names, which is how
// "a question is outstanding" survives without a GitHub comment id.
func TestAPendingQuestionNamesAStoredComment(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("t1", true)); err != nil {
		t.Fatal(err)
	}

	agent := model.Principal{Kind: model.PrincipalAgent, ID: "run-1"}
	id, err := store.AddComment(ctx, model.Comment{
		TaskID: "t1", Author: model.Attribution{Actor: bot, OnBehalfOf: &agent},
		Body: "which endpoint?", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{
		TaskID: "t1", PendingQuestionCommentID: &id, ObservedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}

	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateAwaitingReply {
		t.Fatalf("state = %q, want awaiting_reply", state)
	}

	obs, err := store.GetObservation(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.PendingQuestionCommentID == nil || *obs.PendingQuestionCommentID != id {
		t.Fatalf("pending question = %+v, want the stored comment id %d", obs, id)
	}
}

// Comment bodies are untrusted text, the same as titles and task bodies
// (see TestUntrustedTextIsStoredNotInterpreted above).
func TestAnUntrustedCommentBodyIsStoredNotInterpreted(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("t1", true)); err != nil {
		t.Fatal(err)
	}

	nasty := "'); DROP TABLE `task`; -- \x00 \\ \" ' %s ?"
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID: "t1", Author: model.Attribution{Actor: human}, Body: nasty, CreatedAt: now,
	}); err != nil {
		t.Fatalf("storing an untrusted comment body: %v", err)
	}

	got, err := store.Comments(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != nasty {
		t.Fatalf("comment body did not round-trip verbatim: %q", got[0].Body)
	}
	// The table it tried to drop is still answering.
	if _, err := store.GetTask(ctx, "t1"); err != nil {
		t.Fatalf("task table after an injection attempt: %v", err)
	}
}

// --- concurrency: SQLite's own single-writer lock -----------------------
//
// Dolt merged concurrent writers cell by cell and only reported a
// conflict when two of them touched the same cell with different values
// -- which is why the deleted dolt/store_test.go needed a random write
// stamp and a test (TestACounterStampWouldNotConflict) proving a plain
// counter would not have caught every overlap. SQLite admits only one
// writer at a time, full stop (sqlite.Open's _txlock=immediate), so
// there is no equivalent hazard to guard against here: two writers
// either take turns, serialised by the lock itself, or one of them fails
// outright with SQLITE_BUSY. The tests below exercise both outcomes
// instead.

// Two writers changing different fields of the same task at the same
// time: SQLite's write lock makes one wait for the other, and both
// changes end up present once each has had its turn.
func TestConcurrentUpdatesBothLand(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
			tk.Title = "renamed"
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
			tk.Base = "release"
			return nil
		})
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed" {
		t.Fatalf("title = %q, want the first writer's change", got.Title)
	}
	if got.Base != "release" {
		t.Fatalf("base = %q, want the second writer's change -- it was lost", got.Base)
	}
}

// One writer's transaction rolls back whole, child rows included, if it
// loses the race for the write lock and has to retry -- without that,
// two writers each doing PutTask's delete-and-reinsert of a task's tags
// could see their sets silently unioned.
func TestAConflictRollsBackChildRowsToo(t *testing.T) {
	store, _, ctx := openStore(t)
	base := task("a1b2", true)
	base.Tags = []string{"keep"}
	if err := store.PutTask(ctx, base); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	for i, tag := range []string{"from-0", "from-1"} {
		go func(i int, tag string) {
			defer wg.Done()
			<-start
			if err := store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
				tk.Tags = append(tk.Tags, tag)
				return nil
			}); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i, tag)
	}
	close(start)
	wg.Wait()

	got, err := store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got.Tags)
	want := []string{"from-0", "from-1", "keep"}
	if len(got.Tags) != len(want) {
		t.Fatalf("tags = %v, want %v -- a retry that rewrote over the winner would have lost one", got.Tags, want)
	}
	for i := range want {
		if got.Tags[i] != want[i] {
			t.Fatalf("tags = %v, want %v", got.Tags, want)
		}
	}
}

// The wording model.isSerializationFailure matches on, asserted against
// the real engine rather than assumed. pkg/model's own stamp_test.go
// pins the same string, so a driver reword fails both together.
func TestSQLiteReportsABusyDatabase(t *testing.T) {
	db, cleanup := openWithBusyTimeout(t, 200*time.Millisecond)
	defer cleanup()
	if _, err := db.Exec(
		"CREATE TABLE `probe` (`id` INTEGER NOT NULL, `v` TEXT NOT NULL, PRIMARY KEY (`id`))"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	holder, err := db.BeginTx(ctx, nil) // takes SQLite's one write lock and holds it
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()

	_, err = db.BeginTx(ctx, nil) // must wait out busy_timeout, then fail
	if err == nil {
		t.Fatal("a second writer started while the first was still open")
	}
	if !strings.Contains(err.Error(), "database is locked") && !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("SQLite reports a busy database as %q, which model.isSerializationFailure "+
			"does not recognise -- update the markers there and in pkg/model/stamp_test.go", err)
	}
	t.Logf("SQLite reports a busy database as: %v", err)
}

// The retry path, forced deterministically with a short busy_timeout
// rather than raced for: a competing transaction holds the write lock
// past that timeout, so the first attempt or two are refused with
// SQLITE_BUSY before the lock frees up and Store.write's retry loop
// succeeds.
func TestARetryRecoversFromABusyDatabase(t *testing.T) {
	db, cleanup := openWithBusyTimeout(t, 100*time.Millisecond)
	defer cleanup()
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// Held comfortably longer than one busy_timeout, so the update
		// below is refused at least once before this releases the lock --
		// exercising write's retry loop rather than hoping to catch it.
		time.Sleep(350 * time.Millisecond)
		_ = blocker.Rollback()
	}()

	if err := store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
		tk.Title = "renamed"
		return nil
	}); err != nil {
		t.Fatalf("UpdateTask did not recover from a busy database: %v", err)
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed" {
		t.Fatalf("title = %q, want the retried write to have landed", got.Title)
	}
}

// The answer to "what if it crashes mid-write": an operation that does
// not reach commit leaves nothing behind. There is no lock left held and
// no merge to reconcile, so the failure needs no cleanup at all.
func TestAFailedWriteLeavesNothingBehind(t *testing.T) {
	store, _, ctx := openStore(t)
	before := task("a1b2", true)
	before.Tags = []string{"keep"}
	if err := store.PutTask(ctx, before); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("the process died here")
	err := store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
		// Change everything, then fail before the commit.
		tk.Title = "half-written"
		tk.Tags = []string{"gone"}
		tk.Base = "wrong"
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the failure to reach the caller", err)
	}

	got, err := store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != before.Title || got.Base != before.Base {
		t.Fatalf("task changed despite the write failing: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "keep" {
		t.Fatalf("tags = %v, want the original -- a partial write survived", got.Tags)
	}

	// And the store is immediately usable: nothing is held.
	if err := store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
		tk.Title = "after the failure"
		return nil
	}); err != nil {
		t.Fatalf("the store was left unusable by a failed write: %v", err)
	}
}

func TestGetConfigReturnsNilOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetConfig(ctx)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) before anything has written a config, got (%+v, %v)", got, err)
	}
}

func testConfig() model.Config {
	return model.Config{
		PollInterval: 30 * time.Second, Slots: []string{"a", "b"},
		GeminiModel: "gemini-2.5-pro", MaxAgentTurns: 40,
		GitHubHost: "github.com", GitHubInsecureHTTP: false,
		GCPProject: "grain-prod", GCPServiceAccountEmail: "agent@grain-prod.iam.gserviceaccount.com",
		TargetRepos: []string{"acme/widgets", "acme/gadgets"},
	}
}

func TestConfigRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// PutConfig replaces the single row wholesale rather than accumulating a
// second one -- there is exactly one deployment configuration, the same
// discipline grain_schema holds to.
func TestPutConfigReplacesRatherThanAccumulating(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutConfig(ctx, testConfig()); err != nil {
		t.Fatalf("first put: %v", err)
	}
	updated := testConfig()
	updated.PollInterval = time.Minute
	updated.Slots = []string{"only-one"}
	if err := store.PutConfig(ctx, updated); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, updated) {
		t.Fatalf("got %+v, want %+v", *got, updated)
	}
}

// bwsalmon/agents#427: grain_config.target_repos did not exist at all
// before this, on any database, so an already-created grain_config table
// has no such column -- CREATE TABLE IF NOT EXISTS (schema.go's own
// Tables) does nothing to a table that's already there. This simulates
// exactly that database, built with the pre-#427 column set directly
// rather than through Store, and checks Store.Init's own migration step
// (ensureConfigTargetReposColumn) brings it up to date in place without
// disturbing the row already sitting in it.
func TestInitMigratesAnExistingDatabaseMissingTargetRepos(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`slots`"+`                      TEXT    NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#427 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`slots`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`) "+
			"VALUES (1,30000,'a,b','gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com')"); err != nil {
		t.Fatalf("seeding a pre-#427 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing target_repos: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.GeminiModel != "gemini-2.5-pro" || len(got.TargetRepos) != 0 {
		t.Fatalf("got %+v, want the pre-existing row intact and targetRepos empty", got)
	}

	// And it's not just readable -- PutConfig can now actually make
	// targetRepos durable, which is the whole bug this migration fixes.
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get after migrating: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// openWithBusyTimeout opens a database file with a shorter busy_timeout
// than sqlite.Open's own (five seconds -- too long for a test to wait
// out on purpose), so the two determinism tests above can force
// SQLITE_BUSY on a bounded, predictable schedule instead of racing for
// it.
func openWithBusyTimeout(t *testing.T, timeout time.Duration) (*sql.DB, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grain.db")
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(timeout.Milliseconds(), 10)+")")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		t.Fatalf("opening sqlite with a custom busy_timeout: %v", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		t.Fatalf("enabling WAL: %v", err)
	}
	return db, func() { db.Close() }
}
