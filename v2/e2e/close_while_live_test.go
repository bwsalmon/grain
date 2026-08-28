// TestClosingATaskWhileItsRunIsStillLiveNeverReDispatchesOrOpensAPullRequest
// is bwsalmon/agents#332's own scenario, filed by bwsalmon/agents#319 as
// one of a dozen siblings extending v2/e2e's whole-pipeline coverage to
// model/state.go's StateOf: a task closed while its run is still live.
// model/simulate_test.go's TestClosingATaskWhileItsRunIsStillLiveOutranks
// Running already proves, at the store level, that such a task reads
// closed even though its slot stays occupied until FinishRun actually
// runs. What no store-level test can check is what happens to the run
// itself once it does finish: this drives a real `grain close` through
// the CLI binary while a dispatch's slot is still claimed, lets the
// scripted agent's turn actually push a branch and RunDispatch call
// FinishRun (a real sandbox would not be killed mid-flight just because
// someone closed the issue), and then proves both that dispatch.Cycle
// never re-dispatches the now-freed slot to it and that
// orchestrator.ProcessResult never turns that push into a real pull
// request -- closing a task means nobody wants its work merged, whatever
// its already-live run went on to do.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestClosingATaskWhileItsRunIsStillLiveNeverReDispatchesOrOpensAPullRequest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bin := buildGrainCLI(t)

	const owner, repoName = "acme", "widgets"
	upstream := t.TempDir()
	bare := filepath.Join(upstream, owner, repoName+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "git", "init", "--bare", "-b", "main", bare)
	run(t, upstream, "git", "-C", bare, "config", "http.receivepack", "true")
	seedParent := t.TempDir()
	run(t, seedParent, "git", "clone", bare, "seed")
	seed := filepath.Join(seedParent, "seed")
	run(t, seed, "git", "config", "user.email", "seed@example.com")
	run(t, seed, "git", "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "NOTES.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "git", "add", "NOTES.md")
	run(t, seed, "git", "commit", "-q", "-m", "initial commit")
	run(t, seed, "git", "push", "origin", "main")

	sim := &syncedSim{sim: githubsim.New(owner, repoName, bare, "main")}
	githubHost := githubHostServer(t, sim, upstream)

	// File the task the way an operator would, through the real CLI.
	storeDir := t.TempDir()
	created := runCLI(t, bin,
		"-data-dir", storeDir,
		"-json",
		"create",
		"-title", "add a NOTES entry",
		"-body", "please add a line to NOTES.md",
		"-repo", owner+"/"+repoName,
		"-approve",
	)
	var task ui.Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("parsing grain create -json output: %v\n%s", err, created)
	}
	if task.State != model.StateQueued {
		t.Fatalf("state after create = %q, want queued", task.State)
	}

	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	const slot = "close-live-6d934c83-1"
	root, err := sandboxes.RootFor(slot)
	if err != nil {
		t.Fatal(err)
	}
	remote := "http://" + githubHost + "/" + owner + "/" + repoName + ".git"
	if err := mcp.ConfigureGitCredentials(root, remote, "unused"); err != nil {
		t.Fatal(err)
	}
	branch := model.BranchName(task.ID)

	// Dispatch the task: its run now occupies the slot (dispatch.Cycle
	// calls StartRun), but nothing has run the agent or called FinishRun
	// yet -- a live run, exactly the race this test needs.
	var d dispatch.Dispatch
	var fullTask model.Task
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		dispatches, err := dispatch.Cycle(ctx, store, []string{slot}, baseTime)
		if err != nil {
			t.Fatal(err)
		}
		if len(dispatches) != 1 || dispatches[0].TaskID != task.ID {
			t.Fatalf("Cycle dispatched %+v, want exactly %s", dispatches, task.ID)
		}
		d = dispatches[0]

		st, err := store.State(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st != model.StateRunning {
			t.Fatalf("state after dispatch = %q, want running", st)
		}

		got, err := store.GetTask(ctx, task.ID)
		if err != nil || got == nil {
			t.Fatalf("GetTask(%s): %v (nil=%v)", task.ID, err, got == nil)
		}
		fullTask = *got
	})

	// Close the task through the real CLI while its run is still live --
	// the same command an operator would run at a shell, racing an agent
	// that has already claimed the slot.
	closed := runCLI(t, bin, "-data-dir", storeDir, "-json", "close", task.ID)
	var closedTask ui.Task
	if err := json.Unmarshal([]byte(closed), &closedTask); err != nil {
		t.Fatalf("parsing grain close -json output: %v\n%s", err, closed)
	}
	if closedTask.State != model.StateClosed {
		t.Fatalf("state after grain close = %q, want closed even with a run still live", closedTask.State)
	}

	// Let the scripted agent's run actually finish and push its branch --
	// a real sandbox would not be killed mid-flight just because someone
	// closed the issue -- and let RunDispatch call FinishRun exactly as it
	// would for any other run.
	var result *agent.Result
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		st, err := store.State(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st != model.StateClosed {
			t.Fatalf("state before the run finishes = %q, want closed (the slot is still occupied, but closing outranks running)", st)
		}
		if occ, err := store.OccupiedSlots(ctx); err != nil || len(occ) != 1 {
			t.Fatalf("occupied slots before the run finishes = %v (%v), want the slot still held", occ, err)
		}

		fw := gemini.NewForTest(&scriptedGenerator{responses: pushScript(remote, branch, task.ID)})
		tools := mcp.NewSandboxTools(root)
		result, err = orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, fullTask, d, tools, root, baseTime.Add(time.Minute))
		if err != nil {
			t.Fatalf("RunDispatch: %v", err)
		}
		if !pushedOK(result) {
			t.Fatalf("agent run did not push cleanly: %+v", result.ToolCalls)
		}

		st, err = store.State(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st != model.StateClosed {
			t.Fatalf("state after FinishRun = %q, want still closed", st)
		}
		if occ, err := store.OccupiedSlots(ctx); err != nil || len(occ) != 0 {
			t.Fatalf("occupied slots after FinishRun = %v (%v), want none: FinishRun frees the slot", occ, err)
		}
	})

	// The slot is free again, but dispatch.Cycle must never hand a closed
	// task another run.
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		again, err := dispatch.Cycle(ctx, store, []string{slot}, baseTime.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		for _, dd := range again {
			if dd.TaskID == task.ID {
				t.Fatalf("Cycle re-dispatched a closed task: %+v", again)
			}
		}
	})

	// The part no store-level test can check: ProcessResult must never
	// open a pull request for the since-closed task's push, even though
	// the branch really is there -- the point of closing is that nobody
	// wants that work merged.
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		client := github.NewClient(sim, nil)
		if err := orchestrator.ProcessResult(ctx, store, client, fullTask, result, baseTime.Add(2*time.Minute)); err != nil {
			t.Fatalf("ProcessResult: %v", err)
		}
	})
	if n := sim.pullRequestCount(); n != 0 {
		t.Fatalf("ProcessResult opened %d pull request(s) for a since-closed task, want none", n)
	}

	// Closing a task does not undo work already in flight -- only that
	// nobody turns it into a merge -- so the pushed branch is still there.
	if !branchExistsInBare(t, bare, branch) {
		t.Fatalf("expected the agent's branch %q to exist in the upstream repo even though no PR was opened", branch)
	}

	// Confirm the loop stayed closed the way an operator would actually
	// see it, by asking the CLI itself in a fresh subprocess.
	got := runCLI(t, bin, "-data-dir", storeDir, "-json", "get", task.ID)
	var detail ui.TaskDetail
	if err := json.Unmarshal([]byte(got), &detail); err != nil {
		t.Fatalf("parsing grain get -json output: %v\n%s", err, got)
	}
	if detail.State != model.StateClosed {
		t.Fatalf("task %s state = %q as the CLI reports it, want closed", task.ID, detail.State)
	}
}

// branchExistsInBare reports whether ref exists in the bare repo at path
// bare -- world.branchExists' own check, duplicated here since this test
// builds no world (see harness_test.go's own doc comment on why small
// helpers are duplicated per file rather than shared).
func branchExistsInBare(t *testing.T, bare, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", ref)
	return cmd.Run() == nil
}
