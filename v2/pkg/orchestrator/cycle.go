package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/loop"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Deps is everything one RunCycle call needs. Framework is a factory, not
// a shared instance: a real deployment gives every dispatch its own agent
// conversation, the way gemini.Framework.Run's own in-process MCP server
// is scoped to one run's own tools at a time. Sandboxes is HostSandboxes
// for the local-directory stand-in, or KonturSandboxes for a real
// bwsalmon/kontur-managed VM per slot — see the package doc comment.
type Deps struct {
	Store     *model.Store
	Client    github.Client
	Sandboxes Sandboxes
	Framework func() agent.Framework
	Config    Config
	Slots     []string
}

// RunCycle is v2's whole Orchestrator.run_once equivalent: poll the task
// repo, let loop.Cycle decide what runs, actually run it, turn each
// result into the GitHub effect it implies, and refresh every pull
// request grain is still watching. A deployment's own timer calls this
// once per tick; nothing here loops on its own.
func RunCycle(ctx context.Context, deps Deps, now time.Time) error {
	if err := PollIssues(ctx, deps.Store, deps.Client, deps.Config, now); err != nil {
		return err
	}

	dispatches, err := loop.Cycle(ctx, deps.Store, deps.Slots, now)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	for _, d := range dispatches {
		if err := runOne(ctx, deps, d, now); err != nil {
			return err
		}
	}

	return SyncPullRequests(ctx, deps.Store, deps.Client, now)
}

func runOne(ctx context.Context, deps Deps, d loop.Dispatch, now time.Time) error {
	task, err := deps.Store.GetTask(ctx, d.TaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading task %s: %w", d.TaskID, err)
	}
	if task == nil {
		return fmt.Errorf("orchestrator: loop.Cycle dispatched unknown task %s", d.TaskID)
	}

	tools, err := deps.Sandboxes.ToolsFor(ctx, d.Slot)
	if err != nil {
		return err
	}

	var sandboxRoot string
	if deps.Config.Capabilities != nil && len(task.Grants) > 0 {
		rooted, ok := deps.Sandboxes.(rootedSandboxes)
		if !ok {
			return fmt.Errorf("orchestrator: task %s requests capabilities but slot %s's sandbox has no local directory to place them in", task.ID, d.Slot)
		}
		sandboxRoot, err = rooted.RootFor(d.Slot)
		if err != nil {
			return err
		}
	}

	result, err := RunDispatch(ctx, deps.Store, deps.Framework(), deps.Config, *task, d, tools, sandboxRoot, now)
	if err != nil {
		return err
	}
	return ProcessResult(ctx, deps.Store, deps.Client, *task, result, now)
}
