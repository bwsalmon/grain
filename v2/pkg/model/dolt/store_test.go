package dolt_test

// The store against a real embedded Dolt database. Unlike the model
// tests, which check what grain generates, these prove Dolt accepts the
// DDL and that the views answer — which is the thing that could not be
// verified at all while the store shelled out to a CLI that was not
// installed.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
)

var (
	now   = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	human = model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}
	bot   = model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}
)

func open(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded dolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, ctx
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
	store, ctx := open(t)
	// Init runs on every start; a second one must be a no-op, not an error.
	if err := store.Init(ctx); err != nil {
		t.Fatalf("re-initialising: %v", err)
	}
}

func TestTaskRoundTripsWithEveryCollection(t *testing.T) {
	store, ctx := open(t)
	folder := model.FolderRef{Path: []string{"payments", "services/billing"}}
	want := task("a1b2", true)
	want.Body = "some body"
	want.Folder = &folder
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "gemini-key", Via: model.GrantByFolder, Folder: &folder}}
	want.Links = []model.Link{{Kind: model.LinkDependsOn, Target: "c3d4"}}
	want.Tags = []string{"nightly"}
	want.ExternalRef = "owner/agents#42"
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
	if !got.AutoMerge || got.ExternalRef != "owner/agents#42" {
		t.Errorf("flags: %+v", got)
	}
}

func TestPutTaskReplacesChildRowsRatherThanAccumulating(t *testing.T) {
	store, ctx := open(t)
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
	store, ctx := open(t)
	got, err := store.GetTask(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestUntrustedTextIsStoredNotInterpreted(t *testing.T) {
	// The reason bind parameters were worth a language change: this is a
	// value, and there is no rendering step that could make it anything
	// else.
	store, ctx := open(t)
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
	store, ctx := open(t)
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

	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded"); err != nil {
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
	store, ctx := open(t)
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
	store, ctx := open(t)
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
	store, ctx := open(t)
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
	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded"); err != nil {
		t.Fatal(err)
	}
	if live, _ = store.LiveLeases(ctx, ""); len(live) != 0 {
		t.Errorf("a finished run still holds leases: %+v", live)
	}
}

func TestDroppingALeaseTwiceIsNotAnError(t *testing.T) {
	// Release and the expiry reaper can both reach the same lease.
	store, ctx := open(t)
	for i := 0; i < 2; i++ {
		if err := store.DropLease(ctx, "r1", "gemini-key", "projects/p/keys/k"); err != nil {
			t.Fatalf("drop %d: %v", i, err)
		}
	}
}

func TestAttemptsCountsRuns(t *testing.T) {
	store, ctx := open(t)
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
		if err := store.FinishRun(ctx, id, now.Add(time.Hour), "requeued"); err != nil {
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
	store, ctx := open(t)
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
	store, ctx := open(t)
	target, reads, err := store.GitScope(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if target != nil || len(reads) != 0 {
		t.Errorf("expected no scope for an idle sandbox, got target=%+v reads=%+v", target, reads)
	}
}

func TestGitScopeStopsFollowingASandboxOnceItsRunFinishes(t *testing.T) {
	store, ctx := open(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Slot: "sandbox-0", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded"); err != nil {
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
	store, ctx := open(t)
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
	store, ctx := open(t)
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
	store, ctx := open(t)
	if _, ok, err := store.GitCredentialOverride(ctx, "sandbox-0"); err != nil || ok {
		t.Errorf("ok=%v err=%v, want false, nil for an idle sandbox", ok, err)
	}
}

func TestObservationBaselinesRoundTrip(t *testing.T) {
	store, ctx := open(t)
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

func TestDoltCommitMakesAKnownGoodPoint(t *testing.T) {
	// The durability boundary is a Dolt commit, not a transaction: this
	// is what a data diff is later taken against.
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := dolt.Commit(db, "grain: one cycle"); err != nil {
		t.Fatalf("dolt commit: %v", err)
	}
}
