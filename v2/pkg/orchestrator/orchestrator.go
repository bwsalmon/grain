// Package orchestrator is v2's equivalent of v1's core.py Orchestrator:
// the component that decides *when* to call GitHub, wired to loop.Cycle's
// dispatch decisions and model.Store's state. v2/README.md's "What this
// does not have yet" section named this gap after bwsalmon/agents#243
// ported github.Client and github/githubsim but wired neither into
// anything that runs; this package is that wiring.
//
// It is a library, not a binary — RunCycle is what a deployment's own
// cron/timer loop calls once per tick, the same shape v1's `automation
// run-once` command wraps around core.py's Orchestrator.run_once. No
// cmd/ binary exists yet to call it on a real deployment; that is the
// obvious next step once this has a real GitHub credential and a real
// place to run agents, neither of which this package assumes.
//
// **Sandboxing is "execute on the host," deliberately, for now.** v2 has
// no host adapter (no way to create an isolated VM/container and run
// commands in it — v2/README.md) and this package does not build one. It
// reuses exactly the stand-in v2/e2e already validated: a plain directory
// on the machine this process runs on, which pkg/mcp's sandbox tools
// confine a run to (NewSandboxTools' own doc comment: "root stands in for
// the sandbox"). HostSandboxes below is that stand-in promoted from
// test-only code to something RunCycle actually uses. This carries no
// isolation at all — an agent given a directory here can do anything this
// process's own user can do — which is why v1 runs the agent process
// itself against a real, separate sandbox VM reached over SSH. Wiring a
// real host adapter into v2, and threading a Runner through this package
// in its place, is follow-on work; this package's own docstrings on
// HostSandboxes and RunDispatch say so at the point that would change.
package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Config is what one deployment's orchestrator needs to know: which repo
// is the task queue, and which label marks an issue as ready to dispatch.
type Config struct {
	TaskRepo     model.RepoRef
	TriggerLabel string
	// DefaultTarget is used when a task's issue body carries no /repo
	// directive — grain/automation/directives.py's own "unless the
	// deployment configures default_target_repo". Nil means every task
	// must name its target explicitly.
	DefaultTarget *model.RepoRef
}

// TaskID names the task a task-repo issue maps to, deterministically —
// the same reasoning model.BranchName and loop.RunID already give for
// their own names: two callers (a poll that just filed the task, and a
// later poll that sees the same issue again) must agree on its ID without
// coordinating through anything but the issue itself.
//
// docs/data-model.md's own representation table calls this out directly:
// "a task has a stable identity: a GitHub issue number in a repo." Owner
// and name can never themselves contain '/', so joining all three with it
// is unambiguous with no escaping.
func TaskID(taskRepo model.RepoRef, issueNumber int) string {
	return fmt.Sprintf("%s/%s/%d", taskRepo.Owner, taskRepo.Name, issueNumber)
}

// externalRef is the ExternalRef a polled task is stamped with — "where
// this task appears for humans," per Task's own doc comment — formatted
// so parseExternalRef can read the originating task-repo issue back off
// of a Task with nothing else in hand.
func externalRef(taskRepo model.RepoRef, issueNumber int) string {
	return fmt.Sprintf("%s#%d", taskRepo, issueNumber)
}

// parseExternalRef reverses externalRef.
func parseExternalRef(s string) (repo model.RepoRef, number int, err error) {
	repoPart, numPart, ok := strings.Cut(s, "#")
	if !ok {
		return model.RepoRef{}, 0, fmt.Errorf("external ref must be owner/name#number, got %q", s)
	}
	repo, err = model.ParseRepo(repoPart)
	if err != nil {
		return model.RepoRef{}, 0, fmt.Errorf("external ref %q: %w", s, err)
	}
	number, err = strconv.Atoi(numPart)
	if err != nil {
		return model.RepoRef{}, 0, fmt.Errorf("external ref %q: bad issue number: %w", s, err)
	}
	return repo, number, nil
}

// HostSandboxes hands out one directory per slot, on the host this
// process itself runs on — see the package doc comment on why that is the
// whole sandbox for now. Directories persist across cycles for the same
// slot, matching v1's own long-lived-sandbox shape (docs/next-session.md:
// "sequential tasks share a long-lived sandbox"); nothing here resets one
// between tasks, which is the caller's job the same way v1's
// ensure_workspace is.
type HostSandboxes struct {
	baseDir string

	mu    sync.Mutex
	roots map[string]string
}

// NewHostSandboxes returns a HostSandboxes rooted at baseDir, which must
// already exist.
func NewHostSandboxes(baseDir string) *HostSandboxes {
	return &HostSandboxes{baseDir: baseDir, roots: map[string]string{}}
}

// RootFor returns slot's sandbox directory, creating it on first use.
func (h *HostSandboxes) RootFor(slot string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if root, ok := h.roots[slot]; ok {
		return root, nil
	}
	root := filepath.Join(h.baseDir, slot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("orchestrator: creating sandbox directory for slot %q: %w", slot, err)
	}
	h.roots[slot] = root
	return root, nil
}
