// Package ui is a JSON API (and the static frontend it serves) for
// creating and managing grain tasks and their capability grants.
//
// It reads and writes model.Store directly. That is the whole of the
// change bwsalmon's "the input is via direct model updates" asked for:
// this package used to read and write GitHub issues, deriving a task's
// state from labels and keeping its declared fields (/repo, /base,
// /auto-merge) as directive lines in an issue body, because the
// store-backed intake path was not wired into anything that ran. GitHub
// was the record and this was a view onto it.
//
// Now the store is the record. A task is a row, its state comes from
// model.StateOf's own view rather than a second label-shaped derivation
// that had already drifted (this package used to call the same state
// "needs_approval" that the store calls "proposed", and had an
// "untracked" the store has no notion of), its capabilities are
// model.Grants, and its conversation is model.Comments. Nothing here
// talks to GitHub at all: the pull request a task's run produces is
// recorded on the task as a model.LinkFixes link, so rendering it needs
// no API call.
package ui

import (
	"context"

	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/upgrade"
)

// Capability is one attachable, opt-in capability a human toggles on a
// task -- the CAPABILITY-tier rows of v1's labels.py _STYLES table that
// were genuinely human-driven.
//
// Label is gone from this type. It named the GitHub label that used to
// carry the grant; a grant is a model.Grant row now, and ID is what it
// records. waiting_on_dependency_label stays excluded for the reason it
// always was: labels.py's own doc comment marks it as the one grain
// applies itself, never a human, so it does not belong in a picker a
// human drives.
type Capability struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DefaultCapabilities matches labels.py's _STYLES table, human-toggled
// rows only.
func DefaultCapabilities() []Capability {
	return []Capability{
		{ID: "gemini-key", Name: "Gemini key",
			Description: "Mint a short-lived Gemini API key for this task"},
		{ID: "self-debug", Name: "Self debug",
			Description: "Let this task read grain's own controller logs"},
		{ID: "self-repair", Name: "Self repair",
			Description: "Let this task restart grain services or reboot/reformat its sandbox"},
		{ID: "scratch-repo", Name: "Scratch repo",
			Description: "Dispatch this task into its sandbox's own scratch repo instead of /repo"},
	}
}

// Config is what a Client needs to know about the deployment it fronts.
//
// TaskRepo and the label taxonomy are both gone: there is no task repo to
// file issues in any more, and no labels to derive anything from. What
// replaces them is Actor -- who a task created here is attributed to,
// which a GitHub issue used to answer with its own opening account.
type Config struct {
	// Actor is the principal the CLI or UI acts as. A task filed here
	// records it as both origin and approval, the same way a human who
	// could apply the trigger label used to land a task queued rather
	// than proposed.
	Actor model.Principal
	// DefaultTarget is the repo a task with no explicit one targets,
	// mirroring orchestrator.Config.DefaultTarget so a single-repo
	// deployment need not repeat itself on every task.
	DefaultTarget *model.RepoRef
	// TargetRepos restricts which repos a task's Repo (explicit or
	// defaulted) may name -- model.Config's own field of the same name,
	// mirrored here the way DefaultTarget already is. Empty means
	// unrestricted. CreateTask enforces it.
	TargetRepos  []string
	Capabilities []Capability
	// Secrets is set only when this UI runs on the same host as the
	// server whose secrets directory it names -- nil means it does not,
	// and the secrets pane and its API routes report themselves
	// unavailable rather than erroring on every call (bwsalmon/agents#357).
	// It is write/list only in the sense that matters: nothing in this
	// package ever calls Store.Resolve, so a value, once set, is never
	// readable back through here -- only Set, DeleteKey, DeleteSecret and
	// List (which reports names and key names, never values).
	Secrets *secrets.Store
	// Reboot, when set, is what the UI's "reboot host" button
	// (bwsalmon/agents#395) calls to reboot the machine this deployment's
	// daemon is running on. nil means handleRebootHost reports the action
	// unavailable rather than erroring on every call -- the same
	// nil-means-unavailable contract Secrets already gives the secrets
	// pane, and the right default for `grain demo`'s throwaway UI, which
	// has no real machine behind it worth rebooting.
	Reboot func(ctx context.Context) error
	// Credentials is the same GitHub credential ladder (secrets/github/
	// credentials.json, loaded once at startup, not hot-reloaded) the
	// git proxy resolves pushes against -- given here so Settings can
	// flag a targetRepos entry the ladder has no owner/repo, owner/*, or
	// * pattern covering. nil (`grain demo`'s throwaway UI, or any UI
	// not colocated with the proxy that built it) means Settings reports
	// no such gaps rather than erroring, the same nil-means-unavailable
	// contract Secrets and Reboot already give. Without this, the only
	// way to learn targetRepos and the credential ladder have drifted
	// apart is a confusing 500 "no credential configured" from the git
	// proxy on the next push (bwsalmon/agents#427).
	Credentials *gitproxy.CredentialSet
	// Upgrader is set only when this deployment was told where its own
	// git checkout and build/install/restart mechanics live
	// (bwsalmon/agents#396, cmd/grain/daemon.go's -upgrade-src-dir) --
	// nil means it wasn't, and the upgrade pane and its API routes
	// report themselves unavailable rather than erroring on every call,
	// the same as a nil Secrets above.
	Upgrader Upgrader
	// Logs is the set of named log sources a debugging page in the UI can
	// tail -- "daemon" for grain daemon's own journal, and, on a
	// deployment colocated with one, the git proxy's audit log
	// (bwsalmon/agents#444). Keyed by the name a caller passes to
	// GET /api/logs/{source}. nil or empty means no sources are
	// configured, and the debugging page reports itself unavailable
	// rather than a page with nothing on it, the same nil-means-
	// unavailable contract Secrets, Reboot and Upgrader above already
	// give their own panes.
	Logs map[string]LogSource
}

// Upgrader is the subset of *upgrade.Upgrader the UI needs -- an
// interface so a test can fake an upgrade without a real git checkout,
// docker daemon, or restart command behind it.
type Upgrader interface {
	Start(branch string) error
	Status() (upgrade.Status, error)
}

// DefaultActor is the principal a deployment gets without saying
// otherwise. A human ID rather than an automation one: the thing on the
// other end of this package is a person at a terminal or a browser, and
// model.LandsQueued's own rule turns on that distinction.
func DefaultActor(id string) model.Principal {
	if id == "" {
		id = "operator"
	}
	return model.Principal{Kind: model.PrincipalHuman, ID: id}
}
