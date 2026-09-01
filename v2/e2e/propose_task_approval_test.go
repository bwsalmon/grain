// TestProposedTaskWaitsForApprovalThenRunsThroughTheCLI is
// bwsalmon/agents#331's own scenario, the whole-pipeline sibling of
// pkg/orchestrator/finish_test.go's
// TestProcessResultFilesAProposedTaskIntoTheStore, which proves only the
// filing half in isolation. Here, an agent completing its own task (a
// real push, over a real local git server) also calls propose_task once
// (mcp's own contract, pkg/mcp/mock_tools.go's proposeTaskTool) in the
// same turn -- relayProposedTasks runs unconditionally regardless of how
// else a run ended (finish.go's own doc comment), so both effects land
// from one ProcessResult call. The resulting task must read as
// model.StateProposed (Approval nil) and never be dispatchable, even with
// a free slot sitting right there, until a human approves it -- which
// here means the real `grain approve` CLI verb (cmd/grain/main.go's
// cmdApprove), not a direct store write. Once approved, a following
// dispatch.Cycle must pick it up, and it runs a normal push/PR/merge/close
// cycle of its own, exactly like any other task.
//
// The CLI subprocess and this test's own dispatch/ProcessResult calls
// take turns owning storeDir rather than holding it open for the whole
// test, the same discipline TestCLICreatesTaskAgentOpensPRAndUserMergeClosesIt
// already holds to (cli_test.go's own withStore doc comment) -- not
// because the embedded SQLite store requires it, but because taking
// turns is what proves the handoff between the two writers goes through
// the store rather than through anything held in memory.
// This test reuses that file's rig (syncedSim, githubHostServer, a plain
// HostSandboxes root credentialed directly against the git/REST stand-in)
// rather than harness_test.go's gitproxy-fronted world, since gitproxy
// authorizes off one fixed *model.Store captured at build time and has no
// supported way to be repointed at a fresh store once the old one closes.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

// pushAndProposeScript is pushScript's own clone/commit/push, plus one
// propose_task call in the same turn -- what "completing its own task
// while also proposing a follow-up" looks like scripted through the real
// mocked tool, rather than the hand-built agent.ToolCall
// finish_test.go's own unit test uses.
func pushAndProposeScript(remote, branch, taskID, title, body string) []*genai.GenerateContentResponse {
	cmd := "git clone " + remote + " work && cd work && " +
		"git checkout -b " + branch + " && " +
		"echo 'change for " + taskID + "' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'agent commit for " + taskID + "' && " +
		"git push origin " + branch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		toolCall("propose_task", map[string]any{"title": title, "body": body}),
		finalText("pushed " + branch + " and proposed a follow-up"),
	}
}

func TestProposedTaskWaitsForApprovalThenRunsThroughTheCLI(t *testing.T) {
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
	remote := "http://" + githubHost + "/" + owner + "/" + repoName + ".git"
	client := github.NewClient(sim, nil)

	// One credentialed directory per run. This test drives runDispatch
	// directly rather than through a Sandboxes backend, so it seeds
	// world.roots itself; a real dispatch builds each of these as it goes.
	// It used to need two *slots* for the same reason it needs two
	// directories here -- a slot's root was the same directory every time,
	// so the leftover "work" clone from the parent's run collided with the
	// proposal's.
	credentialedRoot := func() string {
		root := t.TempDir()
		if err := mcp.ConfigureGitCredentials(root, remote, "unused"); err != nil {
			t.Fatal(err)
		}
		return root
	}

	storeDir := t.TempDir()
	const parentID = "task-parent-1"
	parentTarget := model.RepoRef{Owner: owner, Name: repoName}
	parentBranch := model.BranchName(parentID)

	clock := baseTime
	var proposalID string

	// Phase 1: file the parent task the way a human would, dispatch it,
	// and have its own run both push a branch and call propose_task.
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		w := &world{t: t, store: store, ctx: ctx, roots: map[string]string{parentID + "-r1": credentialedRoot()}}
		fileIssue(w, parentID, human("alice"), parentTarget)
		assertState(w, parentID, model.StateQueued, false)

		dispatches, err := dispatch.Cycle(ctx, store, 1, clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(dispatches) != 1 || dispatches[0].TaskID != parentID {
			t.Fatalf("Cycle dispatched %+v, want exactly %s", dispatches, parentID)
		}
		assertState(w, parentID, model.StateRunning, true)

		clock = clock.Add(time.Minute)
		script := pushAndProposeScript(remote, parentBranch, parentID,
			"follow-up: document NOTES.md's format",
			"a human should describe the format before more agents touch it")
		result := w.runDispatch(dispatches[0], script, clock)
		if !pushedOK(result) {
			t.Fatalf("parent run did not push cleanly: %+v", result.ToolCalls)
		}
		if occ, _ := store.LiveRunCount(ctx); occ != 0 {
			t.Fatalf("occupied slots after finish = %v, want none", occ)
		}

		parentTask, err := store.GetTask(ctx, parentID)
		if err != nil || parentTask == nil {
			t.Fatalf("GetTask(%s): %v", parentID, err)
		}
		clock = clock.Add(time.Minute)
		if err := orchestrator.ProcessResult(ctx, store, client, *parentTask, result, dispatches[0].RunID, clock); err != nil {
			t.Fatalf("ProcessResult: %v", err)
		}

		// The parent's own push still finishes normally -- relayProposedTasks
		// runs unconditionally before anything else, it does not replace the
		// rest of ProcessResult.
		assertState(w, parentID, model.StateCompleted, false)

		tasks, err := store.ListTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range tasks {
			if tk.ID == parentID {
				continue
			}
			for _, l := range tk.Links {
				if l.Kind == model.LinkProposedBy && l.Target == parentID {
					proposalID = tk.ID
				}
			}
		}
		if proposalID == "" {
			t.Fatalf("no task was filed as proposed-by %s; tasks = %+v", parentID, tasks)
		}

		proposal, err := store.GetTask(ctx, proposalID)
		if err != nil || proposal == nil {
			t.Fatalf("GetTask(%s): %v", proposalID, err)
		}
		if proposal.Approval != nil {
			t.Fatalf("proposal %s has Approval = %+v, want nil", proposalID, proposal.Approval)
		}
		if len(proposal.Links) != 1 || proposal.Links[0].Kind != model.LinkProposedBy || proposal.Links[0].Target != parentID {
			t.Fatalf("proposal %s links = %+v, want proposed-by %s", proposalID, proposal.Links, parentID)
		}
		assertState(w, proposalID, model.StateProposed, false)

		// Not dispatchable, even with the slot the parent's own run just
		// freed up.
		clock = clock.Add(time.Minute)
		stillProposed, err := dispatch.Cycle(ctx, store, 1, clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(stillProposed) != 0 {
			t.Fatalf("Cycle dispatched a proposed, unapproved task: %+v", stillProposed)
		}
	})

	// Phase 2: approve it through the real CLI binary, not a direct store
	// write -- storeDir is untouched by anything else while this runs.
	approved := runCLIStore(t, bin, storeDir, "-json", "approve", proposalID)
	var approvedTask ui.Task
	if err := json.Unmarshal([]byte(approved), &approvedTask); err != nil {
		t.Fatalf("parsing grain approve -json output: %v\n%s", err, approved)
	}
	if approvedTask.ID != proposalID {
		t.Fatalf("grain approve responded about %q, want %q", approvedTask.ID, proposalID)
	}
	if approvedTask.State != model.StateQueued {
		t.Fatalf("state after grain approve = %q, want queued", approvedTask.State)
	}

	// Phase 3: now it dispatches, and runs a normal push/PR/merge/close
	// cycle of its own, exactly like any other task.
	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		w := &world{t: t, store: store, ctx: ctx, roots: map[string]string{proposalID + "-r1": credentialedRoot()}}

		clock = clock.Add(time.Minute)
		dispatches, err := dispatch.Cycle(ctx, store, 1, clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(dispatches) != 1 || dispatches[0].TaskID != proposalID {
			t.Fatalf("Cycle after approval dispatched %+v, want exactly %s", dispatches, proposalID)
		}

		proposalBranch := model.BranchName(proposalID)
		clock = clock.Add(time.Minute)
		result := w.runDispatch(dispatches[0], pushScript(remote, proposalBranch, proposalID), clock)
		if !pushedOK(result) {
			t.Fatalf("approved task's run did not push cleanly: %+v", result.ToolCalls)
		}

		proposal, err := store.GetTask(ctx, proposalID)
		if err != nil || proposal == nil {
			t.Fatalf("GetTask(%s): %v", proposalID, err)
		}
		clock = clock.Add(time.Minute)
		if err := orchestrator.ProcessResult(ctx, store, client, *proposal, result, dispatches[0].RunID, clock); err != nil {
			t.Fatalf("ProcessResult after approval: %v", err)
		}
		assertState(w, proposalID, model.StateCompleted, false)

		updated, err := store.GetTask(ctx, proposalID)
		if err != nil || updated == nil {
			t.Fatalf("GetTask(%s): %v", proposalID, err)
		}
		var prNumber int
		for _, l := range updated.Links {
			if l.Kind == model.LinkFixes {
				ref, err := model.ParsePullRequestRef(l.Target)
				if err != nil {
					t.Fatalf("parsing pull request link %q: %v", l.Target, err)
				}
				prNumber = ref.Number
			}
		}
		if prNumber == 0 {
			t.Fatalf("approved task has no pull request link: %+v", updated.Links)
		}

		// GitHub itself merges the PR -- a real git merge over the same
		// local git server, plus a real HTTP PUT to the merge endpoint from
		// a second, independent github.Client, standing in for a human
		// clicking "Merge pull request", exactly
		// TestCLICreatesTaskAgentOpensPRAndUserMergeClosesIt's own step 3.
		mergeParent := t.TempDir()
		run(t, mergeParent, "git", "clone", remote, "merge")
		mergeWd := filepath.Join(mergeParent, "merge")
		run(t, mergeWd, "git", "config", "user.email", "github@example.com")
		run(t, mergeWd, "git", "config", "user.name", "github (simulated merge)")
		run(t, mergeWd, "git", "fetch", "origin", proposalBranch)
		run(t, mergeWd, "git", "checkout", "main")
		run(t, mergeWd, "git", "merge", "--no-ff", "origin/"+proposalBranch, "-m", "Merge "+proposalBranch)
		run(t, mergeWd, "git", "push", "origin", "main")

		userTransport := github.NewRealTransport(githubHost)
		userTransport.UseTLS = false
		userClient := github.NewClient(userTransport, nil)
		if err := userClient.MergePullRequest(owner, repoName, prNumber); err != nil {
			t.Fatalf("submitting (merging) the approved task's pull request: %v", err)
		}

		clock = clock.Add(time.Minute)
		if err := orchestrator.SyncPullRequests(ctx, store, client, clock); err != nil {
			t.Fatalf("SyncPullRequests: %v", err)
		}
		assertState(w, proposalID, model.StateClosed, false)
	})
}
