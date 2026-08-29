// Package orchestrator is v2's equivalent of v1's core.py Orchestrator:
// the component that decides *when* to call GitHub, wired to dispatch.Cycle's
// dispatch decisions and model.Store's state. v2/README.md's "What this
// does not have yet" section named this gap after bwsalmon/agents#243
// ported github.Client and github/githubsim but wired neither into
// anything that runs; this package is that wiring.
//
// It is a library, not a binary — RunCycle is what a deployment's own
// cron/timer loop calls once per tick, the same shape v1's `automation
// run-once` command wraps around core.py's Orchestrator.run_once.
// cmd/graind is that timer loop, calling RunCycle against a real embedded
// Dolt store, a real github.RESTClient, and a real agent/gemini.Framework
// (bwsalmon/agents#263, reconciling this package with the parallel
// pkg/orchestrate/cmd/graind bwsalmon/agents#254 built independently of
// it — see v2/README.md's "What this does not have yet" for what that
// merge kept and dropped).
//
// **Sandboxing defaults to "execute on the host," deliberately, for now,
// with a real host adapter available as an opt in.** Deps.Sandboxes is the
// seam: HostSandboxes reuses exactly the stand-in v2/e2e already
// validated, a plain directory on the machine this process runs on, which
// pkg/mcp's sandbox tools confine a run to (NewSandboxTools' own doc
// comment: "root stands in for the sandbox"). It carries no isolation at
// all — an agent given a directory here can do anything this process's own
// user can do. KonturSandboxes is the real alternative: one
// bwsalmon/kontur-managed VM per dispatch slot, reached over SSH the way
// v1 runs the agent process itself against a real, separate sandbox VM.
// Neither this package nor pkg/kontur builds that VM's guest image —
// KonturSandboxes assumes one already exists that carries the operator's
// SSH key and runs sshd, the same assumption v1's own sandbox provisioning
// stands in for; provisioning it is still open (v2/README.md).
package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Sandboxes hands out the MCP tools one dispatch slot's agent run should
// have its tool calls confined to. HostSandboxes and KonturSandboxes both
// implement it; RunCycle only ever calls ToolsFor, never anything backend-
// specific, so swapping Deps.Sandboxes between them is the whole change a
// deployment needs to make to move a slot from the local stand-in to a
// real VM.
type Sandboxes interface {
	ToolsFor(ctx context.Context, slot string) ([]mcp.Tool, error)
}

// Config is what one deployment's orchestrator needs to know: which repo
// is the task queue, which label marks an issue as ready to dispatch, and
// what a dispatched run is allowed to do on its own.
type Config struct {
	// TaskRepo, TriggerLabel and DefaultTarget are gone with the poll that
	// needed them: nothing here reads GitHub issues to find tasks any
	// more, so there is no task repo to list, no label to look for, and no
	// default target to apply to an issue body that named none. A task
	// arrives with its Target already set, because whatever wrote it set
	// one (ui.Config.DefaultTarget is where that fallback lives now).

	// Capabilities is the registry RunDispatch resolves and materializes
	// each dispatched task's Grants against before running its agent, and
	// revokes once the run finishes -- ported from pkg/orchestrate's own
	// Config.Capabilities (bwsalmon/agents#254) when that package merged
	// into this one. Nil, or a task with no Grants, skips capability
	// handling entirely rather than erroring, so a deployment or test
	// that grants no capabilities needs to configure none of this.
	Capabilities *model.CapabilityRegistry
	// Credentials resolves the named credentials a capability provider
	// asks for by name, e.g. gcpkey's minter service account.
	Credentials model.CredentialResolver
	// MaxAgentTurns caps model/tool round trips per run; 0 leaves the
	// agent framework's own default in place.
	MaxAgentTurns int
	// CancelPollInterval is how often RunDispatch re-reads its task's
	// state from store while a run is live, to notice the task being
	// closed out from under it -- bwsalmon/agents#346's own store-polled
	// cancellation signal, needed because grain daemon (running the run)
	// and grain ui (where a close actually lands) are separate processes
	// sharing only the store. 2 seconds if zero; a test that wants to
	// prove cancellation happens promptly, without waiting seconds for
	// real, sets this to something much smaller.
	CancelPollInterval time.Duration
}

func (c Config) cancelPollInterval() time.Duration {
	if c.CancelPollInterval > 0 {
		return c.CancelPollInterval
	}
	return 2 * time.Second
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

// ToolsFor implements Sandboxes: mcp.NewSandboxTools confined to
// RootFor(slot).
func (h *HostSandboxes) ToolsFor(ctx context.Context, slot string) ([]mcp.Tool, error) {
	root, err := h.RootFor(slot)
	if err != nil {
		return nil, err
	}
	return mcp.NewSandboxTools(root), nil
}
