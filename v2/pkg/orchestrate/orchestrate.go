// Package orchestrate is the side-effecting counterpart loop.Cycle's own
// doc comment says a later change would give it: for each Dispatch,
// resolve and materialize whatever capabilities the task carries, run the
// agent for real, open the pull request its push implies, and revoke
// what was minted -- then, once a cycle, poll GitHub for the pull
// requests that resulted and fold what changed back into the store as an
// Observation. cmd/graind is the process that calls this on a timer;
// this package holds the decision of what one call does, so it is
// testable against fakes the way loop and model already are.
//
// Everything here assumes bwsalmon/agents#254's own simplification: the
// MCP server's sandbox tools are confined to a local directory rather
// than a real remote VM (v2 has no host adapter yet -- see v2/README.md),
// and Slots names the one concurrency pool a single machine offers, not
// a fleet. Reconciler itself places no limit on len(Slots); a Config with
// several is exactly what a later host adapter needs nothing here to
// change to serve.
package orchestrate

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/loop"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Config is everything one Reconciler needs. Every field is a seam a test
// substitutes a fake for -- GitHub and Agent especially, so this
// package's own tests need no live API key and no network.
type Config struct {
	Store *model.Store
	// Slots is the whole concurrency pool passed to loop.Cycle, and Roots
	// is where each one's sandbox tools are confined -- one local
	// directory standing in for a real sandbox, per bwsalmon/agents#254.
	Slots []string
	Roots map[string]string

	Agent         agent.Framework
	Capabilities  *model.CapabilityRegistry
	Credentials   model.CredentialResolver
	MaxAgentTurns int

	// GitHub is nil-able: a Reconciler with no GitHub client still
	// dispatches and materializes capabilities, it just never opens a
	// pull request or polls one -- useful for a test, or a deployment
	// that has not configured a token yet.
	GitHub github.Client

	// Logger defaults to log.Default() if nil.
	Logger *log.Logger
}

// Reconciler runs Config's dispatch-and-sync cycle on demand or on a
// timer.
type Reconciler struct {
	cfg Config
	log *log.Logger
}

func New(cfg Config) *Reconciler {
	l := cfg.Logger
	if l == nil {
		l = log.Default()
	}
	return &Reconciler{cfg: cfg, log: l}
}

// Run calls Tick every interval until ctx is done, logging (never
// panicking on) whatever Tick itself returns -- one bad cycle must not
// take the whole daemon down, since the next tick gets another chance at
// whatever failed. Ticks are not overlapped: Run waits for one Tick to
// return before the next interval starts, so a slow GitHub poll simply
// delays the next dispatch rather than racing it.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	r.tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	if err := r.Tick(ctx, time.Now().UTC()); err != nil {
		r.log.Printf("orchestrate: tick: %v", err)
	}
}

// Tick is one reconcile pass: dispatch every task loop.Cycle finds ready,
// run each to completion, then poll GitHub for what changed on every
// task still worth asking about. Errors from an individual dispatch or
// sync are logged and do not stop the rest of the pass -- the same "one
// already-failed item must not stop the others" discipline
// model.Reaper's own doc comment describes for Reap.
func (r *Reconciler) Tick(ctx context.Context, now time.Time) error {
	dispatches, err := loop.Cycle(ctx, r.cfg.Store, r.cfg.Slots, now)
	if err != nil {
		return fmt.Errorf("orchestrate: cycle: %w", err)
	}
	for _, d := range dispatches {
		r.runDispatch(ctx, d, now)
	}
	r.syncGitHub(ctx, now)
	return nil
}

// runDispatch drives one loop.Cycle Dispatch to completion: resolve and
// materialize its task's capabilities, run the agent, open the pull
// request a successful push implies, revoke what was minted, and record
// the outcome. The run is already durable by the time Cycle returned this
// Dispatch (loop.Dispatch's own doc comment), so every error path here
// still finishes it -- an unfinished run would hold its slot forever.
func (r *Reconciler) runDispatch(ctx context.Context, d loop.Dispatch, now time.Time) {
	task, err := r.cfg.Store.GetTask(ctx, d.TaskID)
	if err != nil || task == nil {
		r.log.Printf("orchestrate: dispatch %s: loading task %s: %v", d.RunID, d.TaskID, err)
		r.finish(ctx, d.RunID, now, "failed")
		return
	}
	root := r.cfg.Roots[d.Slot]
	if root == "" {
		r.log.Printf("orchestrate: dispatch %s: slot %q has no sandbox root configured", d.RunID, d.Slot)
		r.finish(ctx, d.RunID, now, "failed")
		return
	}

	run := model.Run{ID: d.RunID, TaskID: d.TaskID, Slot: d.Slot, Sandbox: d.Slot, Attempt: d.Attempt, StartedAt: now}
	cc := model.CapabilityContext{Task: *task, Run: run, Now: now, Workdir: root, Credentials: r.cfg.Credentials}

	materialized, prompt, outcome := r.prepare(ctx, cc, *task, root)
	var result *agent.Result
	if outcome == "" {
		result, outcome = r.runAgent(ctx, prompt, root)
	}
	if outcome == "" {
		outcome = "succeeded"
		// The push, if any, already happened; failing to open the PR (or
		// to relay a question/comment/proposed task) is worth logging and
		// retrying next pass, not worth failing the run over --
		// FindOpenPullRequestForBranch inside syncGitHub and a retried
		// CreatePullRequest here are both idempotent, and reportOutcome
		// itself logs rather than propagates every effect it attempts.
		// See effects.go's own doc comment for what this call covers.
		r.reportOutcome(ctx, *task, result, now)
	}

	r.finish(ctx, d.RunID, now, outcome)
	r.revoke(ctx, cc, materialized)
}

// prepare resolves and materializes cc.Task's capability grants and
// assembles the prompt they and the task itself contribute. A non-empty
// outcome means preparation itself failed (or a grant was refused) and
// runDispatch must not run the agent at all -- the same "a
// half-materialized capability is never described to the agent as
// present" rule model.MaterializeGrants's own doc comment holds to, one
// level up: an agent whose capability request was refused must not run
// at all, since the task it would work almost always depends on it.
func (r *Reconciler) prepare(ctx context.Context, cc model.CapabilityContext, task model.Task, root string) (materialized []model.Materialized, prompt string, outcome string) {
	prompt = strings.TrimSpace(task.Title + "\n\n" + task.Body)

	resolved, err := model.ResolveGrants(ctx, r.cfg.Capabilities, cc)
	if err != nil {
		r.log.Printf("orchestrate: task %s: resolving capabilities: %v", task.ID, err)
		return nil, prompt, "failed"
	}
	for _, gr := range resolved {
		if gr.Resolution.Refused {
			r.log.Printf("orchestrate: task %s: capability %q refused: %s",
				task.ID, gr.Grant.Capability, gr.Resolution.Reason)
			return nil, prompt, "failed"
		}
	}

	materialized, err = model.MaterializeGrants(ctx, r.cfg.Capabilities, cc, resolved)
	if err != nil {
		r.log.Printf("orchestrate: task %s: materializing capabilities: %v", task.ID, err)
		return materialized, prompt, "failed"
	}
	if err := applyPlacements(root, materialized); err != nil {
		r.log.Printf("orchestrate: task %s: applying placements: %v", task.ID, err)
		return materialized, prompt, "failed"
	}
	sections, err := model.PromptSections(ctx, cc, materialized)
	if err != nil {
		r.log.Printf("orchestrate: task %s: building prompt sections: %v", task.ID, err)
		return materialized, prompt, "failed"
	}
	for _, s := range sections {
		prompt += "\n\n" + s
	}
	return materialized, prompt, ""
}

// applyPlacements writes every SideSandbox placement Materialize
// returned into root, the local directory standing in for the sandbox --
// see the package doc comment. A placement's Path is always the absolute
// path it would land at inside a real sandbox (gcpkey.SandboxKeyPath,
// geminikey.KeyPath); root here plays the same role a chroot's own root
// plays for an absolute path, which is why it is joined after stripping
// the leading separator rather than passed through mcp.resolvePath --
// that function confines an agent's own tool arguments, a different
// question from where a controller-applied placement lands.
//
// A SideController placement is skipped, not written anywhere: no
// current provider returns one (gcpkey and geminikey both mint straight
// into the sandbox), and nothing in v2 has decided what a controller-side
// destination for one even is yet.
func applyPlacements(root string, materialized []model.Materialized) error {
	for _, m := range materialized {
		for _, p := range m.Materialization.Placements {
			if p.Side != model.SideSandbox {
				continue
			}
			full := filepath.Join(root, strings.TrimPrefix(p.Path, "/"))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
			}
			mode, err := strconv.ParseUint(p.EffectiveMode(), 8, 32)
			if err != nil {
				return fmt.Errorf("placement %s: invalid mode %q: %w", p.Path, p.Mode, err)
			}
			if err := os.WriteFile(full, []byte(p.Content), os.FileMode(mode)); err != nil {
				return fmt.Errorf("writing %s: %w", full, err)
			}
		}
	}
	return nil
}

// runAgent runs the agent and turns its result into an outcome string,
// "" meaning success -- agent.Result.ToolCalls is the only record of
// what happened inside the run (mcp/mock_tools.go's own sink is internal
// and discarded when Run returns), so a run that made no tool call at all
// counts as failed too: an agent that never touched run_command did not
// do the work.
func (r *Reconciler) runAgent(ctx context.Context, prompt, root string) (*agent.Result, string) {
	result, err := r.cfg.Agent.Run(ctx, agent.RunConfig{Prompt: prompt, SandboxRoot: root, MaxTurns: r.cfg.MaxAgentTurns})
	if err != nil {
		r.log.Printf("orchestrate: agent run failed: %v", err)
		return nil, "failed"
	}
	sawTool := false
	for _, c := range result.ToolCalls {
		sawTool = true
		if c.IsError {
			return result, "failed"
		}
	}
	if !sawTool {
		return result, "failed"
	}
	return result, ""
}

// openPullRequest opens the pull request a successful dispatch's push
// implies, if task.Target's branch for it exists and nothing has already
// opened one -- idempotent for the same reason
// FindOpenPullRequestForBranch's own doc comment gives: a retried
// dispatch, or a sync pass that runs before the agent's own push is even
// visible yet, must not error or duplicate.
func (r *Reconciler) openPullRequest(ctx context.Context, task model.Task, result *agent.Result) error {
	if r.cfg.GitHub == nil || task.Target == nil {
		return nil
	}
	branch := model.BranchName(task.ID)
	exists, err := r.cfg.GitHub.BranchExists(task.Target.Owner, task.Target.Name, branch)
	if err != nil {
		return fmt.Errorf("checking branch %s: %w", branch, err)
	}
	if !exists {
		// Nothing pushed -- an analyze/review-intent task, most likely,
		// which model.IntentAnalyze/model.IntentReview's own doc comments
		// describe as pushing nothing by design.
		return nil
	}
	if pr, err := r.cfg.GitHub.FindOpenPullRequestForBranch(task.Target.Owner, task.Target.Name, branch); err != nil {
		return fmt.Errorf("checking for an existing pull request: %w", err)
	} else if pr != nil {
		return nil
	}

	base := task.Base
	if base == "" {
		base, err = r.cfg.GitHub.DefaultBranch(task.Target.Owner, task.Target.Name)
		if err != nil {
			return fmt.Errorf("reading default branch: %w", err)
		}
	}
	body := task.Body
	if result != nil && result.FinalText != "" {
		body = result.FinalText
	}
	title := task.Title
	if title == "" {
		title = "grain: " + task.ID
	}
	if _, err := r.cfg.GitHub.CreatePullRequest(task.Target.Owner, task.Target.Name, branch, base, title, body); err != nil {
		return fmt.Errorf("creating pull request: %w", err)
	}
	return nil
}

// finish calls FinishRun, logging rather than propagating a failure --
// its caller has already decided the run's outcome and has nothing left
// to do differently if the write itself fails, beyond leaving the slot
// occupied until the next restart notices and a human intervenes. That
// is the same "log and move on" shape TestFailedRunReturnsTaskToQueueForRetry
// and its neighbours (e2e/e2e_test.go) already exercise one layer up.
func (r *Reconciler) finish(ctx context.Context, runID string, at time.Time, outcome string) {
	if err := r.cfg.Store.FinishRun(ctx, runID, at, outcome); err != nil {
		r.log.Printf("orchestrate: finishing run %s: %v", runID, err)
	}
}

// revoke calls Revoke on every capability materialized carried a Lease
// for, then drops it from the store -- the mirror image of prepare's
// MaterializeGrants call, run unconditionally (success or failure) so a
// failed run never strands a minted credential the way it would if
// revocation only ran on the happy path.
func (r *Reconciler) revoke(ctx context.Context, cc model.CapabilityContext, materialized []model.Materialized) {
	for _, m := range materialized {
		lease := m.Materialization.Lease
		if lease == nil {
			continue
		}
		if err := m.Provider.Revoke(ctx, cc, *lease); err != nil {
			r.log.Printf("orchestrate: task %s: revoking capability %q: %v", cc.Task.ID, m.Grant.Capability, err)
			continue
		}
		if err := r.cfg.Store.DropLease(ctx, cc.Run.ID, lease.Capability, lease.Resource); err != nil {
			r.log.Printf("orchestrate: task %s: dropping lease for capability %q: %v", cc.Task.ID, m.Grant.Capability, err)
		}
	}
}

// syncGitHub polls every model.TrackedTarget for the pull request its
// branch implies and folds what it finds back into the store as an
// Observation: an open PR means the task is complete, and a PR that was
// open before but is not anymore means it merged or was closed by a
// human -- v2's model has no separate "merged" bit (model.Observation
// carries only ClosedAt/CompletedAt), so this treats both the same way
// model.StateClosed itself does not distinguish them.
//
// This is deliberately not the TrackedPullRequest table v2/README.md
// describes as still missing: it re-derives "is there a PR for this
// branch right now" from GitHub on every pass instead of remembering a
// PR number, which costs an extra request per tracked task but needs no
// schema of its own and cannot drift from what GitHub actually reports.
func (r *Reconciler) syncGitHub(ctx context.Context, now time.Time) {
	if r.cfg.GitHub == nil {
		return
	}
	tracked, err := r.cfg.Store.TrackedTargets(ctx)
	if err != nil {
		r.log.Printf("orchestrate: sync: listing tracked targets: %v", err)
		return
	}
	for _, tt := range tracked {
		branch := model.BranchName(tt.TaskID)
		pr, err := r.cfg.GitHub.FindOpenPullRequestForBranch(tt.Target.Owner, tt.Target.Name, branch)
		if err != nil {
			r.log.Printf("orchestrate: sync: task %s: checking for a pull request: %v", tt.TaskID, err)
			continue
		}

		obs, err := r.cfg.Store.GetObservation(ctx, tt.TaskID)
		if err != nil {
			r.log.Printf("orchestrate: sync: task %s: reading observation: %v", tt.TaskID, err)
			continue
		}
		next := model.Observation{TaskID: tt.TaskID}
		if obs != nil {
			next = *obs
		}
		next.TaskID, next.ObservedAt = tt.TaskID, &now

		if pr != nil {
			if next.CompletedAt == nil {
				next.CompletedAt = &now
			}
		} else if next.CompletedAt != nil && next.ClosedAt == nil {
			next.ClosedAt = &now
		} else {
			// No PR yet, and none was ever observed -- still running,
			// nothing changed.
			continue
		}
		if err := r.cfg.Store.Observe(ctx, next); err != nil {
			r.log.Printf("orchestrate: sync: task %s: recording observation: %v", tt.TaskID, err)
		}
	}
}
