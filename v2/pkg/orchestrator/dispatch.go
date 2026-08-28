package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/loop"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// BuildPrompt is the prompt a dispatched run receives — deliberately
// plain: the task's own title and body plus exactly the two facts that
// are grain's own, never the agent's to guess (which branch to push, and
// which repo it lives in), the same "deterministic, not self-reported"
// reasoning model.BranchName's own doc comment gives, restated here at the
// one place that fact reaches the prompt.
func BuildPrompt(task model.Task) string {
	return fmt.Sprintf(
		"%s\n\n%s\n\nWork in %s. Push your change to a new branch named %q -- "+
			"never to the repo's default branch directly.",
		task.Title, task.Body, task.Target, model.BranchName(task.ID),
	)
}

// RunDispatch drives one loop.Dispatch to completion: it runs framework
// against sandboxRoot (see the package doc comment on why that is a plain
// host directory rather than a real sandbox VM) and records the run's
// outcome once it returns. It does not touch task_observation or GitHub
// at all -- see ProcessResult for that half, kept separate the same way
// v2/e2e's own runDispatch and its caller are, since deciding what a run
// produced is a different question from deciding what to do about it.
func RunDispatch(ctx context.Context, store *model.Store, framework agent.Framework,
	task model.Task, d loop.Dispatch, sandboxRoot string, at time.Time) (*agent.Result, error) {

	result, err := framework.Run(ctx, agent.RunConfig{Prompt: BuildPrompt(task), SandboxRoot: sandboxRoot})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: running %s: %w", d.RunID, err)
	}

	outcome := "succeeded"
	for _, c := range result.ToolCalls {
		if c.IsError {
			outcome = "failed"
			break
		}
	}
	if err := store.FinishRun(ctx, d.RunID, at, outcome); err != nil {
		return nil, fmt.Errorf("orchestrator: finishing run %s: %w", d.RunID, err)
	}
	return result, nil
}
