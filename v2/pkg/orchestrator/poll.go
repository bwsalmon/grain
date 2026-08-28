package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// filingActor is the Principal a freshly filed task is attributed to.
// Applying the trigger label already required the same "can this person
// write to the task repo" trust docs/data-model.md's trust gate is built
// from, so the issue's own opening account lands the task queued, never
// proposed -- LandsQueued's own rule for a human filing one directly. A
// missing Author (an issue somehow returned with no user object) falls
// back to a placeholder rather than failing the whole poll over it.
func filingActor(issue github.Issue) model.Principal {
	author := issue.Author
	if author == "" {
		author = "unknown"
	}
	return model.Principal{Kind: model.PrincipalHuman, ID: author}
}

// PollIssues lists cfg.TaskRepo's issues carrying cfg.TriggerLabel and
// turns each into a queued model.Task, the v2 equivalent of core.py's own
// intake half. An issue already tracked (TaskID already has a Task) is
// either left alone (still working through its current state) or, if it
// was awaiting a reply, treated as "a human answered and re-applied the
// label": PollIssues clears the pending question so the task queues
// again, the same re-trigger v1's own trust gate relies on the label
// for. Either way, the trigger label comes off before PollIssues moves on
// to the next issue — removing it is what stops the same issue being
// queued twice by two ticks racing each other, and doing it only after
// the task is durably in the store (never before) is the same
// persistence-before-irreversible-effect ordering docs/next-session.md
// records finding a real bug from getting backwards once already.
//
// A directive error (a malformed or missing /repo, per directives.go)
// parks the issue instead: a comment explaining why, the label removed,
// no task created — the same "a human must reply with a correction"
// shape v1's own directive parser forces, since posting a task with no
// resolvable target would just fail later with a worse error.
func PollIssues(ctx context.Context, store *model.Store, client github.Client, cfg Config, now time.Time) error {
	issues, err := client.ListIssues(cfg.TaskRepo.Owner, cfg.TaskRepo.Name, cfg.TriggerLabel)
	if err != nil {
		return fmt.Errorf("orchestrator: listing labelled issues: %w", err)
	}

	for _, issue := range issues {
		id := TaskID(cfg.TaskRepo, issue.Number)
		existing, err := store.GetTask(ctx, id)
		if err != nil {
			return fmt.Errorf("orchestrator: reading task %s: %w", id, err)
		}

		if existing == nil {
			if err := fileTask(ctx, store, client, cfg, issue, id, now); err != nil {
				return err
			}
		} else if err := requeueIfAwaitingReply(ctx, store, id, now); err != nil {
			return err
		}

		if err := client.RemoveLabel(cfg.TaskRepo.Owner, cfg.TaskRepo.Name, issue.Number, cfg.TriggerLabel); err != nil {
			return fmt.Errorf("orchestrator: removing trigger label from %s#%d: %w",
				cfg.TaskRepo, issue.Number, err)
		}
	}
	return nil
}

func fileTask(ctx context.Context, store *model.Store, client github.Client, cfg Config,
	issue github.Issue, id string, now time.Time) error {

	directives, err := ParseDirectives(issue.Body)
	if err != nil {
		return parkIssue(client, cfg, issue.Number, err.Error())
	}
	target := directives.Repo
	if target == nil {
		target = cfg.DefaultTarget
	}
	if target == nil {
		return parkIssue(client, cfg, issue.Number,
			"no /repo directive, and this deployment has no default target repo configured")
	}

	actor := filingActor(issue)
	task := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  issue.Title,
		Body:   issue.Body,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: actor},
			Reason:      model.ReasonDirect,
		},
		Approval:    &model.Attribution{Actor: actor},
		Target:      target,
		Binding:     model.BindingDirective,
		Base:        directives.Base,
		AutoMerge:   directives.AutoMerge,
		ExternalRef: model.ExternalRef(cfg.TaskRepo, issue.Number),
		CreatedAt:   &now,
	}
	if err := store.PutTask(ctx, task); err != nil {
		return fmt.Errorf("orchestrator: filing task %s: %w", id, err)
	}
	return nil
}

// parkIssue leaves a labelled issue un-dispatched: a comment explaining
// why, and the label removed by PollIssues' own caller right after this
// returns.
func parkIssue(client github.Client, cfg Config, issueNumber int, reason string) error {
	comment := "Cannot dispatch this task: " + reason
	if _, err := client.CreateComment(cfg.TaskRepo.Owner, cfg.TaskRepo.Name, issueNumber, comment); err != nil {
		return fmt.Errorf("orchestrator: parking %s#%d: %w", cfg.TaskRepo, issueNumber, err)
	}
	return nil
}

// requeueIfAwaitingReply clears a task's pending question the moment its
// issue is seen carrying the trigger label again — a human replied and
// re-applied it, the only way (per model.State's own precedence) an
// awaiting_reply task ever becomes queued again.
func requeueIfAwaitingReply(ctx context.Context, store *model.Store, id string, now time.Time) error {
	obs, err := store.GetObservation(ctx, id)
	if err != nil {
		return fmt.Errorf("orchestrator: reading observation for %s: %w", id, err)
	}
	if obs == nil || obs.PendingQuestionCommentID == nil {
		return nil
	}
	return observeField(ctx, store, id, now, func(o *model.Observation) { o.PendingQuestionCommentID = nil })
}
