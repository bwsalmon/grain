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
// SQLite store, a real github.RESTClient, and a real agent.Framework
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
// bwsalmon/kontur-managed VM per run, reached over SSH the way
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

// Sandboxes builds the sandbox one run's agent is confined to, and
// destroys it when that run is done. HostSandboxes and KonturSandboxes
// both implement it; RunCycle only ever calls Acquire and then the
// returned Sandbox's own methods, never anything backend-specific, so
// swapping Deps.Sandboxes between them is the whole change a deployment
// needs to make to move from the local stand-in to a real VM.
//
// It used to hand out tools for a named slot, and a slot's sandbox
// outlived every run dispatched onto it -- which is why isolating one
// task from the next had to be bolted on afterwards, as a recreate
// between runs plus a reset pass at startup to cover the runs a crash
// interrupted. A sandbox that is created for a run and destroyed with it
// gets that for free: there is no previous task's filesystem to inherit
// because there is no previous task.
type Sandboxes interface {
	// Acquire builds a sandbox called name, sized to shape, ready for use
	// by the time it returns. A caller that gets a nil error owns the
	// returned Sandbox and must Release it.
	Acquire(ctx context.Context, name string, shape Shape) (Sandbox, error)
}

// Shape is how big a sandbox a run asked for -- model.Task's own
// SandboxCPUs/SandboxMemoryMB (bwsalmon/agents#534), or the zero value
// for a run content with the deployment default.
//
// It is passed to Acquire rather than applied afterwards because a
// sandbox is now built per run: the one moment its size is decided is
// when it is created, so there is nothing to resize. KonturSandboxes
// used to expose a Reshape for exactly that gap -- a `konturctl vm
// update` against a long-lived slot VM already sized from the deployment
// default at create time, undone by the next recreate -- and it is gone
// with the gap.
type Shape struct {
	CPUs, MemoryMB int
}

// IsZero reports whether a shape asks for nothing in particular, which is
// what a task with neither override set produces.
func (s Shape) IsZero() bool { return s.CPUs == 0 && s.MemoryMB == 0 }

// Sandbox is one run's own sandbox: a local directory, or a
// bwsalmon/kontur-managed VM. It lives exactly as long as the run does.
type Sandbox interface {
	// Name is what this sandbox is called -- the string recorded as the
	// run's model.Run.Sandbox, and the identity the git proxy resolves
	// back to that run's task.
	Name() string
	// Tools are the MCP tools the run's agent has its tool calls confined
	// to.
	Tools(ctx context.Context) ([]mcp.Tool, error)
	// ConfigureGitCredentials points this sandbox's git at the proxy,
	// using the bearer token minted for it.
	ConfigureGitCredentials(ctx context.Context, remoteURL, token string) error
	// Release destroys the sandbox. It is called once the run is done,
	// success or failure alike, and is what makes the isolation between
	// one task and the next a property of the lifecycle rather than
	// something a caller has to remember to do.
	Release(ctx context.Context) error
}

// rootedSandbox is implemented by a Sandbox with a local directory on the
// host this process runs on -- HostSandboxes' own. It is what a task with
// capabilities to place needs (orchestrator.placeAttachments writes into
// it directly), and what a kontur VM deliberately does not have: its
// filesystem is inside a guest this process can only reach by exec'ing
// into it.
type rootedSandbox interface {
	Root() (string, error)
}

// SandboxPlacer is implemented by a Sandbox that can write a file into
// its own filesystem over whatever transport it already has --
// konturSandbox's own, over the same runner every tool call and
// ConfigureGitCredentials use. It is rootedSandbox's counterpart for
// capability placements (orchestrator.applyPlacements): a rooted sandbox
// is written into with plain os.WriteFile under its directory, and a
// sandbox that has no directory on this host needs a route of its own or
// its placements have nowhere to land at all (bwsalmon/agents#643 left
// exactly that gap: every deployment running -kontur-sandboxes failed
// any task granting gcp-key/gemini-key/github-sandbox during
// preparation, before the agent's first turn).
//
// Exported, unlike rootedSandbox and vmNamedSandbox, because RunDispatch
// takes one: a Sandboxes implementation outside this package can supply
// the route, and a Sandbox that wraps another (e2e's own) must forward
// this method explicitly for it to be seen -- an embedded interface value
// carries no methods outside its own method set.
type SandboxPlacer interface {
	// PlaceFile writes content to path -- always the absolute path the
	// placement means inside the sandbox -- with mode, an octal string
	// (model.Placement.EffectiveMode's own shape). It creates path's
	// parent directory, and must not widen mode at any point, since
	// everything placed this way is credential material.
	PlaceFile(ctx context.Context, path, content, mode string) error
}

// vmNamedSandbox is implemented by a Sandbox reachable only as a named
// bwsalmon/kontur-managed VM rather than a local directory --
// konturSandbox's own, and rootedSandbox's counterpart. It is what
// agent.RunConfig.KonturVM needs: a Framework with no in-process route to
// a sandbox (agent/claude, which forks a real MCP client as a subprocess
// rather than looping tool calls itself) points its own forked
// "mcpserver -kontur-vm" at this name instead of a local -sandbox-root.
type vmNamedSandbox interface {
	VMName() string
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
	// GitRemoteBase is the base URL of this deployment's git proxy
	// (cmd/grain/daemon.go's startGitProxy), which RunDispatch turns into
	// a task's own clone URL to prepare its sandbox's checkout with --
	// see prepareCheckout. Empty skips that preparation entirely and
	// leaves the sandbox as it always was, an empty directory the agent
	// has to populate itself: the default for a test, and for a
	// deployment running no proxy.
	GitRemoteBase string
	// CancelPollInterval is how often RunDispatch re-reads its task's
	// state from store while a run is live, to notice the task being
	// closed out from under it -- bwsalmon/agents#346's own store-polled
	// cancellation signal, needed because grain daemon (running the run)
	// and grain ui (where a close actually lands) are separate processes
	// sharing only the store. 2 seconds if zero; a test that wants to
	// prove cancellation happens promptly, without waiting seconds for
	// real, sets this to something much smaller.
	CancelPollInterval time.Duration
	// MaxRunRuntime bounds how long RunDispatch lets one run's
	// framework.Run call stay live before cancelling it outright --
	// v2's own equivalent of v1's AutomationConfig.max_runtime_minutes
	// plus its sweeper (bwsalmon/agents#575), both aimed at the same
	// failure: a run that is alive but stuck making no progress, tying
	// up its share of the concurrency limit indefinitely with nothing to notice or
	// recover it. A tool call with no bound of its own is exactly how
	// that happens in practice (a run_command whose own caller omitted
	// "timeout" -- see mcp.defaultRunCommandTimeout, the fix for the
	// same issue's other half), but this is the backstop for any way a
	// framework can end up wedged, not only that one.
	//
	// v1 needed a whole separate sweeper process for this because its
	// controller launches each run as an external unit, polled between
	// cron invocations, with no long-lived supervisor of its own to hand
	// a deadline to. RunDispatch already is that supervisor -- one
	// goroutine, alive for exactly as long as the run is -- so a
	// deadline on the very ctx framework.Run receives is the whole
	// mechanism; see RunDispatch's own use of context.WithTimeoutCause.
	// It reaches an already-live tool call the same way task-closed
	// cancellation does (bwsalmon/agents#346's "actually terminate a
	// running task's sandbox process on cancel"), through
	// exec.CommandContext/procgroup.
	//
	// Zero uses defaultMaxRunRuntime. There is no "uncapped" value the
	// way MaxAgentTurns' own zero means "the framework's own default":
	// an uncapped run is exactly the gap this field exists to close, so
	// a deployment that really wants a longer ceiling sets one
	// explicitly instead of switching this off.
	MaxRunRuntime time.Duration
	// TranscriptDir, if set, is a directory RunDispatch asks each run's
	// agent.Framework to mirror its own transcript-in-progress into, one
	// file per run named after d.RunID -- agent.RunConfig.TranscriptPath's
	// own doc comment says what a Framework does with it. A deployment
	// wiring this up also wires the very same directory into
	// ui.Config.LiveTranscripts (e.g. claude.LiveTranscriptDir) so the two
	// sides agree on where one run's file is; RunDispatch itself never
	// reads it back, only writes to and then removes it. "" means no
	// caller wants this, and RunDispatch leaves agent.RunConfig's own
	// TranscriptPath empty (bwsalmon/agents#467).
	TranscriptDir string
	// GrantTools maps a capability name to a function building the extra
	// MCP tools a task holding that grant gets added to its own
	// dispatched run -- runOne's own hook for a capability whose effect
	// is which tools a run has, rather than only what Materialize's own
	// Placements put in the sandbox or what PromptSection adds to the
	// prompt (model.CapabilityProvider's four-moment contract has no
	// room for either). selfdebug's read-only source tools and
	// selfrepair's confirmation-gated host command tool
	// (bwsalmon/agents#540) are the two capabilities that need this
	// today.
	//
	// Kept as a caller-supplied map rather than a fifth method on
	// CapabilityProvider deliberately: model/capability.go's own doc
	// comment already explains why a provider is handed no Runner, and a
	// method returning live mcp.Tools would be exactly the kind of
	// convention that comment says a structural contract should avoid.
	// A deployment that wants a grant to add tools wires it here
	// instead, entirely outside that package.
	//
	// runOne only ever consults this for an Interactive task
	// (model.Task.Interactive) -- every capability it is meant for today
	// assumes a human is actually watching the task's own chat closely
	// enough to answer a tool's own confirmation prompt (selfrepair.
	// Confirm) within its timeout, which is not a safe assumption for an
	// unattended dispatch. nil, or a capability with no entry, adds no
	// tools.
	//
	// These tools reach no agent at present. They travel as
	// agent.RunConfig.Tools, whose only consumer was the in-process
	// Gemini runtime agent/antigravity replaced; every Framework left
	// forks a CLI that manages its own MCP connection and cannot be
	// handed an in-process registry. See selfrepair.Confirm's own doc
	// comment for what closing that gap would take.
	GrantTools map[string]func(store *model.Store, taskID string) []mcp.Tool
}

func (c Config) cancelPollInterval() time.Duration {
	if c.CancelPollInterval > 0 {
		return c.CancelPollInterval
	}
	return 2 * time.Second
}

// defaultMaxRunRuntime is Config.maxRunRuntime's fallback -- the same
// 120-minute value v1's own AutomationConfig.max_runtime_minutes
// defaulted to, so a deployment moving from v1 gets back the ceiling it
// already had rather than a new, differently-tuned one.
const defaultMaxRunRuntime = 120 * time.Minute

func (c Config) maxRunRuntime() time.Duration {
	if c.MaxRunRuntime > 0 {
		return c.MaxRunRuntime
	}
	return defaultMaxRunRuntime
}

// HostSandboxes hands out one directory per run, on the host this process
// itself runs on — see the package doc comment on why that is the whole
// sandbox for now. A directory is created by Acquire and removed by
// Release, so nothing of one task's run survives into the next.
//
// That is a change of substance for this backend, not just of shape. A
// HostSandboxes directory used to be per *slot* and deliberately
// long-lived, resetting one between tasks being "the caller's job" -- and
// since no caller did, sequential tasks on one slot genuinely shared a
// filesystem. They no longer can.
type HostSandboxes struct {
	baseDir string

	mu   sync.Mutex
	live map[string]*hostSandbox
}

// NewHostSandboxes returns a HostSandboxes rooted at baseDir, which must
// already exist.
func NewHostSandboxes(baseDir string) *HostSandboxes {
	return &HostSandboxes{baseDir: baseDir, live: map[string]*hostSandbox{}}
}

// Acquire implements Sandboxes: a fresh directory under baseDir, named
// for the run.
//
// A non-zero shape is refused rather than ignored. A local directory has
// no CPU or memory of its own to size, so a task that asked for a
// specific one would silently get the host's instead -- the same refusal
// this backend gave before, when a shape override went looking for a
// Reshape it does not implement.
func (h *HostSandboxes) Acquire(ctx context.Context, name string, shape Shape) (Sandbox, error) {
	if !shape.IsZero() {
		return nil, fmt.Errorf("orchestrator: sandbox %q asks for %d vCPU/%d MiB but a host-directory sandbox has no shape of its own", name, shape.CPUs, shape.MemoryMB)
	}
	root := filepath.Join(h.baseDir, name)
	// A directory left behind by a previous process using this same
	// -sandbox-dir would otherwise be inherited wholesale, which is the
	// one thing a sandbox per run exists to rule out.
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("orchestrator: clearing sandbox directory %q: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: creating sandbox directory %q: %w", root, err)
	}
	sb := &hostSandbox{owner: h, name: name, root: root}
	h.mu.Lock()
	h.live[name] = sb
	h.mu.Unlock()
	return sb, nil
}

// hostSandbox is one run's directory.
type hostSandbox struct {
	owner *HostSandboxes
	name  string
	root  string
}

func (s *hostSandbox) Name() string { return s.name }

// Root implements rootedSandbox: the directory itself, which is what a
// task with capabilities to place needs.
func (s *hostSandbox) Root() (string, error) { return s.root, nil }

// Tools implements Sandbox: mcp.NewSandboxTools confined to this run's
// own directory.
func (s *hostSandbox) Tools(ctx context.Context) ([]mcp.Tool, error) {
	return mcp.NewSandboxTools(s.root), nil
}

// ConfigureGitCredentials points this run's git at the proxy -- an
// ordinary file write under its directory, where KonturSandboxes' own
// method of the same name has to reach into a VM's guest to do it.
func (s *hostSandbox) ConfigureGitCredentials(ctx context.Context, remoteURL, token string) error {
	return mcp.ConfigureGitCredentials(s.root, remoteURL, token)
}

// Release removes the directory. Unlike a kontur VM's own Release there
// is no isolation boundary to enforce here -- a host directory never had
// one from the host daemon it sits beside -- but leaving one behind would
// accumulate a checkout per run for the life of the deployment.
func (s *hostSandbox) Release(ctx context.Context) error {
	s.owner.mu.Lock()
	delete(s.owner.live, s.name)
	s.owner.mu.Unlock()
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("orchestrator: removing sandbox directory %q: %w", s.root, err)
	}
	return nil
}
