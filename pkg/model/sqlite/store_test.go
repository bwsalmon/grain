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
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
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
	want.Interactive = true
	want.Configuration = true

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
	if !got.AutoMerge || !got.Interactive || !got.Configuration {
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
	if err := store.Approve(ctx, "a1b2", model.Attribution{Actor: bot, OnBehalfOf: &human}, now); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateQueued)

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Sandbox: "sandbox-1",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
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

// TestAnUnsubmittedPullRequestIsItsOwnStateAndStillSynced proves the
// half of task_state's CASE that reads something other than
// task_observation -- the task's own auto_merge column and its fixes-link
// -- against a real database rather than only against model.StateOf, and
// pins the query that keeps such a task's pull request under grain's
// watch while it waits.
func TestAnUnsubmittedPullRequestIsItsOwnStateAndStillSynced(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.Links = []model.Link{{Kind: model.LinkFixes, Target: "owner/payments-api#7"}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	done := now.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", CompletedAt: &done}); err != nil {
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
		stored, err := store.GetTask(ctx, "a1b2")
		if err != nil {
			t.Fatal(err)
		}
		obs, err := store.GetObservation(ctx, "a1b2")
		if err != nil {
			t.Fatal(err)
		}
		// The two derivations of one rule, held together the way
		// state_test.go holds the precedence: the view and StateOf must
		// answer the same on the condition only one of them can see.
		if derived := model.StateOf(*stored, obs, false, 0); derived != got {
			t.Fatalf("task_state view = %q, model.StateOf = %q", got, derived)
		}
	}
	assertState(model.StateAwaitingSubmit)

	// A pull request waiting on a human is still watched: SyncPullRequests
	// has to see a hand-merge on GitHub, or the task would never close.
	links, err := store.OpenPullRequestLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].PullRequest != "owner/payments-api#7" {
		t.Fatalf("OpenPullRequestLinks = %+v, want the unsubmitted pull request", links)
	}

	// Submit is AutoMerge (ui.Client.Submit), and it is the only way out.
	tk.AutoMerge = true
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateCompleted)
	if links, err := store.OpenPullRequestLinks(ctx); err != nil || len(links) != 1 {
		t.Fatalf("OpenPullRequestLinks after submitting = %+v (err %v), want it still watched", links, err)
	}

	// And closing outranks both, which is what drops it out of the sync.
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", CompletedAt: &done, ClosedAt: &done,
	}); err != nil {
		t.Fatal(err)
	}
	assertState(model.StateClosed)
	if links, err := store.OpenPullRequestLinks(ctx); err != nil || len(links) != 0 {
		t.Fatalf("OpenPullRequestLinks after closing = %+v (err %v), want none", links, err)
	}
}

// A task the merge queue has sent back to repair its own pull request
// branch (orchestrator.requeueForRepair) has no completed_at at all --
// it reads 'queued' or 'running' like any other attempt -- and yet its
// pull request is exactly the one the queue is in the middle of driving.
// Dropping it would hand its queue position to whatever is behind it and
// leave the repair running with nothing watching the deadline it runs
// against, so OpenPullRequestLinks keeps returning it until it completes
// or closes.
func TestOpenPullRequestLinksKeepsWatchingATaskBeingRepaired(t *testing.T) {
	store, _, ctx := openStore(t)
	tk := task("a1b2", true)
	tk.AutoMerge = true
	tk.Links = []model.Link{{Kind: model.LinkFixes, Target: "owner/payments-api#7"}}
	if err := store.PutTask(ctx, tk); err != nil {
		t.Fatal(err)
	}

	done := now
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", CompletedAt: &done}); err != nil {
		t.Fatal(err)
	}
	if links, err := store.OpenPullRequestLinks(ctx); err != nil || len(links) != 1 {
		t.Fatalf("OpenPullRequestLinks for a completed task = %+v (err %v), want one", links, err)
	}

	// The requeue: completed_at cleared, merge_queue_repair_at written.
	asked := now.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", MergeQueueRepairAt: &asked}); err != nil {
		t.Fatal(err)
	}
	if st, err := store.State(ctx, "a1b2"); err != nil || st != model.StateQueued {
		t.Fatalf("state = %q (%v), want queued: clearing completed_at is what requeues it", st, err)
	}
	links, err := store.OpenPullRequestLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].PullRequest != "owner/payments-api#7" {
		t.Fatalf("OpenPullRequestLinks while being repaired = %+v, want the pull request still watched", links)
	}
	// Once per task, however the two arms of the WHERE overlap.
	repairing, err := store.MergeQueueRepairing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !repairing["a1b2"] {
		t.Fatalf("MergeQueueRepairing = %+v, want a1b2", repairing)
	}

	// Closing still outranks it: a task nobody is watching any more.
	closed := asked.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", MergeQueueRepairAt: &asked, ClosedAt: &closed,
	}); err != nil {
		t.Fatal(err)
	}
	if links, err := store.OpenPullRequestLinks(ctx); err != nil || len(links) != 0 {
		t.Fatalf("OpenPullRequestLinks after closing = %+v (err %v), want none", links, err)
	}
	if repairing, err := store.MergeQueueRepairing(ctx); err != nil || repairing["a1b2"] {
		t.Fatalf("MergeQueueRepairing after closing = %+v (err %v), want none", repairing, err)
	}
}

// Withdrawing approval is the one way a task moves back up the
// precedence order rather than down it, so it is worth proving against a
// real database rather than only against StateOf: the state comes out of
// task_state's own CASE on approval_actor_kind, and dispatch stops
// seeing the task because task_ready reads that view.
func TestWithdrawingApprovalReturnsATaskToTheProposalsAndOutOfReady(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", false)); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(ctx, "a1b2", model.Attribution{Actor: human}, now); err != nil {
		t.Fatal(err)
	}
	if ready, _ := store.Ready(ctx); len(ready) != 1 || ready[0] != "a1b2" {
		t.Fatalf("ready after approval = %v, want [a1b2]", ready)
	}

	if err := store.WithdrawApproval(ctx, "a1b2"); err != nil {
		t.Fatalf("withdrawing approval: %v", err)
	}
	if state, err := store.State(ctx, "a1b2"); err != nil || state != model.StateProposed {
		t.Fatalf("state = %q (err %v), want proposed", state, err)
	}
	if ready, _ := store.Ready(ctx); len(ready) != 0 {
		t.Fatalf("ready after withdrawal = %v, want nothing dispatchable", ready)
	}
	got, err := store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Approval != nil || got.ApprovedAt != nil {
		t.Fatalf("approval = %+v, approvedAt = %v, want both cleared", got.Approval, got.ApprovedAt)
	}

	// Approving again is the whole of re-queueing it, and the row comes
	// back with a live timestamp rather than the withdrawn one.
	later := now.Add(time.Hour)
	if err := store.Approve(ctx, "a1b2", model.Attribution{Actor: human}, later); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetTask(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ApprovedAt == nil || !got.ApprovedAt.Equal(later) {
		t.Fatalf("approvedAt after re-approval = %v, want %v", got.ApprovedAt, later)
	}
	if ready, _ := store.Ready(ctx); len(ready) != 1 || ready[0] != "a1b2" {
		t.Fatalf("ready after re-approval = %v, want [a1b2]", ready)
	}
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
		ID: "r1", TaskID: "a1b2", Sandbox: "s1", Attempt: 1, StartedAt: now,
		Leases: []model.Lease{{
			Capability: "gemini-key", Resource: "projects/p/keys/k",
			MintedBy: model.CredentialRef{Name: "gcp-host-service-account"},
			IssuedAt: now, ExpiresAt: &expires,
		}},
	}, model.Limits{}); err != nil {
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
			ID: id, TaskID: "a1b2", Sandbox: "s1",
			Attempt: i + 1, StartedAt: now,
		}, model.Limits{}); err != nil {
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

func TestRunsReturnsEveryAttemptOldestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 is given the id that sorts *last*, to prove Runs sorts by
	// attempt rather than by id. It cannot also be started last: a task
	// has at most one live run (schema.go's task_run_open_task), so the
	// earlier attempt has to have finished before the later one starts,
	// which is the order a real dispatch produces them in anyway.
	if err := store.StartRun(ctx, model.Run{
		ID: "r-second-by-id", TaskID: "a1b2", Sandbox: "s1",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r-second-by-id", now.Add(30*time.Minute), "failed", "build error"); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r-first-by-id", TaskID: "a1b2", Sandbox: "s2",
		Attempt: 2, StartedAt: now.Add(time.Hour),
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	// Attempt 2 is left unfinished, to prove a still-running attempt comes
	// back with a nil FinishedAt and empty Outcome rather than erroring.

	runs, err := store.Runs(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want 2", runs)
	}
	if runs[0].Attempt != 1 || runs[0].Outcome != "failed" || runs[0].Detail != "build error" {
		t.Fatalf("runs[0] = %+v, want attempt 1, failed, build error", runs[0])
	}
	if runs[0].FinishedAt == nil {
		t.Fatalf("runs[0].FinishedAt = nil, want the time FinishRun recorded")
	}
	if runs[1].Attempt != 2 || runs[1].Outcome != "" || runs[1].FinishedAt != nil {
		t.Fatalf("runs[1] = %+v, want attempt 2, still running", runs[1])
	}

	if none, err := store.Runs(ctx, "nonexistent"); err != nil || len(none) != 0 {
		t.Fatalf("runs for a nonexistent task = %+v (%v), want none", none, err)
	}
}

func TestRunTranscriptRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: "a1b2", Sandbox: "s1",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	// A started but not yet finished attempt exists, but has nothing
	// recorded for it yet.
	transcript, found, err := store.RunTranscript(ctx, "a1b2", 1)
	if err != nil || !found || transcript != "" {
		t.Fatalf("RunTranscript before FinishRun = (%q, %v, %v), want (\"\", true, nil)", transcript, found, err)
	}

	if err := store.FinishRun(ctx, "r1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunTranscript(ctx, "r1", "> read_file(out.txt)\nPONG\n\nfound it"); err != nil {
		t.Fatal(err)
	}

	transcript, found, err = store.RunTranscript(ctx, "a1b2", 1)
	if err != nil || !found || transcript != "> read_file(out.txt)\nPONG\n\nfound it" {
		t.Fatalf("RunTranscript after SetRunTranscript = (%q, %v, %v)", transcript, found, err)
	}

	if _, found, err := store.RunTranscript(ctx, "a1b2", 2); err != nil || found {
		t.Fatalf("RunTranscript for a nonexistent attempt = (found %v, %v), want (false, nil)", found, err)
	}
	if _, found, err := store.RunTranscript(ctx, "nonexistent", 1); err != nil || found {
		t.Fatalf("RunTranscript for a nonexistent task = (found %v, %v), want (false, nil)", found, err)
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
		ID: "r1", TaskID: "a1b2", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
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
		ID: "r1", TaskID: "a1b2", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
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
		ID: "r1", TaskID: "a1b2", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
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
		ID: "r1", TaskID: "a1b2", Sandbox: "sandbox-0",
		Attempt: 1, StartedAt: now,
	}, model.Limits{}); err != nil {
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

// TestPullRequestEventTimestampsRoundTrip covers PrOpenedAt/PrMergedAt/
// PrClosedAt (bwsalmon/agents#493) the same way TestObservationBaselinesRoundTrip
// already covers BaselineCommentID -- a plain round trip through Observe/
// GetObservation, since nothing about these three needs the derivation
// TestStateIsDerivedThroughEveryTransition exercises for ClosedAt.
func TestPullRequestEventTimestampsRoundTrip(t *testing.T) {
	store, _, ctx := openStore(t)
	opened := now.Add(-2 * time.Hour)
	merged := now
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", PrOpenedAt: &opened, PrMergedAt: &merged, ObservedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation: %v", err)
	}
	if got.PrOpenedAt == nil || !got.PrOpenedAt.Equal(opened) {
		t.Errorf("PrOpenedAt did not survive: %+v", got.PrOpenedAt)
	}
	if got.PrMergedAt == nil || !got.PrMergedAt.Equal(merged) {
		t.Errorf("PrMergedAt did not survive: %+v", got.PrMergedAt)
	}
	if got.PrClosedAt != nil {
		t.Errorf("expected PrClosedAt to stay nil, got %+v", got.PrClosedAt)
	}
}

// MergeQueueRefreshedAt is the merge queue's record that it has already
// merged a stale head's base into it once (orchestrator.refreshStaleHead),
// and it is only worth having because it is durable: losing it costs a
// repeated write to GitHub rather than another window of waiting, so a
// process that restarted in a loop would merge in a loop. That makes this
// round trip the property, not an incidental one.
func TestMergeQueueRefreshedAtRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	refreshed := now.Add(-time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: "a1b2", MergeQueueRefreshedAt: &refreshed, ObservedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation: %v", err)
	}
	if got.MergeQueueRefreshedAt == nil || !got.MergeQueueRefreshedAt.Equal(refreshed) {
		t.Errorf("MergeQueueRefreshedAt did not survive: %+v", got.MergeQueueRefreshedAt)
	}
	if got.MergeQueueBlockedAt != nil {
		t.Errorf("refreshing is not giving up: MergeQueueBlockedAt = %+v, want nil", got.MergeQueueBlockedAt)
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

// The two tests above pin the mechanism with two writers. A real
// deployment's daemon, ui, and git-proxy processes all share one store
// (cmd/grain/daemon.go runs all three against the same *sql.DB), so it is
// worth also knowing the same guarantee holds at higher fan-out: every
// writer's change lands somewhere, none silently vanishes, and nothing
// under database/sql itself races even though every one of these
// goroutines shares the one *sql.DB Store wraps.
func TestManyConcurrentWritersEachLandTheirOwnTask(t *testing.T) {
	store, _, ctx := openStore(t)

	const writers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("task-%02d", i)
			errs[i] = store.PutTask(ctx, task(id, true))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	for i := 0; i < writers; i++ {
		id := fmt.Sprintf("task-%02d", i)
		got, err := store.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got == nil {
			t.Errorf("task %s was never written -- one of %d concurrent writers lost its change", id, writers)
		}
	}
}

// The same "one winner at a time, nothing lost" guarantee
// TestConcurrentUpdatesBothLand and TestAConflictRollsBackChildRowsToo
// check with two writers, at a fan-out closer to what would actually
// force Store.write's retry loop to run more than once for some of them.
func TestManyConcurrentUpdatesToTheSameTaskAllLand(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			tag := fmt.Sprintf("from-%02d", i)
			errs[i] = store.UpdateTask(ctx, "a1b2", func(tk *model.Task) error {
				tk.Tags = append(tk.Tags, tag)
				return nil
			})
		}(i)
	}
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
	if len(got.Tags) != writers {
		t.Fatalf("tags = %v (%d), want %d -- a retry that rewrote over another winner would have lost one",
			got.Tags, len(got.Tags), writers)
	}
	seen := map[string]bool{}
	for _, tag := range got.Tags {
		seen[tag] = true
	}
	for i := 0; i < writers; i++ {
		tag := fmt.Sprintf("from-%02d", i)
		if !seen[tag] {
			t.Errorf("missing tag %q", tag)
		}
	}
}

// A store's read side under the same concurrent writers: GetTask,
// Ready, and OccupiedSlots must never see a half-written row, and none
// of them may return an error just because a writer is mid-transaction
// at the moment they run -- database/sql already guarantees the first;
// this is what tests it against Store's own queries rather than taking
// it on faith.
func TestReadsSeeConsistentStateWhileWritersAreActive(t *testing.T) {
	store, _, ctx := openStore(t)

	const (
		writers = 8
		readers = 8
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	writeErrs := make([]error, writers)
	readErrs := make([]error, readers)

	wg.Add(writers + readers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("task-%02d", i)
			if err := store.PutTask(ctx, task(id, true)); err != nil {
				writeErrs[i] = err
				return
			}
			writeErrs[i] = store.StartRun(ctx, model.Run{
				ID: "r-" + id, TaskID: id,
				Sandbox: fmt.Sprintf("sandbox-%d", i), Attempt: 1, StartedAt: now,
			}, model.Limits{})
		}(i)
	}
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				if _, err := store.Ready(ctx); err != nil {
					readErrs[i] = fmt.Errorf("Ready: %w", err)
					return
				}
				if _, err := store.LiveRunCount(ctx); err != nil {
					readErrs[i] = fmt.Errorf("LiveRunCount: %w", err)
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range writeErrs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	for i, err := range readErrs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
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

// TestFailureStreakCountsConsecutiveFailuresAndStopsAtASuccess is
// Store.FailureStreak's own baseline: it walks task_run newest-first and
// counts up until (and not including) the first "succeeded" outcome, the
// same cutoff model.MaxConsecutiveFailures compares against.
func TestFailureStreakCountsConsecutiveFailuresAndStopsAtASuccess(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	if streak, err := store.FailureStreak(ctx, "a1b2"); err != nil {
		t.Fatal(err)
	} else if streak != nil {
		t.Fatalf("FailureStreak with no runs = %+v, want nil", streak)
	}

	starts := []time.Time{
		now, now.Add(time.Hour), now.Add(2 * time.Hour), now.Add(3 * time.Hour),
	}
	outcomes := []string{"succeeded", "failed", "failed", "failed"}
	for i, at := range starts {
		id := fmt.Sprintf("r%d", i+1)
		if err := store.StartRun(ctx, model.Run{
			ID: id, TaskID: "a1b2", Sandbox: "s1", Attempt: i + 1, StartedAt: at,
		}, model.Limits{}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, at.Add(time.Minute), outcomes[i], "boom"); err != nil {
			t.Fatal(err)
		}
	}

	streak, err := store.FailureStreak(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if streak == nil || streak.Count != 3 {
		t.Fatalf("FailureStreak = %+v, want Count 3 (the three failures after the one success)", streak)
	}
	if streak.LastOutcome != "failed" || streak.LastDetail != "boom" {
		t.Fatalf("FailureStreak = %+v, want the most recent run's own outcome and detail", streak)
	}
}

// TestFailureStreakIsNarrowedByARetryRequest covers bwsalmon/agents#403's
// "Retry" button end to end at the store layer: RetryRequestedAt does not
// delete or rewrite any task_run row, it only tells FailureStreak to stop
// counting once it reaches a run that started at or before the retry
// request -- so a run that already failed before the retry stays in the
// record (Runs, GetTask's own attempt history) but no longer contributes
// to the count that trips StateFailed.
func TestFailureStreakIsNarrowedByARetryRequest(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < model.MaxConsecutiveFailures; i++ {
		id := fmt.Sprintf("r%d", i+1)
		at := now.Add(time.Duration(i) * time.Hour)
		if err := store.StartRun(ctx, model.Run{
			ID: id, TaskID: "a1b2", Sandbox: "s1", Attempt: i + 1, StartedAt: at,
		}, model.Limits{}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, at.Add(time.Minute), "failed", "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if streak, err := store.FailureStreak(ctx, "a1b2"); err != nil {
		t.Fatal(err)
	} else if streak == nil || streak.Count != model.MaxConsecutiveFailures {
		t.Fatalf("FailureStreak before retrying = %+v, want Count %d", streak, model.MaxConsecutiveFailures)
	}

	retryAt := now.Add(model.MaxConsecutiveFailures * time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: "a1b2", RetryRequestedAt: &retryAt}); err != nil {
		t.Fatal(err)
	}

	streak, err := store.FailureStreak(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if streak == nil || streak.Count != 0 {
		t.Fatalf("FailureStreak after retrying = %+v, want Count 0: every prior failure started before the retry request", streak)
	}
	if streak.LastOutcome != "failed" {
		t.Fatalf("FailureStreak after retrying = %+v, want LastOutcome still reporting the last run's own outcome", streak)
	}

	runs, err := store.Runs(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != model.MaxConsecutiveFailures {
		t.Fatalf("Runs after retrying = %d, want the retry to narrow the streak without deleting any run", len(runs))
	}

	// A run that starts after the retry request still counts: retrying
	// forgives the past, it does not disable the cap going forward.
	freshID := "r" + fmt.Sprint(model.MaxConsecutiveFailures+1)
	freshAt := retryAt.Add(time.Hour)
	if err := store.StartRun(ctx, model.Run{
		ID: freshID, TaskID: "a1b2", Sandbox: "s1",
		Attempt: model.MaxConsecutiveFailures + 1, StartedAt: freshAt,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, freshID, freshAt.Add(time.Minute), "failed", "boom again"); err != nil {
		t.Fatal(err)
	}
	if streak, err := store.FailureStreak(ctx, "a1b2"); err != nil {
		t.Fatal(err)
	} else if streak == nil || streak.Count != 1 {
		t.Fatalf("FailureStreak after a fresh post-retry failure = %+v, want Count 1", streak)
	}
}

// TestAPausedRunCountsAgainstNeitherStreak is model.PausedOutcome's own
// rule, held against both implementations of it at once: Store.
// FailureStreak's walk in Go, and task_streak's WHERE clause in SQL
// (which task_state reads, and which State answers out of). A run grain
// stopped because the deployment's agent had no budget left says nothing
// about the task it was given, so it neither adds to the streak nor
// clears the failures before it -- otherwise a single provider outage
// would spend the retry budget of every task that happened to be running
// when it began.
func TestAPausedRunCountsAgainstNeitherStreak(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	// Every run this task has ever had was stopped by a usage limit.
	for i := 0; i < model.MaxConsecutiveFailures+1; i++ {
		id := fmt.Sprintf("r%d", i+1)
		at := now.Add(time.Duration(i) * time.Hour)
		if err := store.StartRun(ctx, model.Run{
			ID: id, TaskID: "a1b2", Sandbox: "s1", Attempt: i + 1, StartedAt: at,
		}, model.Limits{}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, at.Add(time.Minute), model.PausedOutcome, "usage limit"); err != nil {
			t.Fatal(err)
		}
	}

	streak, err := store.FailureStreak(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if streak == nil || streak.Count != 0 {
		t.Fatalf("FailureStreak over paused runs = %+v, want Count 0", streak)
	}
	if state, err := store.State(ctx, "a1b2"); err != nil {
		t.Fatal(err)
	} else if state != model.StateQueued {
		t.Fatalf("state after %d paused runs = %q, want %q: task_streak must skip them too",
			model.MaxConsecutiveFailures+1, state, model.StateQueued)
	}

	// And a paused run is not a success either: the failures before it
	// still stand.
	failedAt := now.Add(100 * time.Hour)
	if err := store.StartRun(ctx, model.Run{
		ID: "f1", TaskID: "a1b2", Sandbox: "s1", Attempt: 100, StartedAt: failedAt,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "f1", failedAt.Add(time.Minute), "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	pausedAt := failedAt.Add(time.Hour)
	if err := store.StartRun(ctx, model.Run{
		ID: "p1", TaskID: "a1b2", Sandbox: "s1", Attempt: 101, StartedAt: pausedAt,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "p1", pausedAt.Add(time.Minute), model.PausedOutcome, "usage limit"); err != nil {
		t.Fatal(err)
	}
	if streak, err := store.FailureStreak(ctx, "a1b2"); err != nil {
		t.Fatal(err)
	} else if streak == nil || streak.Count != 1 {
		t.Fatalf("FailureStreak = %+v, want Count 1: a pause neither counts nor forgives", streak)
	}
}

// TestStartRunRejectsASecondLiveRunOnTheSameTask pins what is left of
// the bwsalmon/agents#434 guard once slots stopped existing. It used to
// be one live run per slot, catching two overlapping dispatch.Cycle
// callers that both decided the same slot was free; the concurrency
// limit is now enforced inside StartRun's own transaction instead, and
// what this index still rules out is a task running twice at once --
// which task_state assumes when it reads a live run as 'running', and
// which a REPLACE INTO would have hidden by silently overwriting the
// first run's row.
func TestStartRunRejectsASecondLiveRunOnTheSameTask(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}

	first := model.Run{ID: "a1b2-1", TaskID: "a1b2", Sandbox: "a1b2-1", Attempt: 1, StartedAt: now}
	if err := store.StartRun(ctx, first, model.Limits{}); err != nil {
		t.Fatalf("first StartRun for an idle task: %v", err)
	}

	second := model.Run{ID: "a1b2-2", TaskID: "a1b2", Sandbox: "a1b2-2", Attempt: 2, StartedAt: now}
	if err := store.StartRun(ctx, second, model.Limits{}); err == nil {
		t.Fatal("a second live run on a task that already has one should have failed, not landed")
	}

	live, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live runs = %d, want exactly 1 -- the rejected second run must not have landed", live)
	}
	streak, err := store.FailureStreak(ctx, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if streak != nil {
		t.Fatalf("first run's row was overwritten: FailureStreak(a1b2) = %+v, want nil (never finished)", streak)
	}

	// Once the first attempt has finished, a second one starts fine --
	// the index guards against an overlap, not against a retry.
	if err := store.FinishRun(ctx, "a1b2-1", now.Add(time.Hour), "failed", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, second, model.Limits{}); err != nil {
		t.Fatalf("StartRun for a second attempt after the first finished: %v", err)
	}
}

// TestStartRunRefusesToExceedTheConcurrencyLimit pins the guard that
// replaced the one-run-per-slot index as the thing keeping two
// overlapping dispatch.Cycle callers from over-dispatching. Cycle reads
// the live-run count and task_ready outside any one transaction, so
// nothing in Go stops both from seeing the same headroom; StartRun
// re-counts inside the transaction that records the run, so the second
// one loses with model.ErrAtCapacity rather than landing an extra run.
func TestStartRunRefusesToExceedTheConcurrencyLimit(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, id := range []string{"a1b2", "c3d4", "e5f6"} {
		if err := store.PutTask(ctx, task(id, true)); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{"a1b2", "c3d4"} {
		run := model.Run{ID: id + "-1", TaskID: id, Sandbox: id + "-1", Attempt: 1, StartedAt: now}
		if err := store.StartRun(ctx, run, model.Limits{Workers: 2}); err != nil {
			t.Fatalf("StartRun for %s within the limit: %v", id, err)
		}
	}

	third := model.Run{ID: "e5f6-1", TaskID: "e5f6", Sandbox: "e5f6-1", Attempt: 1, StartedAt: now}
	err := store.StartRun(ctx, third, model.Limits{Workers: 2})
	if !errors.Is(err, model.ErrAtCapacity) {
		t.Fatalf("StartRun past the limit = %v, want ErrAtCapacity", err)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 2 {
		t.Fatalf("live runs = %d (%v), want exactly 2 -- the refused run must not have landed", live, err)
	}

	// Finishing one makes room for exactly one more.
	if err := store.FinishRun(ctx, "a1b2-1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, third, model.Limits{Workers: 2}); err != nil {
		t.Fatalf("StartRun into freed capacity: %v", err)
	}

	// A limit of zero or less means "no limit of mine to enforce" -- what
	// a caller with its own reason to record a run passes.
	fourth := model.Run{ID: "a1b2-2", TaskID: "a1b2", Sandbox: "a1b2-2", Attempt: 2, StartedAt: now.Add(2 * time.Hour)}
	if err := store.StartRun(ctx, fourth, model.Limits{}); err != nil {
		t.Fatalf("StartRun with no limit: %v", err)
	}
}

// fixTask is a task the merge queue filed to repair a stuck queue head
// (Origin.Reason == ReasonFix) -- the one kind whose runs count against
// model.Limits.Mergers rather than against the worker ceiling.
func fixTask(id string) model.Task {
	t := task(id, true)
	t.Origin.Reason = model.ReasonFix
	return t
}

// A repair is a merger too, and it is not a ReasonFix task: it is
// another attempt of whatever task the merge queue sent back to work on
// its own branch, whose origin_reason is whatever it was filed as. The
// store answers "is this run a merger" off both halves at once
// (mergerTaskSQL), so a repair draws on the capacity kept back for
// exactly this rather than queueing behind ordinary work.
func TestARepairInFlightCountsAsAMergerThoughItsReasonIsNot(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, id := range []string{"w1", "r1"} {
		if err := store.PutTask(ctx, task(id, true)); err != nil {
			t.Fatal(err)
		}
	}
	asked := now
	if err := store.Observe(ctx, model.Observation{TaskID: "r1", MergeQueueRepairAt: &asked}); err != nil {
		t.Fatal(err)
	}

	ready, err := store.ReadyMergers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0] != "r1" {
		t.Fatalf("ReadyMergers = %v, want just the task being repaired", ready)
	}

	limits := model.Limits{Workers: 1, Mergers: 1}
	start := func(id string) error {
		return store.StartRun(ctx, model.Run{
			ID: id + "-1", TaskID: id, Sandbox: id + "-1", Attempt: 1, StartedAt: now,
		}, limits)
	}
	if err := start("w1"); err != nil {
		t.Fatalf("StartRun for the one worker slot: %v", err)
	}
	// The worker ceiling is full, so this only starts if the repair is
	// classified as a merger.
	if err := start("r1"); err != nil {
		t.Fatalf("StartRun for a repair with the worker ceiling full: %v", err)
	}
	counts, err := store.LiveRunCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Workers != 1 || counts.Mergers != 1 {
		t.Fatalf("LiveRunCounts = %+v, want one worker and one merger", counts)
	}

	// And once the repair has completed, the same task is ordinary work
	// again -- nothing holds merger capacity open for a finished repair.
	finished := now.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: "r1", MergeQueueRepairAt: &asked, CompletedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.LiveRunCounts(ctx); err != nil || counts.Mergers != 0 || counts.Workers != 2 {
		t.Fatalf("LiveRunCounts once the repair completed = %+v (%v), want two workers", counts, err)
	}
}

// TestStartRunCountsMergersAgainstTheirOwnHalfOfTheLimit is
// grain/task-63 at the only place the limit is really enforced: inside
// the transaction that records the run. Ordinary work stops at
// Limits.Workers even with merger capacity free; a merge-queue fix task
// takes that free capacity *and* a spare worker slot; and nothing of
// either kind gets past the sum of the two.
func TestStartRunCountsMergersAgainstTheirOwnHalfOfTheLimit(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, id := range []string{"w1", "w2", "f1", "f2"} {
		tk := task(id, true)
		if id[0] == 'f' {
			tk = fixTask(id)
		}
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	limits := model.Limits{Workers: 1, Mergers: 2}
	start := func(id string) error {
		return store.StartRun(ctx, model.Run{
			ID: id + "-1", TaskID: id, Sandbox: id + "-1", Attempt: 1, StartedAt: now,
		}, limits)
	}

	if err := start("w1"); err != nil {
		t.Fatalf("StartRun for the one worker slot: %v", err)
	}
	if err := start("w2"); !errors.Is(err, model.ErrAtCapacity) {
		t.Fatalf("StartRun for a second worker = %v, want ErrAtCapacity -- merger capacity is not a general pool", err)
	}
	// Both fix tasks fit: one in a merger slot, one in the other.
	if err := start("f1"); err != nil {
		t.Fatalf("StartRun for the first fix task: %v", err)
	}
	if err := start("f2"); err != nil {
		t.Fatalf("StartRun for the second fix task: %v", err)
	}

	counts, err := store.LiveRunCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Workers != 1 || counts.Mergers != 2 {
		t.Fatalf("LiveRunCounts = %+v, want one worker and two mergers", counts)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != counts.Total() {
		t.Fatalf("LiveRunCount = %d (%v), want the same %d LiveRunCounts totals", live, err, counts.Total())
	}

	// Everything together is capped at the sum, so with three live there
	// is nothing left for a fix task either.
	if err := store.PutTask(ctx, fixTask("f3")); err != nil {
		t.Fatal(err)
	}
	if err := start("f3"); !errors.Is(err, model.ErrAtCapacity) {
		t.Fatalf("StartRun past the sum of both limits = %v, want ErrAtCapacity", err)
	}

	// Freeing the worker slot frees it for a merger too -- that is the
	// direction the asymmetry runs.
	if err := store.FinishRun(ctx, "w1-1", now.Add(time.Hour), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := start("f3"); err != nil {
		t.Fatalf("StartRun for a fix task into a freed worker slot: %v", err)
	}
	if counts, err := store.LiveRunCounts(ctx); err != nil || counts.Mergers != 3 || counts.Workers != 0 {
		t.Fatalf("LiveRunCounts = %+v (%v), want three mergers and no worker", counts, err)
	}
}

// TestInitMigratesAnExistingDatabaseMissingWorkerMergerColumns is
// grain/task-63's own migration: a database whose grain_config still has
// the single max_concurrent count keeps that count as its worker limit
// (renamed to max_workers, the old column dropped) and gains
// model.DefaultMaxMergers of merge capacity it never had a way to
// express.
func TestInitMigratesAnExistingDatabaseMissingWorkerMergerColumns(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-split grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`) "+
			"VALUES (1,30000,3,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','')"); err != nil {
		t.Fatalf("seeding a pre-split config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing max_workers/max_mergers: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.MaxWorkers != 3 {
		t.Fatalf("MaxWorkers after migrating = %d, want the 3 the old max_concurrent held", got.MaxWorkers)
	}
	if got.MaxMergers != model.DefaultMaxMergers {
		t.Fatalf("MaxMergers after migrating = %d, want DefaultMaxMergers (%d)", got.MaxMergers, model.DefaultMaxMergers)
	}

	// The old column is gone, not merely ignored -- PutConfig stops
	// supplying it, so a NOT NULL column left behind would fail every
	// later write.
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	after, err := store.GetConfig(ctx)
	if err != nil || after == nil {
		t.Fatalf("get after migrating: (%+v, %v)", after, err)
	}
	if !reflect.DeepEqual(*after, want) {
		t.Fatalf("got %+v, want %+v", *after, want)
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
		PollInterval: 30 * time.Second, MaxWorkers: 2,
		// AgentFramework is named explicitly rather than left zero
		// because it is the one Config field that never reads back
		// exactly as written: GetConfig runs it through
		// model.NormalizeAgentFramework, so "" and the legacy "gemini"
		// both come back as AgentFrameworkAntigravity (see
		// TestGetConfigNormalizesTheLegacyAgentFrameworkName below).
		AgentFramework: model.AgentFrameworkAntigravity,
		GeminiModel:    "gemini-2.5-pro", ClaudeModel: "claude-sonnet-5", CodexModel: "gpt-5.1-codex",
		MaxAgentTurns: 40,
		GitHubHost: "github.com", GitHubInsecureHTTP: false,
		GCPProject: "grain-prod", GCPServiceAccountEmail: "agent@grain-prod.iam.gserviceaccount.com",
		TargetRepos:         []string{"acme/widgets", "acme/gadgets"},
		SandboxCPUs:         4,
		SandboxMemoryMB:     8192,
		SandboxDiskGB:       40,
		DefaultCapabilities: []string{"gcp-key", "github-sandbox"},
		EnvironmentName:     "staging",
		// Multi-line on purpose: the deployment's standing instructions
		// are the one Config field that is prose (grain/task-114), and a
		// column that stored only its first line would still round-trip a
		// one-line value.
		PromptExtension: "Run `make lint` before you push.\nSay what you tried.",
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

// grain/task-24: one repo's own configuration, the per-repo layer of
// what grain_config says deployment-wide. Round-trips, replaces rather
// than accumulating, and lists only repos that actually say something.
func TestRepoConfigRoundTripsAndReplaces(t *testing.T) {
	store, _, ctx := openStore(t)
	widgets := model.RepoRef{Owner: "acme", Name: "widgets"}
	gadgets := model.RepoRef{Owner: "acme", Name: "gadgets"}

	// A repo nobody has configured reads back as nil, with no error --
	// the same "nothing configured one yet" GetQualificationPlan gives.
	got, err := store.GetRepoConfig(ctx, widgets)
	if err != nil {
		t.Fatalf("get on an unconfigured repo: %v", err)
	}
	if got != nil {
		t.Fatalf("get on an unconfigured repo = %+v, want nil", got)
	}

	want := model.RepoConfig{
		Repo:                widgets,
		DefaultCapabilities: []string{"gcp-key", "github-sandbox"},
		// The second field this row carries (grain/task-114), stored
		// beside the first rather than instead of it: both are written
		// through the same wholesale replace, so a put that named only
		// one of them and dropped the other is exactly what this pins.
		PromptExtension: "Widgets keeps its migrations in db/.",
	}
	if err := store.PutRepoConfig(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err = store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}

	// Replaced wholesale, not merged into.
	want.DefaultCapabilities = []string{"gemini-key"}
	if err := store.PutRepoConfig(ctx, want); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err = store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}

	if err := store.PutRepoConfig(ctx, model.RepoConfig{
		Repo: gadgets, DefaultCapabilities: []string{"gcp-key"},
	}); err != nil {
		t.Fatalf("put for a second repo: %v", err)
	}
	list, err := store.ListRepoConfigs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantList := []model.RepoConfig{
		{Repo: gadgets, DefaultCapabilities: []string{"gcp-key"}},
		{Repo: widgets, DefaultCapabilities: []string{"gemini-key"},
			PromptExtension: "Widgets keeps its migrations in db/."},
	}
	if !reflect.DeepEqual(list, wantList) {
		t.Fatalf("list = %+v, want %+v, sorted by repo", list, wantList)
	}
}

// A config that says nothing leaves no row: "has a row" and "has
// something of its own to say" are one fact, so a repo whose last
// default capability is unticked stops being listed at all rather than
// lingering as an empty entry (model.RepoConfig.Empty).
func TestPutRepoConfigWithNothingToSayDeletesTheRow(t *testing.T) {
	store, _, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutRepoConfig(ctx, model.RepoConfig{
		Repo: repo, DefaultCapabilities: []string{"gcp-key"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.PutRepoConfig(ctx, model.RepoConfig{Repo: repo}); err != nil {
		t.Fatalf("put with nothing to say: %v", err)
	}
	got, err := store.GetRepoConfig(ctx, repo)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("get = %+v, want nil", got)
	}
	list, err := store.ListRepoConfigs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want no rows left", list)
	}
}

// The other half of that rule, since RepoConfig.Empty gained a second
// term with grain/task-114: standing instructions alone are something to
// say, so a repo that has unticked its last default capability but still
// carries a prompt extension keeps its row -- and stays listed, which is
// what puts it on the repos page and in `grain repo list` at all. A row
// dropped here would leave text that every run against the repo is still
// told but nothing admits exists.
func TestPutRepoConfigKeepsARowThatOnlyHasStandingInstructions(t *testing.T) {
	store, _, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	want := model.RepoConfig{Repo: repo, PromptExtension: "Read db/README.md before touching migrations."}
	if err := store.PutRepoConfig(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetRepoConfig(ctx, repo)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v), want the row to have survived", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
	list, err := store.ListRepoConfigs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !reflect.DeepEqual(list, []model.RepoConfig{want}) {
		t.Fatalf("list = %+v, want %+v", list, []model.RepoConfig{want})
	}

	// A setup command alone is the third such term (grain/task-154), and
	// keeps the row for the same reason: it is run in every checkout made
	// for this repo, so a row dropped here would leave a command grain
	// still runs and nothing admits exists.
	setupOnly := model.RepoConfig{Repo: repo, SetupCommand: "make deps"}
	if err := store.PutRepoConfig(ctx, setupOnly); err != nil {
		t.Fatalf("put with only a setup command: %v", err)
	}
	if got, err := store.GetRepoConfig(ctx, repo); err != nil || got == nil ||
		!reflect.DeepEqual(*got, setupOnly) {
		t.Fatalf("get = (%+v, %v), want %+v", got, err, setupOnly)
	}

	// And clearing that last thing it had to say does delete it, exactly
	// as unticking a last capability does.
	if err := store.PutRepoConfig(ctx, model.RepoConfig{Repo: repo}); err != nil {
		t.Fatalf("put with nothing to say: %v", err)
	}
	if got, err := store.GetRepoConfig(ctx, repo); err != nil || got != nil {
		t.Fatalf("get after clearing = (%+v, %v), want nil", got, err)
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
	updated.MaxWorkers = 1
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

// grain/task-202: filing a task remembers which end of the backlog it
// joined, and does it through SetNewestFirst rather than through a
// read-modify-write of the whole row -- so it must move that one column
// and leave everything else stored exactly as it was, including whatever
// was written between the read and this write.
func TestSetNewestFirstLeavesEveryOtherSettingAlone(t *testing.T) {
	store, _, ctx := openStore(t)
	stored := testConfig()
	if err := store.PutConfig(ctx, stored); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.SetNewestFirst(ctx, true); err != nil {
		t.Fatalf("remembering the front of the backlog: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	want := stored
	want.NewestFirst = true
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}

	if err := store.SetNewestFirst(ctx, false); err != nil {
		t.Fatalf("remembering the end of the backlog: %v", err)
	}
	if got, err = store.GetConfig(ctx); err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, stored) {
		t.Fatalf("got %+v, want the original %+v back", *got, stored)
	}
}

// A deployment nothing has ever configured has no row to remember it in,
// and this must not write one: the row's existence is what
// ui.Settings.Configured reports, and a task filed before anyone opened
// Settings must not answer "has this deployment been configured" on an
// operator's behalf.
func TestSetNewestFirstWritesNoConfigRowOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.SetNewestFirst(ctx, true); err != nil {
		t.Fatalf("remembering with nothing stored: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) still, got (%+v, %v)", got, err)
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
	if got.GeminiModel != "gemini-2.5-pro" || len(got.TargetRepos) != 0 || got.MaxWorkers != 2 {
		t.Fatalf("got %+v, want the pre-existing row intact, targetRepos empty, and maxWorkers migrated from the old two-name slots column", got)
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

func TestInitMigratesAnExistingDatabaseMissingTranscript(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_run`"+` (
  `+"`id`"+`          TEXT     NOT NULL,
  `+"`task_id`"+`     TEXT     NOT NULL,
  `+"`slot`"+`        TEXT     NOT NULL,
  `+"`sandbox`"+`     TEXT     NOT NULL,
  `+"`unit`"+`        TEXT     NULL,
  `+"`attempt`"+`     INTEGER  NOT NULL,
  `+"`started_at`"+`  DATETIME NOT NULL,
  `+"`finished_at`"+` DATETIME NULL,
  `+"`outcome`"+`     TEXT     NULL,
  `+"`detail`"+`      TEXT     NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#446 task_run table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_run` (`id`,`task_id`,`slot`,`sandbox`,`attempt`,`started_at`,`outcome`,`detail`) "+
			"VALUES ('r1','a1b2','s1','s1',1,?,'succeeded',NULL)", now); err != nil {
		t.Fatalf("seeding a pre-#446 task_run row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task_run.transcript: %v", err)
	}

	// The pre-existing row is still readable, transcript-less...
	transcript, found, err := store.RunTranscript(ctx, "a1b2", 1)
	if err != nil || !found || transcript != "" {
		t.Fatalf("RunTranscript after migrating = (%q, %v, %v), want (\"\", true, nil)", transcript, found, err)
	}
	// ...and SetRunTranscript can now actually make one durable, which is
	// the whole bug this migration fixes.
	if err := store.SetRunTranscript(ctx, "r1", "found it"); err != nil {
		t.Fatalf("set after migrating: %v", err)
	}
	if transcript, _, err := store.RunTranscript(ctx, "a1b2", 1); err != nil || transcript != "found it" {
		t.Fatalf("RunTranscript after set = (%q, %v), want (\"found it\", nil)", transcript, err)
	}
}

// A store written before task_run.prompt existed keeps working: Init
// adds the column (Store.ensureTaskRunPromptColumn), the runs recorded
// before it read back with no prompt rather than failing the query, and
// the next run can record one for real -- the same shape the transcript
// migration above pins for the column before it.
func TestInitMigratesAnExistingDatabaseMissingPrompt(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_run`"+` (
  `+"`id`"+`               TEXT     NOT NULL,
  `+"`task_id`"+`          TEXT     NOT NULL,
  `+"`sandbox`"+`          TEXT     NOT NULL,
  `+"`unit`"+`             TEXT     NULL,
  `+"`attempt`"+`          INTEGER  NOT NULL,
  `+"`started_at`"+`       DATETIME NOT NULL,
  `+"`agent_started_at`"+` DATETIME NULL,
  `+"`finished_at`"+`      DATETIME NULL,
  `+"`outcome`"+`          TEXT     NULL,
  `+"`detail`"+`           TEXT     NULL,
  `+"`transcript`"+`       TEXT     NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the task_run table as it was before the prompt column: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_run` (`id`,`task_id`,`sandbox`,`attempt`,`started_at`,`outcome`) "+
			"VALUES ('r1','a1b2','s1',1,?,'succeeded')", now); err != nil {
		t.Fatalf("seeding a run recorded before the column existed: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task_run.prompt: %v", err)
	}

	prompt, found, err := store.RunPrompt(ctx, "a1b2", 1)
	if err != nil || !found || prompt != "" {
		t.Fatalf("RunPrompt after migrating = (%q, %v, %v), want (\"\", true, nil)", prompt, found, err)
	}
	if err := store.SetRunPrompt(ctx, "r1", "what the agent was told"); err != nil {
		t.Fatalf("set after migrating: %v", err)
	}
	if prompt, _, err := store.RunPrompt(ctx, "a1b2", 1); err != nil || prompt != "what the agent was told" {
		t.Fatalf("RunPrompt after set = (%q, %v), want the prompt now durable", prompt, err)
	}
}

// The merge queue's own two records -- having refreshed a stale head
// (task_observation.merge_queue_refreshed_at) and having sent a task back
// to repair its own branch (merge_queue_repair_at) -- were each added to
// a table every existing deployment already has rows in, and CREATE TABLE
// IF NOT EXISTS never alters one of those. So this simulates a database
// built without either column, directly, and checks Store.Init's own
// migration steps (ensureTaskObservationRefreshedColumn,
// ensureTaskObservationRepairColumn) both leave the pre-existing
// observation readable and make the new fields durable afterwards.
func TestInitMigratesAnExistingDatabaseMissingTheMergeQueueColumns(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_observation`"+` (
  `+"`task_id`"+`                     TEXT     NOT NULL,
  `+"`closed_at`"+`                   DATETIME NULL,
  `+"`completed_at`"+`                DATETIME NULL,
  `+"`pending_question_comment_id`"+` INTEGER  NULL,
  `+"`baseline_comment_id`"+`         INTEGER  NULL,
  `+"`merge_queue_blocked_at`"+`      DATETIME NULL,
  `+"`observed_at`"+`                 DATETIME NULL,
  `+"`retry_requested_at`"+`          DATETIME NULL,
  `+"`pr_opened_at`"+`                DATETIME NULL,
  `+"`pr_merged_at`"+`                DATETIME NULL,
  `+"`pr_closed_at`"+`                DATETIME NULL,
  PRIMARY KEY (`+"`task_id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-106 task_observation table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_observation` (`task_id`,`completed_at`,`observed_at`) VALUES ('a1b2',?,?)",
		now, now); err != nil {
		t.Fatalf("seeding a pre-task-106 observation row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task_observation.merge_queue_refreshed_at: %v", err)
	}

	// The pre-existing row is intact, and reads back as a pull request
	// the queue has never refreshed -- which is the direction that errs
	// toward one merge attempt rather than toward never making one.
	got, err := store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation after migrating: (%+v, %v)", got, err)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(now) {
		t.Fatalf("CompletedAt after migrating = %+v, want the seeded row's own %v", got.CompletedAt, now)
	}
	if got.MergeQueueRefreshedAt != nil {
		t.Fatalf("MergeQueueRefreshedAt after migrating = %+v, want nil", got.MergeQueueRefreshedAt)
	}

	// And durable afterwards, which is the whole point: observe binds
	// this column now, so a refresh recorded after the upgrade survives a
	// restart rather than being merged all over again.
	refreshed := now.Add(time.Minute)
	if err := store.ObserveField(ctx, "a1b2", refreshed, func(o *model.Observation) {
		o.MergeQueueRefreshedAt = &refreshed
	}); err != nil {
		t.Fatalf("observe after migrating: %v", err)
	}
	got, err = store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation: (%+v, %v)", got, err)
	}
	if got.MergeQueueRefreshedAt == nil || !got.MergeQueueRefreshedAt.Equal(refreshed) {
		t.Fatalf("MergeQueueRefreshedAt = %+v, want %v", got.MergeQueueRefreshedAt, refreshed)
	}

	// The same for the repair record, whose direction of erring is the
	// same: a pull request repaired the old way, through a separate fix
	// task, reads back as never repaired rather than as already having
	// had the one repair it gets.
	if got.MergeQueueRepairAt != nil {
		t.Fatalf("MergeQueueRepairAt after migrating = %+v, want nil", got.MergeQueueRepairAt)
	}
	asked := refreshed.Add(time.Minute)
	if err := store.ObserveField(ctx, "a1b2", asked, func(o *model.Observation) {
		o.MergeQueueRepairAt = &asked
		o.CompletedAt = nil
	}); err != nil {
		t.Fatalf("observe a repair after migrating: %v", err)
	}
	got, err = store.GetObservation(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get observation: (%+v, %v)", got, err)
	}
	if !got.RepairInFlight() {
		t.Fatalf("observation after recording a repair = %+v, want it in flight", got)
	}
}

// release.merge_note -- why a release merged without a pull request of
// its own -- was added to a table a deployment already has releases in,
// and CREATE TABLE IF NOT EXISTS never alters one of those. This
// simulates a database built without the column, directly, and checks
// Store.Init's own migration step (ensureReleaseMergeNoteColumn) leaves
// the pre-existing release readable and makes the note durable
// afterwards.
func TestInitMigratesAnExistingDatabaseMissingReleaseMergeNote(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`release`"+` (
  `+"`id`"+`                INTEGER PRIMARY KEY AUTOINCREMENT,
  `+"`owner`"+`             TEXT     NOT NULL,
  `+"`repo`"+`              TEXT     NOT NULL,
  `+"`name`"+`              TEXT     NOT NULL,
  `+"`status`"+`            TEXT     NOT NULL,
  `+"`created_at`"+`        DATETIME NOT NULL,
  `+"`merged_at`"+`         DATETIME NULL,
  `+"`pull_request_url`"+`  TEXT     NULL,
  `+"`last_error`"+`        TEXT     NULL
)`); err != nil {
		t.Fatalf("creating the release table as it was before the merge_note column: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `release` (`owner`,`repo`,`name`,`status`,`created_at`) VALUES ('acme','widgets','myfeat','merge_requested',?)",
		now); err != nil {
		t.Fatalf("seeding a release recorded before the column existed: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing release.merge_note: %v", err)
	}

	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	got, err := store.GetRelease(ctx, repo, "myfeat")
	if err != nil || got == nil {
		t.Fatalf("get release after migrating: (%+v, %v)", got, err)
	}
	if got.Status != model.ReleaseMergeRequested || got.MergeNote != "" {
		t.Fatalf("got %+v, want the seeded row intact and no note", *got)
	}

	// And durable afterwards, which is the whole point: a release the
	// reconciler settles as having nothing to merge back keeps its
	// explanation across a restart, instead of the reconciler having
	// nowhere to put it.
	const note = "myfeat carried no commits main did not already have"
	if err := store.MarkReleaseNothingToMerge(ctx, got.ID, note, now.Add(time.Hour)); err != nil {
		t.Fatalf("mark nothing to merge after migrating: %v", err)
	}
	got, err = store.GetRelease(ctx, repo, "myfeat")
	if err != nil || got == nil {
		t.Fatalf("get release: (%+v, %v)", got, err)
	}
	if got.MergeNote != note || got.Status != model.ReleaseMerged {
		t.Fatalf("got %+v, want it merged with the note recorded", *got)
	}
}

// bwsalmon/agents#461: grain_config.slots (a comma-separated list of
// operator-chosen concurrency-slot names) became max_concurrent (a plain
// count) instead. This simulates a database built with the pre-#461
// column set -- slots present, max_concurrent absent -- directly rather
// than through Store, and checks Store.Init's own migration step
// (ensureConfigMaxConcurrentColumn) both backfills max_concurrent from
// however many names the old column held and drops that column, without
// disturbing the rest of the row.
func TestInitMigratesAnExistingDatabaseWithNamedSlots(t *testing.T) {
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
  `+"`target_repos`"+`                TEXT    NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#461 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`slots`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`) "+
			"VALUES (1,30000,'a,b,c','gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','')"); err != nil {
		t.Fatalf("seeding a pre-#461 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing max_concurrent: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.GeminiModel != "gemini-2.5-pro" || got.MaxWorkers != 3 {
		t.Fatalf("got %+v, want the pre-existing row intact and maxWorkers migrated from the old three-name slots column", got)
	}

	// The old column is gone, not merely ignored -- PutConfig stops
	// supplying it, so it would otherwise fail every write with a NOT
	// NULL constraint violation.
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

// --- backlog order (bwsalmon/agents#476) --------------------------------

func listedIDs(t *testing.T, store *model.Store, ctx context.Context) []string {
	t.Helper()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := make([]string, len(tasks))
	for i, tk := range tasks {
		ids[i] = tk.ID
	}
	return ids
}

// TestReadyAndListTasksFollowOrderKeyNotID puts three tasks in an order
// their ids alone would not produce, to pin down that both Ready (the
// dispatch order) and ListTasks (the backlog order a UI or CLI defaults
// to) sort by OrderKey rather than id or CreatedAt.
func TestReadyAndListTasksFollowOrderKeyNotID(t *testing.T) {
	store, _, ctx := openStore(t)
	first := task("c3d4", true)
	first.OrderKey = 10
	second := task("a1b2", true)
	second.OrderKey = 20
	third := task("z9y8", true)
	third.OrderKey = 30
	for _, tk := range []model.Task{third, first, second} {
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c3d4", "a1b2", "z9y8"}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready = %v, want %v", ready, want)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"c3d4", "a1b2", "z9y8"}) {
		t.Fatalf("ListTasks = %v, want ascending OrderKey order", got)
	}
}

func TestOrderKeyForNewTaskExtendsOrJumpsTheQueue(t *testing.T) {
	store, _, ctx := openStore(t)

	// An empty backlog has no extreme to step past, either direction.
	for _, atFront := range []bool{false, true} {
		key, err := store.OrderKeyForNewTask(ctx, atFront)
		if err != nil || key != 0 {
			t.Fatalf("OrderKeyForNewTask(empty, %v) = (%v, %v), want (0, nil)", atFront, key, err)
		}
	}

	only := task("a1b2", true)
	only.OrderKey = 100
	if err := store.PutTask(ctx, only); err != nil {
		t.Fatal(err)
	}

	back, err := store.OrderKeyForNewTask(ctx, false)
	if err != nil || back <= 100 {
		t.Fatalf("OrderKeyForNewTask(atFront=false) = (%v, %v), want > 100", back, err)
	}
	front, err := store.OrderKeyForNewTask(ctx, true)
	if err != nil || front >= 100 {
		t.Fatalf("OrderKeyForNewTask(atFront=true) = (%v, %v), want < 100", front, err)
	}
}

// TestReorderPlacesBetweenNeighbours drags one task at a time and checks
// the resulting backlog order, including the two unbounded cases
// (dropped at the very head or the very tail of the list) the issue's own
// "just before the following job if moved to the head of the list" calls
// out by name.
func TestReorderPlacesBetweenNeighbours(t *testing.T) {
	store, _, ctx := openStore(t)
	for id, key := range map[string]float64{"a": 10, "b": 20, "c": 30} {
		tk := task(id, true)
		tk.OrderKey = key
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	after, before := ptr("a"), ptr("b")

	// c dropped between a and b.
	if err := store.Reorder(ctx, []string{"c"}, after, before); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"a", "c", "b"}) {
		t.Fatalf("after dropping c between a and b: %v, want [a c b]", got)
	}

	// b dropped at the very head -- no preceding job, so it goes just
	// before the (new) first task, a.
	if err := store.Reorder(ctx, []string{"b"}, nil, ptr("a")); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Fatalf("after dropping b at the head: %v, want [b a c]", got)
	}

	// a dropped at the very tail -- no following job.
	if err := store.Reorder(ctx, []string{"a"}, ptr("c"), nil); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Fatalf("after dropping a at the tail: %v, want [b c a]", got)
	}
}

// TestReorderMultiSelectKeepsRelativeOrder drags two tasks at once,
// passed in the opposite order from how they currently sit in the
// backlog, and checks Reorder still lands them as a block in their own
// existing relative order (a before c) rather than in whatever order the
// ids argument happened to list them.
func TestReorderMultiSelectKeepsRelativeOrder(t *testing.T) {
	store, _, ctx := openStore(t)
	for id, key := range map[string]float64{"a": 10, "b": 20, "c": 30, "d": 40} {
		tk := task(id, true)
		tk.OrderKey = key
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// ids names c before a -- the reverse of their current order -- to
	// prove Reorder sorts by OrderKey rather than trusting this order.
	if err := store.Reorder(ctx, []string{"c", "a"}, ptr("d"), nil); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"b", "d", "a", "c"}) {
		t.Fatalf("after multi-select drag: %v, want [b d a c] -- a and c keep their own relative order", got)
	}
}

// TestReorderRejectsAnUnknownID is TestReorderRejectsAStaleNeighbour's
// counterpart for ids itself: a task named in the drag rather than as a
// neighbour it was dropped against.
func TestReorderRejectsAnUnknownID(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.Reorder(ctx, []string{"gone"}, ptr("a1b2"), nil); err == nil {
		t.Fatal("Reorder with an unknown id succeeded, want an error")
	}
}

// TestReorderRejectsAStaleNeighbour is what a caller sees when the
// neighbour it computed the drop against (afterID or beforeID) no longer
// names a task -- closed, or reordered out from under it, between when it
// built the request and when this ran.
func TestReorderRejectsAStaleNeighbour(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.Reorder(ctx, []string{"a1b2"}, ptr("gone"), nil); err == nil {
		t.Fatal("Reorder with an unknown afterID succeeded, want an error")
	}
}

// TestReorderRebalancesWhenNeighboursAreCrowded forces two neighbours'
// OrderKey closer together than minOrderKeyGap allows splitting again,
// directly through the database rather than through repeated drags, and
// checks Reorder still lands a task strictly between them -- proof the
// rebalance backstop actually ran rather than Reorder simply producing an
// indistinguishable float and silently misplacing the drop.
func TestReorderRebalancesWhenNeighboursAreCrowded(t *testing.T) {
	store, db, ctx := openStore(t)
	for id, key := range map[string]float64{"a": 10, "b": 10 + 1e-9, "c": 30} {
		tk := task(id, true)
		tk.OrderKey = key
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// Belt and braces: pin the two neighbours' keys directly, in case a
	// future change to PutTask ever stopped taking OrderKey as given.
	if _, err := db.ExecContext(ctx, "UPDATE `task` SET `order_key` = 10 WHERE `id` = 'a'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE `task` SET `order_key` = 10.0000001 WHERE `id` = 'b'"); err != nil {
		t.Fatal(err)
	}

	if err := store.Reorder(ctx, []string{"c"}, ptr("a"), ptr("b")); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"a", "c", "b"}) {
		t.Fatalf("after dropping c into a crowded gap: %v, want [a c b]", got)
	}
}

func ptr[T any](v T) *T { return &v }

// --- the merge queue at the front of the backlog -------------------------

// putOrdered files tasks at the OrderKey each name maps to, so a test can
// state the backlog it starts from as the order itself rather than as a
// sequence of writes.
func putOrdered(t *testing.T, store *model.Store, ctx context.Context, keys map[string]float64) {
	t.Helper()
	for id, key := range keys {
		tk := task(id, true)
		tk.OrderKey = key
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
}

func orderKeys(t *testing.T, store *model.Store, ctx context.Context) map[string]float64 {
	t.Helper()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	keys := map[string]float64{}
	for _, tk := range tasks {
		keys[tk.ID] = tk.OrderKey
	}
	return keys
}

// TestMoveToFrontOfBacklogCarriesTheQueuePastOrdinaryWork is the merge
// queue making its order visible: the tasks whose pull requests are
// waiting to land go to the front of the list, keeping the relative order
// they already had, and everything else keeps its own.
func TestMoveToFrontOfBacklogCarriesTheQueuePastOrdinaryWork(t *testing.T) {
	store, _, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"ordinary": 10, "queued-2": 20, "queued-1": 5})

	// Named in neither backlog nor queue order, to pin that the block
	// lands in the order the backlog already gave it rather than in the
	// order the caller happened to list.
	if err := store.MoveToFrontOfBacklog(ctx, []string{"queued-2", "queued-1"}); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"queued-1", "queued-2", "ordinary"}) {
		t.Fatalf("backlog = %v, want the two queued tasks in front of the ordinary one", got)
	}
}

// TestMoveToFrontOfBacklogStaysBehindAFixTaskAtTheHead is the other half
// of the ordering: one of the separate merge tasks the queue used to file
// sits at the very head, so the queue it repairs goes in front of the
// ordinary backlog but behind that.
func TestMoveToFrontOfBacklogStaysBehindAFixTaskAtTheHead(t *testing.T) {
	store, _, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"ordinary": 10, "queued": 20})
	fix := task("fix", true)
	fix.Origin.Reason = model.ReasonFix
	fix.OrderKey = -10
	if err := store.PutTask(ctx, fix); err != nil {
		t.Fatal(err)
	}

	if err := store.MoveToFrontOfBacklog(ctx, []string{"queued"}); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"fix", "queued", "ordinary"}) {
		t.Fatalf("backlog = %v, want the fix task still at the very head", got)
	}
}

// TestMoveToFrontOfBacklogIgnoresAFixTaskInsideTheBacklog is the same
// question asked of a fix task that is *not* at the head -- dragged down
// by a human, or filed before fix tasks were placed at all and left at
// OrderKey's zero value. It bounds nothing: the queue goes to the front
// of the whole list, which is the only place it can be seen.
func TestMoveToFrontOfBacklogIgnoresAFixTaskInsideTheBacklog(t *testing.T) {
	store, _, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"ordinary": 10, "queued": 30})
	fix := task("fix", true)
	fix.Origin.Reason = model.ReasonFix
	fix.OrderKey = 20
	if err := store.PutTask(ctx, fix); err != nil {
		t.Fatal(err)
	}

	if err := store.MoveToFrontOfBacklog(ctx, []string{"queued"}); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"queued", "ordinary", "fix"}) {
		t.Fatalf("backlog = %v, want the queue in front of a fix task sitting inside the backlog", got)
	}
}

// TestMoveToFrontOfBacklogWritesNothingWhenTheOrderAlreadyHolds is what
// makes it safe for orchestrator.SyncPullRequests to call every cycle: an
// order already correct is left exactly as it is, rather than rewritten to
// the same order with fresh keys that crowd every later split.
func TestMoveToFrontOfBacklogWritesNothingWhenTheOrderAlreadyHolds(t *testing.T) {
	store, _, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"queued-1": 5, "queued-2": 6, "ordinary": 10})

	before := orderKeys(t, store, ctx)
	if err := store.MoveToFrontOfBacklog(ctx, []string{"queued-1", "queued-2"}); err != nil {
		t.Fatal(err)
	}
	if after := orderKeys(t, store, ctx); !reflect.DeepEqual(after, before) {
		t.Fatalf("order keys changed from %v to %v with nothing out of place", before, after)
	}
}

// TestMoveToFrontOfBacklogRebalancesWhenTheFrontIsCrowded is Reorder's own
// backstop reached from here: the gap between the fix task at the head and
// the first ordinary task is too fine to split two keys out of, so the
// whole backlog is renumbered first and the move still lands in order.
func TestMoveToFrontOfBacklogRebalancesWhenTheFrontIsCrowded(t *testing.T) {
	store, db, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"ordinary": 10, "queued-1": 20, "queued-2": 30})
	fix := task("fix", true)
	fix.Origin.Reason = model.ReasonFix
	fix.OrderKey = 9.99999999
	if err := store.PutTask(ctx, fix); err != nil {
		t.Fatal(err)
	}
	// Belt and braces, the same as TestReorderRebalancesWhenNeighboursAreCrowded.
	if _, err := db.ExecContext(ctx, "UPDATE `task` SET `order_key` = 9.99999999 WHERE `id` = 'fix'"); err != nil {
		t.Fatal(err)
	}

	if err := store.MoveToFrontOfBacklog(ctx, []string{"queued-1", "queued-2"}); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"fix", "queued-1", "queued-2", "ordinary"}) {
		t.Fatalf("backlog = %v, want the queue landed between a crowded head and the backlog", got)
	}
}

func TestMoveToFrontOfBacklogRejectsAnUnknownID(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTask(ctx, task("a1b2", true)); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveToFrontOfBacklog(ctx, []string{"gone"}); err == nil {
		t.Fatal("MoveToFrontOfBacklog with an unknown id succeeded, want an error")
	}
}

// TestReadyDispatchesAFixTaskInBacklogOrderLikeAnythingElse pins the
// carve-out Store.Ready used to make for Origin.Reason == ReasonFix being
// gone: a fix task is dispatched first because it was filed at the head
// of the backlog, and one a human has since dragged behind other work
// waits its turn there like anything else. Dispatch order is the order on screen, both ways round.
func TestReadyDispatchesAFixTaskInBacklogOrderLikeAnythingElse(t *testing.T) {
	store, _, ctx := openStore(t)
	putOrdered(t, store, ctx, map[string]float64{"ordinary": 10})
	fix := task("fix", true)
	fix.Origin.Reason = model.ReasonFix
	fix.OrderKey = 20
	if err := store.PutTask(ctx, fix); err != nil {
		t.Fatal(err)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ordinary", "fix"}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready = %v, want %v -- a fix task behind other work in the backlog waits behind it", ready, want)
	}
}

// --- backlog order settings (bwsalmon/agents#476) ------------------------

func TestInitMigratesAnExistingDatabaseMissingOrderKey(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task`"+` (
  `+"`id`"+`                    TEXT    NOT NULL,
  `+"`intent`"+`                TEXT    NOT NULL,
  `+"`title`"+`                 TEXT    NOT NULL,
  `+"`body`"+`                  TEXT    NOT NULL,
  `+"`origin_actor_kind`"+`     TEXT    NOT NULL,
  `+"`origin_actor_id`"+`       TEXT    NOT NULL,
  `+"`origin_behalf_kind`"+`    TEXT    NULL,
  `+"`origin_behalf_id`"+`      TEXT    NULL,
  `+"`origin_reason`"+`         TEXT    NOT NULL,
  `+"`approval_actor_kind`"+`   TEXT    NULL,
  `+"`approval_actor_id`"+`     TEXT    NULL,
  `+"`approval_behalf_kind`"+`  TEXT    NULL,
  `+"`approval_behalf_id`"+`    TEXT    NULL,
  `+"`approved_at`"+`           DATETIME NULL,
  `+"`target_owner`"+`          TEXT    NULL,
  `+"`target_name`"+`           TEXT    NULL,
  `+"`binding`"+`               TEXT    NOT NULL,
  `+"`base`"+`                  TEXT    NULL,
  `+"`folder`"+`                TEXT    NULL,
  `+"`auto_merge`"+`            INTEGER  NOT NULL,
  `+"`created_at`"+`            DATETIME NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#476 task table: %v", err)
	}
	for _, id := range []string{"9", "10", "2"} {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO `task` (`id`,`intent`,`title`,`body`,`origin_actor_kind`,`origin_actor_id`,"+
				"`origin_reason`,`binding`,`auto_merge`,`created_at`) "+
				"VALUES (?,'implement','t','','human','bwsalmon','direct','directive',0,?)", id, now); err != nil {
			t.Fatalf("seeding a pre-#476 task row: %v", err)
		}
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task.order_key: %v", err)
	}

	// Backfilled in the same lexical id order Ready already dispatched in
	// before OrderKey existed -- "10" < "2" < "9" as strings -- so an
	// upgraded deployment's dispatch order is unchanged by this migration.
	if got := listedIDs(t, store, ctx); !reflect.DeepEqual(got, []string{"10", "2", "9"}) {
		t.Fatalf("ListTasks after migrating = %v, want [10 2 9] (lexical id order preserved)", got)
	}
}

func TestInitMigratesAnExistingDatabaseMissingNewestFirst(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#476 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','')"); err != nil {
		t.Fatalf("seeding a pre-#476 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.newest_first: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.NewestFirst {
		// false is what keeps an upgraded deployment's backlog order
		// unchanged -- the whole point of this migration's default.
		t.Fatalf("NewestFirst after migrating = true, want false")
	}
}

// TestInitMigratesAnExistingDatabaseMissingSandboxShape is
// TestInitMigratesAnExistingDatabaseMissingNewestFirst's own pattern,
// applied to grain_config.sandbox_cpus/sandbox_memory_mb (bwsalmon/
// agents#534): a database from before this setting existed gets both
// columns added by ensureConfigSandboxShapeColumns, defaulted to 0 --
// Config.SandboxCPUs/SandboxMemoryMB's own "use bwsalmon/kontur's own
// default" zero value, so an upgraded deployment's sandbox shape is
// unchanged by this migration.
func TestInitMigratesAnExistingDatabaseMissingSandboxShape(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#534 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-#534 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.sandbox_cpus/sandbox_memory_mb: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.SandboxCPUs != 0 || got.SandboxMemoryMB != 0 || got.SandboxDiskGB != 0 {
		t.Fatalf("SandboxCPUs/SandboxMemoryMB/SandboxDiskGB after migrating = %d/%d/%d, want 0/0/0",
			got.SandboxCPUs, got.SandboxMemoryMB, got.SandboxDiskGB)
	}
}

// TestInitMigratesAnExistingDatabaseMissingShowClosedByDefault is the
// same pattern, applied to grain_config.show_closed_by_default
// (bwsalmon/agents#537).
func TestInitMigratesAnExistingDatabaseMissingShowClosedByDefault(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#537 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-#537 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.show_closed_by_default: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.ShowClosedByDefault {
		// false is model.Config's own zero value -- the new "hide closed
		// tasks by default" behaviour applies to an upgraded deployment
		// exactly as it does to a fresh one, the same default
		// ensureConfigShowClosedByDefaultColumn's own doc comment explains.
		t.Fatalf("ShowClosedByDefault after migrating = true, want false")
	}
}

// TestInitMigratesAnExistingDatabaseMissingDefaultCapabilities is the
// same pattern, applied to grain_config.default_capabilities
// (grain/task-14): a database from before a deployment could say
// which capabilities every new task starts with gets the column added by
// ensureConfigDefaultCapabilitiesColumn, defaulted to '' -- which
// splitCSV reads back as no defaults, so an upgraded deployment keeps
// filing tasks with exactly what whoever files them asks for until an
// operator chooses otherwise.
func TestInitMigratesAnExistingDatabaseMissingDefaultCapabilities(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-14 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-task-14 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.default_capabilities: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if len(got.DefaultCapabilities) != 0 {
		t.Fatalf("DefaultCapabilities after migrating = %v, want none", got.DefaultCapabilities)
	}

	// And writable afterwards: the migration adds a column PutConfig
	// binds, so a set chosen after the upgrade is durable rather than
	// failing every save.
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(got.DefaultCapabilities, want.DefaultCapabilities) {
		t.Fatalf("DefaultCapabilities = %v, want %v", got.DefaultCapabilities, want.DefaultCapabilities)
	}
}

// TestInitMigratesAnExistingDatabaseMissingEnvironmentName is the same
// pattern, applied to grain_config.environment_name (grain/task-69): a
// database from before a deployment could be named gets the column added
// by ensureConfigEnvironmentNameColumn, defaulted to '' -- an unnamed
// deployment, whose UI looks exactly as it did before the upgrade until
// an operator names it.
func TestInitMigratesAnExistingDatabaseMissingEnvironmentName(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-69 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-task-69 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.environment_name: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.EnvironmentName != "" {
		t.Fatalf("EnvironmentName after migrating = %q, want empty", got.EnvironmentName)
	}

	// And writable afterwards, the same as every other added column:
	// PutConfig binds this one, so naming the deployment after the
	// upgrade is durable rather than failing every save.
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.EnvironmentName != want.EnvironmentName {
		t.Fatalf("EnvironmentName = %q, want %q", got.EnvironmentName, want.EnvironmentName)
	}
}

// --- the prompt extension's three columns (grain/task-114) ---------------
//
// One migration each, on three tables every existing deployment already
// has rows in, and CREATE TABLE IF NOT EXISTS never alters one of those
// -- so each is simulated here directly: the table as it was, a row in
// it, then Init, then the column read and written for real. Getting any
// of the three wrong breaks an upgraded deployment rather than a fresh
// one, which is the failure nothing else in this suite would see.

// TestInitMigratesAnExistingDatabaseMissingConfigPromptExtension is the
// same pattern as the environment-name migration above, applied to
// grain_config.prompt_extension: the column arrives defaulted to '',
// which adds nothing to any prompt -- exactly what an upgraded
// deployment was doing until somebody writes standing instructions.
func TestInitMigratesAnExistingDatabaseMissingConfigPromptExtension(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-114 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-task-114 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.prompt_extension: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.PromptExtension != "" {
		t.Fatalf("PromptExtension after migrating = %q, want empty", got.PromptExtension)
	}

	// And writable afterwards: PutConfig binds this column, so an
	// operator who writes standing instructions after the upgrade gets
	// them stored rather than every save failing.
	want := testConfig()
	if err := store.PutConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.PromptExtension != want.PromptExtension {
		t.Fatalf("PromptExtension = %q, want %q", got.PromptExtension, want.PromptExtension)
	}
}

// repo_config predates its own prompt_extension column (schema.go's DDL
// comment on that table), so a deployment that had already given one repo
// default capabilities has the table without the column. The row it
// already holds has to survive, and the new field has to be writable
// onto it.
func TestInitMigratesAnExistingDatabaseMissingRepoConfigPromptExtension(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`repo_config`"+` (
  `+"`owner`"+`                TEXT NOT NULL,
  `+"`name`"+`                 TEXT NOT NULL,
  `+"`default_capabilities`"+` TEXT NOT NULL,
  PRIMARY KEY (`+"`owner`"+`, `+"`name`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-114 repo_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `repo_config` (`owner`,`name`,`default_capabilities`) VALUES ('acme','widgets','gcp-key')"); err != nil {
		t.Fatalf("seeding a pre-task-114 repo_config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing repo_config.prompt_extension: %v", err)
	}

	widgets := model.RepoRef{Owner: "acme", Name: "widgets"}
	got, err := store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get after migrating: (%+v, %v)", got, err)
	}
	want := model.RepoConfig{Repo: widgets, DefaultCapabilities: []string{"gcp-key"}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v -- the capabilities it already had, and no standing instructions", *got, want)
	}

	want.PromptExtension = "Widgets keeps its migrations in db/."
	if err := store.PutRepoConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// repo_config.setup_command is newer again (grain/task-154), so the
// same migration has to hold for a deployment that upgrades across it:
// the prompt extension and the capabilities a repo already had survive,
// and a setup command can be written onto the row that holds them.
func TestInitMigratesAnExistingDatabaseMissingRepoConfigSetupCommand(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`repo_config`"+` (
  `+"`owner`"+`                TEXT NOT NULL,
  `+"`name`"+`                 TEXT NOT NULL,
  `+"`default_capabilities`"+` TEXT NOT NULL,
  `+"`prompt_extension`"+`     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (`+"`owner`"+`, `+"`name`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-154 repo_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `repo_config` (`owner`,`name`,`default_capabilities`,`prompt_extension`) "+
			"VALUES ('acme','widgets','gcp-key','Read db/README.md.')"); err != nil {
		t.Fatalf("seeding a pre-task-154 repo_config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing repo_config.setup_command: %v", err)
	}

	widgets := model.RepoRef{Owner: "acme", Name: "widgets"}
	got, err := store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get after migrating: (%+v, %v)", got, err)
	}
	want := model.RepoConfig{
		Repo:                widgets,
		DefaultCapabilities: []string{"gcp-key"},
		PromptExtension:     "Read db/README.md.",
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v -- what it already had, and no setup command", *got, want)
	}

	want.SetupCommand = "make deps"
	if err := store.PutRepoConfig(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	got, err = store.GetRepoConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// task.prompt_extension is the third, and the one whose absence would be
// loudest: scanTask selects every column by name, so a database missing
// this one fails *every* task read rather than only the feature's own.
func TestInitMigratesAnExistingDatabaseMissingTaskPromptExtension(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task`"+` (
  `+"`id`"+`                    TEXT    NOT NULL,
  `+"`intent`"+`                TEXT    NOT NULL,
  `+"`title`"+`                 TEXT    NOT NULL,
  `+"`body`"+`                  TEXT    NOT NULL,
  `+"`origin_actor_kind`"+`     TEXT    NOT NULL,
  `+"`origin_actor_id`"+`       TEXT    NOT NULL,
  `+"`origin_behalf_kind`"+`    TEXT    NULL,
  `+"`origin_behalf_id`"+`      TEXT    NULL,
  `+"`origin_reason`"+`         TEXT    NOT NULL,
  `+"`approval_actor_kind`"+`   TEXT    NULL,
  `+"`approval_actor_id`"+`     TEXT    NULL,
  `+"`approval_behalf_kind`"+`  TEXT    NULL,
  `+"`approval_behalf_id`"+`    TEXT    NULL,
  `+"`approved_at`"+`           DATETIME NULL,
  `+"`target_owner`"+`          TEXT    NULL,
  `+"`target_name`"+`           TEXT    NULL,
  `+"`binding`"+`               TEXT    NOT NULL,
  `+"`base`"+`                  TEXT    NULL,
  `+"`folder`"+`                TEXT    NULL,
  `+"`auto_merge`"+`            INTEGER  NOT NULL,
  `+"`created_at`"+`            DATETIME NULL,
  `+"`order_key`"+`             REAL     NOT NULL DEFAULT 0,
  `+"`sandbox_cpus`"+`          INTEGER  NOT NULL DEFAULT 0,
  `+"`sandbox_memory_mb`"+`     INTEGER  NOT NULL DEFAULT 0,
  `+"`sandbox_disk_gb`"+`       INTEGER  NOT NULL DEFAULT 0,
  `+"`interactive`"+`           INTEGER  NOT NULL DEFAULT 0,
  `+"`configuration`"+`         INTEGER  NOT NULL DEFAULT 0,
  `+"`agent_framework`"+`       TEXT     NOT NULL DEFAULT '',
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-task-114 task table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task` (`id`,`intent`,`title`,`body`,`origin_actor_kind`,`origin_actor_id`,"+
			"`origin_reason`,`binding`,`auto_merge`,`created_at`) "+
			"VALUES ('a1b2','implement','Rename the endpoint','','human','bwsalmon','direct','directive',0,?)",
		now); err != nil {
		t.Fatalf("seeding a pre-task-114 task row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task.prompt_extension: %v", err)
	}

	got, err := store.GetTask(ctx, "a1b2")
	if err != nil || got == nil {
		t.Fatalf("get after migrating: (%+v, %v)", got, err)
	}
	// '' is Task.PromptExtension's own "no override": a task filed before
	// this existed is dispatched with exactly what the deployment and its
	// repo say, which is what it would have been given anyway.
	if got.PromptExtension != "" {
		t.Fatalf("PromptExtension after migrating = %q, want empty", got.PromptExtension)
	}

	got.PromptExtension = "Ignore the house rules: this task is regenerating them."
	if err := store.PutTask(ctx, *got); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
	reread, err := store.GetTask(ctx, "a1b2")
	if err != nil || reread == nil {
		t.Fatalf("get: (%+v, %v)", reread, err)
	}
	if reread.PromptExtension != got.PromptExtension {
		t.Fatalf("PromptExtension = %q, want %q", reread.PromptExtension, got.PromptExtension)
	}
}

// TestInitMigratesAnExistingDatabaseMissingTaskDefaults is the same
// pattern, applied to the pair grain_config.approved_by_default/
// auto_merge_by_default (bwsalmon/agents#612) -- except that what an
// upgraded row lands on is true rather than the column's Go zero value:
// both settings default on (model.DefaultConfig), so a deployment
// upgrading across this migration gets the same "Queue immediately" and
// "Auto-merge once checks pass" starting state a fresh one does.
func TestInitMigratesAnExistingDatabaseMissingTaskDefaults(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#612 grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-#612 config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing the task-default columns: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !got.ApprovedByDefault || !got.AutoMergeByDefault {
		t.Fatalf("ApprovedByDefault/AutoMergeByDefault after migrating = %v/%v, want true/true",
			got.ApprovedByDefault, got.AutoMergeByDefault)
	}
}

// TestInitTurnsTaskDefaultsOnOnceForARowStoringTheOldDefault is
// Store.ensureConfigTaskDefaultsOn: a row written by a build whose
// default for these two was off stores 0 in columns that already exist,
// so no ensure*Column migration would ever touch it and the new default
// would reach fresh databases only. The backfill turns both on, exactly
// once -- an operator who turns either off afterwards has made a
// deliberate choice, and a restart must not overwrite it.
func TestInitTurnsTaskDefaultsOnOnceForARowStoringTheOldDefault(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// grain_config as it stood with both settings present and defaulting
	// off: the columns are there, so only the backfill can move them.
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  `+"`approved_by_default`"+`          INTEGER NOT NULL DEFAULT 0,
  `+"`auto_merge_by_default`"+`        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the old-default grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`,"+
			"`approved_by_default`,`auto_merge_by_default`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0,0,0)"); err != nil {
		t.Fatalf("seeding a config row storing the old default: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against a row storing the old task defaults: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !got.ApprovedByDefault || !got.AutoMergeByDefault {
		t.Fatalf("ApprovedByDefault/AutoMergeByDefault after backfilling = %v/%v, want true/true",
			got.ApprovedByDefault, got.AutoMergeByDefault)
	}

	// An operator turns one back off through Settings, and the daemon
	// restarts: PutConfig is a REPLACE that doesn't bind the ledger
	// column, so this is also what pins that re-defaulting it cannot
	// re-arm the backfill.
	off := *got
	off.ApprovedByDefault = false
	if err := store.PutConfig(ctx, off); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := model.New(db).Init(ctx); err != nil {
		t.Fatalf("re-Init after an operator turned a task default off: %v", err)
	}
	got, err = store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.ApprovedByDefault {
		t.Errorf("ApprovedByDefault = true after a restart, want the false an operator chose")
	}
	if !got.AutoMergeByDefault {
		t.Errorf("AutoMergeByDefault = false after a restart, want the true it was left at")
	}
}

// TestInitMigratesAnExistingDatabaseMissingClaudeModel is the same
// migration, applied to grain_config.claude_model
// (model.Config.ClaudeModel's own doc comment has the reasoning).
func TestInitMigratesAnExistingDatabaseMissingClaudeModel(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`grain_config`"+` (
  `+"`id`"+`                         INTEGER NOT NULL,
  `+"`poll_interval_ms`"+`           INTEGER NOT NULL,
  `+"`max_concurrent`"+`             INTEGER NOT NULL,
  `+"`gemini_model`"+`                TEXT    NOT NULL,
  `+"`max_agent_turns`"+`             INTEGER NOT NULL,
  `+"`github_host`"+`                 TEXT    NOT NULL,
  `+"`github_insecure_http`"+`        INTEGER NOT NULL,
  `+"`gcp_project`"+`                 TEXT    NOT NULL,
  `+"`gcp_service_account_email`"+`   TEXT    NOT NULL,
  `+"`target_repos`"+`                TEXT    NOT NULL,
  `+"`newest_first`"+`                INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-claude-model grain_config table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `grain_config` (`id`,`poll_interval_ms`,`max_concurrent`,`gemini_model`,`max_agent_turns`,"+
			"`github_host`,`github_insecure_http`,`gcp_project`,`gcp_service_account_email`,`target_repos`,`newest_first`) "+
			"VALUES (1,30000,2,'gemini-2.5-pro',40,'github.com',0,'grain-prod','agent@grain-prod.iam.gserviceaccount.com','',0)"); err != nil {
		t.Fatalf("seeding a pre-claude-model config row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing grain_config.claude_model: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.ClaudeModel != "" {
		t.Fatalf("ClaudeModel after migrating = %q, want \"\"", got.ClaudeModel)
	}
}

// TestInitMigratesAnExistingDatabaseMissingTaskSandboxShape is the same
// migration, applied to task.sandbox_cpus/sandbox_memory_mb.
func TestInitMigratesAnExistingDatabaseMissingTaskSandboxShape(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task`"+` (
  `+"`id`"+`                    TEXT    NOT NULL,
  `+"`intent`"+`                TEXT    NOT NULL,
  `+"`title`"+`                 TEXT    NOT NULL,
  `+"`body`"+`                  TEXT    NOT NULL,
  `+"`origin_actor_kind`"+`     TEXT    NOT NULL,
  `+"`origin_actor_id`"+`       TEXT    NOT NULL,
  `+"`origin_behalf_kind`"+`    TEXT    NULL,
  `+"`origin_behalf_id`"+`      TEXT    NULL,
  `+"`origin_reason`"+`         TEXT    NOT NULL,
  `+"`approval_actor_kind`"+`   TEXT    NULL,
  `+"`approval_actor_id`"+`     TEXT    NULL,
  `+"`approval_behalf_kind`"+`  TEXT    NULL,
  `+"`approval_behalf_id`"+`    TEXT    NULL,
  `+"`approved_at`"+`           DATETIME NULL,
  `+"`target_owner`"+`          TEXT    NULL,
  `+"`target_name`"+`           TEXT    NULL,
  `+"`binding`"+`               TEXT    NOT NULL,
  `+"`base`"+`                  TEXT    NULL,
  `+"`folder`"+`                TEXT    NULL,
  `+"`auto_merge`"+`            INTEGER  NOT NULL,
  `+"`created_at`"+`            DATETIME NULL,
  `+"`order_key`"+`             REAL     NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#534 task table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task` (`id`,`intent`,`title`,`body`,`origin_actor_kind`,`origin_actor_id`,`origin_reason`,"+
			"`binding`,`auto_merge`) VALUES ('t1','implement','a title','a body','automation','grain','direct','default',0)"); err != nil {
		t.Fatalf("seeding a pre-#534 task row: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_grant` (`task_id` TEXT, `capability` TEXT, `via` TEXT, `folder` TEXT)"); err != nil {
		t.Fatalf("creating task_grant: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_read` (`task_id` TEXT, `owner` TEXT, `name` TEXT)"); err != nil {
		t.Fatalf("creating task_read: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_link` (`task_id` TEXT, `kind` TEXT, `target` TEXT, `blocks` INTEGER)"); err != nil {
		t.Fatalf("creating task_link: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_tag` (`task_id` TEXT, `tag` TEXT)"); err != nil {
		t.Fatalf("creating task_tag: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task.sandbox_cpus/sandbox_memory_mb: %v", err)
	}

	got, err := store.GetTask(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.SandboxCPUs != 0 || got.SandboxMemoryMB != 0 || got.SandboxDiskGB != 0 {
		t.Fatalf("SandboxCPUs/SandboxMemoryMB/SandboxDiskGB after migrating = %d/%d/%d, want 0/0/0",
			got.SandboxCPUs, got.SandboxMemoryMB, got.SandboxDiskGB)
	}
}

// TestInitMigratesAnExistingDatabaseMissingConfiguration is the same
// migration, applied to task.configuration (bwsalmon/agents#621) -- a
// database that already has every column through task.interactive
// (bwsalmon/agents#539) but predates the configuration agent.
func TestInitMigratesAnExistingDatabaseMissingConfiguration(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task`"+` (
  `+"`id`"+`                    TEXT    NOT NULL,
  `+"`intent`"+`                TEXT    NOT NULL,
  `+"`title`"+`                 TEXT    NOT NULL,
  `+"`body`"+`                  TEXT    NOT NULL,
  `+"`origin_actor_kind`"+`     TEXT    NOT NULL,
  `+"`origin_actor_id`"+`       TEXT    NOT NULL,
  `+"`origin_behalf_kind`"+`    TEXT    NULL,
  `+"`origin_behalf_id`"+`      TEXT    NULL,
  `+"`origin_reason`"+`         TEXT    NOT NULL,
  `+"`approval_actor_kind`"+`   TEXT    NULL,
  `+"`approval_actor_id`"+`     TEXT    NULL,
  `+"`approval_behalf_kind`"+`  TEXT    NULL,
  `+"`approval_behalf_id`"+`    TEXT    NULL,
  `+"`approved_at`"+`           DATETIME NULL,
  `+"`target_owner`"+`          TEXT    NULL,
  `+"`target_name`"+`           TEXT    NULL,
  `+"`binding`"+`               TEXT    NOT NULL,
  `+"`base`"+`                  TEXT    NULL,
  `+"`folder`"+`                TEXT    NULL,
  `+"`auto_merge`"+`            INTEGER  NOT NULL,
  `+"`created_at`"+`            DATETIME NULL,
  `+"`order_key`"+`             REAL     NOT NULL DEFAULT 0,
  `+"`sandbox_cpus`"+`          INTEGER  NOT NULL DEFAULT 0,
  `+"`sandbox_memory_mb`"+`     INTEGER  NOT NULL DEFAULT 0,
  `+"`interactive`"+`           INTEGER  NOT NULL DEFAULT 0,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#621 task table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task` (`id`,`intent`,`title`,`body`,`origin_actor_kind`,`origin_actor_id`,`origin_reason`,"+
			"`binding`,`auto_merge`) VALUES ('t1','implement','a title','a body','automation','grain','direct','default',0)"); err != nil {
		t.Fatalf("seeding a pre-#621 task row: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_grant` (`task_id` TEXT, `capability` TEXT, `via` TEXT, `folder` TEXT)"); err != nil {
		t.Fatalf("creating task_grant: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_read` (`task_id` TEXT, `owner` TEXT, `name` TEXT)"); err != nil {
		t.Fatalf("creating task_read: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_link` (`task_id` TEXT, `kind` TEXT, `target` TEXT, `blocks` INTEGER)"); err != nil {
		t.Fatalf("creating task_link: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE `task_tag` (`task_id` TEXT, `tag` TEXT)"); err != nil {
		t.Fatalf("creating task_tag: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database missing task.configuration: %v", err)
	}

	got, err := store.GetTask(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Configuration {
		t.Fatalf("Configuration after migrating = true, want false")
	}
}

// A schema change that *removes* something -- SchemaVersion 16 dropping
// task_run.slot and its index -- cannot be applied to an existing
// database by Init: CREATE TABLE IF NOT EXISTS never alters a table that
// is already there, and the ensure*Column migrations only ever add. An
// older store therefore keeps a slot column still declared NOT NULL with
// no default, and every StartRun fails to satisfy it -- on every dispatch,
// forever, from a daemon that started up without complaint. Init has to
// refuse it instead, so the failure names itself once at startup rather
// than a tick at a time.
func TestInitRefusesAStoreOlderThanThisBuild(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(sqlite.DefaultConfig(dir))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := model.New(db).Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE `grain_schema` SET `version` = ? WHERE `id` = 1", model.SchemaVersion-1); err != nil {
		t.Fatal(err)
	}

	err = model.New(db).Init(ctx)
	if !errors.Is(err, model.ErrSchemaTooOld) {
		t.Fatalf("Init against an older store = %v, want ErrSchemaTooOld", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(model.SchemaVersion-1)) ||
		!strings.Contains(err.Error(), strconv.Itoa(model.SchemaVersion)) {
		t.Errorf("the error should name both versions, got: %v", err)
	}
}

// The other direction, which has always been refused: a store written by
// a build that knows a later schema.
func TestInitRefusesAStoreNewerThanThisBuild(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(sqlite.DefaultConfig(dir))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := model.New(db).Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE `grain_schema` SET `version` = ? WHERE `id` = 1", model.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}

	if err := model.New(db).Init(ctx); !errors.Is(err, model.ErrSchemaTooNew) {
		t.Fatalf("Init against a newer store = %v, want ErrSchemaTooNew", err)
	}
}

// A store this build itself just created re-opens without complaint --
// the ordinary daemon restart, which neither guard may catch.
func TestInitAcceptsAStoreAtThisBuildsOwnVersion(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(sqlite.DefaultConfig(dir))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := model.New(db).Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	if err := model.New(db).Init(ctx); err != nil {
		t.Fatalf("re-opening a store this build created: %v", err)
	}
}

// TestGetConfigNormalizesTheLegacyAgentFrameworkName is the upgrade path
// for a deployment whose grain_config row still says "gemini" -- the name
// the default framework had while it was a home-grown in-process Gemini
// API loop, before agent/antigravity replaced it. Nothing rewrites those
// rows (Store.ensureConfigAgentFrameworkColumn's own doc comment says
// why), so the whole guarantee that such a deployment keeps running rests
// on GetConfig folding the old spelling into the new one on the way out.
//
// The row is written straight through PutConfig rather than by hand:
// storing a value the vocabulary no longer contains is exactly what an
// older grain binary did, and the read is what has to cope.
func TestGetConfigNormalizesTheLegacyAgentFrameworkName(t *testing.T) {
	store, _, ctx := openStore(t)
	legacy := testConfig()
	legacy.AgentFramework = model.LegacyAgentFrameworkGemini
	if err := store.PutConfig(ctx, legacy); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.AgentFramework != model.AgentFrameworkAntigravity {
		t.Errorf("AgentFramework = %q for a row storing %q, want %q",
			got.AgentFramework, model.LegacyAgentFrameworkGemini, model.AgentFrameworkAntigravity)
	}
}

// TestGetConfigDefaultsAnEmptyAgentFrameworkToAntigravity is the same
// normalization for the other value that names no framework: a row
// written before the column existed at all, which reads back as "".
func TestGetConfigDefaultsAnEmptyAgentFrameworkToAntigravity(t *testing.T) {
	store, _, ctx := openStore(t)
	unset := testConfig()
	unset.AgentFramework = ""
	if err := store.PutConfig(ctx, unset); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.AgentFramework != model.AgentFrameworkAntigravity {
		t.Errorf("AgentFramework = %q for a row storing \"\", want %q",
			got.AgentFramework, model.AgentFrameworkAntigravity)
	}
}
