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
	}, 0); err != nil {
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
		ID: "r1", TaskID: "a1b2", Sandbox: "s1", Attempt: 1, StartedAt: now,
		Leases: []model.Lease{{
			Capability: "gemini-key", Resource: "projects/p/keys/k",
			MintedBy: model.CredentialRef{Name: "gcp-host-service-account"},
			IssuedAt: now, ExpiresAt: &expires,
		}},
	}, 0); err != nil {
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
		}, 0); err != nil {
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
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r-second-by-id", now.Add(30*time.Minute), "failed", "build error"); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r-first-by-id", TaskID: "a1b2", Sandbox: "s2",
		Attempt: 2, StartedAt: now.Add(time.Hour),
	}, 0); err != nil {
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
	}, 0); err != nil {
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
	}, 0); err != nil {
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
	}, 0); err != nil {
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
	}, 0); err != nil {
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
	}, 0); err != nil {
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
			}, 0)
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
		}, 0); err != nil {
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
		}, 0); err != nil {
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
	}, 0); err != nil {
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
	if err := store.StartRun(ctx, first, 0); err != nil {
		t.Fatalf("first StartRun for an idle task: %v", err)
	}

	second := model.Run{ID: "a1b2-2", TaskID: "a1b2", Sandbox: "a1b2-2", Attempt: 2, StartedAt: now}
	if err := store.StartRun(ctx, second, 0); err == nil {
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
	if err := store.StartRun(ctx, second, 0); err != nil {
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
		if err := store.StartRun(ctx, run, 2); err != nil {
			t.Fatalf("StartRun for %s within the limit: %v", id, err)
		}
	}

	third := model.Run{ID: "e5f6-1", TaskID: "e5f6", Sandbox: "e5f6-1", Attempt: 1, StartedAt: now}
	err := store.StartRun(ctx, third, 2)
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
	if err := store.StartRun(ctx, third, 2); err != nil {
		t.Fatalf("StartRun into freed capacity: %v", err)
	}

	// A limit of zero or less means "no limit of mine to enforce" -- what
	// a caller with its own reason to record a run passes.
	fourth := model.Run{ID: "a1b2-2", TaskID: "a1b2", Sandbox: "a1b2-2", Attempt: 2, StartedAt: now.Add(2 * time.Hour)}
	if err := store.StartRun(ctx, fourth, 0); err != nil {
		t.Fatalf("StartRun with no limit: %v", err)
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
		PollInterval: 30 * time.Second, MaxConcurrent: 2,
		// AgentFramework is named explicitly rather than left zero
		// because it is the one Config field that never reads back
		// exactly as written: GetConfig runs it through
		// model.NormalizeAgentFramework, so "" and the legacy "gemini"
		// both come back as AgentFrameworkAntigravity (see
		// TestGetConfigNormalizesTheLegacyAgentFrameworkName below).
		AgentFramework: model.AgentFrameworkAntigravity,
		GeminiModel:    "gemini-2.5-pro", ClaudeModel: "claude-sonnet-5", MaxAgentTurns: 40,
		GitHubHost: "github.com", GitHubInsecureHTTP: false,
		GCPProject: "grain-prod", GCPServiceAccountEmail: "agent@grain-prod.iam.gserviceaccount.com",
		TargetRepos:     []string{"acme/widgets", "acme/gadgets"},
		SandboxCPUs:     4,
		SandboxMemoryMB: 8192,
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
	updated.MaxConcurrent = 1
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
	if got.GeminiModel != "gemini-2.5-pro" || len(got.TargetRepos) != 0 || got.MaxConcurrent != 2 {
		t.Fatalf("got %+v, want the pre-existing row intact, targetRepos empty, and maxConcurrent migrated from the old two-name slots column", got)
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
	if got.GeminiModel != "gemini-2.5-pro" || got.MaxConcurrent != 3 {
		t.Fatalf("got %+v, want the pre-existing row intact and maxConcurrent migrated from the old three-name slots column", got)
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
	if got.SandboxCPUs != 0 || got.SandboxMemoryMB != 0 {
		t.Fatalf("SandboxCPUs/SandboxMemoryMB after migrating = %d/%d, want 0/0", got.SandboxCPUs, got.SandboxMemoryMB)
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
	if got.SandboxCPUs != 0 || got.SandboxMemoryMB != 0 {
		t.Fatalf("SandboxCPUs/SandboxMemoryMB after migrating = %d/%d, want 0/0", got.SandboxCPUs, got.SandboxMemoryMB)
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
