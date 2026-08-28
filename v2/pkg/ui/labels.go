// Package ui is a standalone JSON API (and the static frontend it serves)
// for creating and managing grain tasks and their capability grants,
// without needing a running graind/orchestrator to talk to.
//
// It reads and writes GitHub directly, the same task repo cmd/graind's
// own pkg/orchestrator.Config.TaskRepo names: state and capability grants
// are GitHub labels (grain/automation/labels.py's taxonomy, unchanged
// here), and a task's declared fields (/repo, /base, /auto-merge) are
// directive lines in the issue body (pkg/orchestrator.ParseDirectives'
// own grammar). That is a deliberate scope cut, not an oversight -- see
// v2/README.md's ui/ section for why: the store-backed intake path
// (issue -> model.Task) is not wired into any running binary yet, so
// GitHub is the only place a task genuinely lives today, and reading it
// straight from there is what keeps this package a *view*, never a
// second copy of the record, per docs/data-model.md's "the UI is not a
// fourth record" rule.
package ui

// Labels is the label taxonomy a task repo uses, mirroring
// grain/automation/labels.py's AutomationConfig field defaults. A
// deployment that renamed its labels there must rename them here too --
// the same "this is host-side config the two ends should agree on"
// caveat labels.py's own module doc states about its `task_labels()`
// default vs. a live AutomationConfig.
type Labels struct {
	Trigger       string
	InProgress    string
	AwaitingReply string
	NeedsApproval string
	Completed     string
}

// DefaultLabels matches AutomationConfig's own field defaults
// (grain/automation/config.py).
func DefaultLabels() Labels {
	return Labels{
		Trigger:       "grain-agent",
		InProgress:    "grain-agent-in-progress",
		AwaitingReply: "grain-agent-awaiting-reply",
		NeedsApproval: "grain-agent-needs-approval",
		Completed:     "grain-agent-completed",
	}
}

// Capability is one attachable, opt-in capability label -- the
// CAPABILITY-tier rows of labels.py's own _STYLES table that a human
// actually toggles by hand. waiting_on_dependency_label is deliberately
// excluded: labels.py's own doc comment marks it as the one CAPABILITY
// label grain applies itself, never a human, so it does not belong in a
// picker a human drives.
type Capability struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DefaultCapabilities matches labels.py's _STYLES table, human-toggled
// rows only.
func DefaultCapabilities() []Capability {
	return []Capability{
		{ID: "gemini-key", Label: "grain-gemini-key", Name: "Gemini key",
			Description: "Mint a short-lived Gemini API key for this task"},
		{ID: "self-debug", Label: "grain-self-debug", Name: "Self debug",
			Description: "Let this task read grain's own controller logs"},
		{ID: "self-repair", Label: "grain-self-repair", Name: "Self repair",
			Description: "Let this task restart grain services or reboot/reformat its sandbox"},
		{ID: "scratch-repo", Label: "grain-scratch-repo", Name: "Scratch repo",
			Description: "Dispatch this task into its sandbox's own scratch repo instead of /repo"},
	}
}

// State is a task's derived lifecycle state -- computed off labels the
// same way v1's queue reads it, never stored anywhere of this package's
// own. Order is precedence, matching model.StateOf's own "order is
// precedence, not preference" rule one layer up: a task carrying both
// completed and in-progress (a label left behind by a stale update) reads
// completed, since that is the label a poll only ever adds last.
type State string

const (
	StateCompleted     State = "completed"
	StateAwaitingReply State = "awaiting_reply"
	StateRunning       State = "running"
	StateNeedsApproval State = "needs_approval"
	StateQueued        State = "queued"
	StateUntracked     State = "untracked"
)

// deriveState reads a task's state off its labels, in the precedence
// order above.
func deriveState(labels map[string]struct{}, l Labels) State {
	has := func(name string) bool { _, ok := labels[name]; return ok }
	switch {
	case has(l.Completed):
		return StateCompleted
	case has(l.AwaitingReply):
		return StateAwaitingReply
	case has(l.InProgress):
		return StateRunning
	case has(l.NeedsApproval):
		return StateNeedsApproval
	case has(l.Trigger):
		return StateQueued
	default:
		return StateUntracked
	}
}
