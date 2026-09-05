// Package orchestrator is v2's equivalent of v1's core.py Orchestrator:
// the component that decides *when* to call GitHub, wired to dispatch.Cycle's
// dispatch decisions and model.Store's state. README.md's "What this
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
// it — see README.md's "What this does not have yet" for what that
// merge kept and dropped).
//
// **Sandboxing defaults to "execute on the host," deliberately, for now,
// with a real host adapter available as an opt in.** Deps.Sandboxes is the
// seam: HostSandboxes reuses exactly the stand-in e2e already
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
// stands in for; provisioning it is still open (README.md).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
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
// SandboxCPUs/SandboxMemoryMB/SandboxDiskGB (bwsalmon/agents#534,
// grain/task-41), or the zero value for a run content with the
// deployment default (and, where that names nothing either, with
// grain's own DefaultShape: a dimension no one asked for still reaches
// kontur as a number).
//
// It is passed to Acquire rather than applied afterwards because a
// sandbox is now built per run: the one moment its size is decided is
// when it is created, so there is nothing to resize. KonturSandboxes
// used to expose a Reshape for exactly that gap -- a `konturctl vm
// update` against a long-lived slot VM already sized from the deployment
// default at create time, undone by the next recreate -- and it is gone
// with the gap. Disk is the one dimension that could never have been
// applied afterwards even then: a VM's root disk is a qcow2 overlay
// created with the VM, and growing one under a running guest is not
// something `konturctl vm update` offers.
type Shape struct {
	CPUs, MemoryMB int
	// DiskGB is the VM's root disk size, in GiB -- the third dimension,
	// added by grain/task-41 alongside the other two rather than as a
	// type of its own so every path that already carries a requested
	// size carries this one too.
	DiskGB int
}

// IsZero reports whether a shape asks for nothing in particular, which is
// what a task with no override at all produces.
func (s Shape) IsZero() bool { return s.CPUs == 0 && s.MemoryMB == 0 && s.DiskGB == 0 }

// orDefault fills each dimension this shape leaves at zero from def --
// how a run that asked for no size of its own (or asked in only one
// dimension) lands on the deployment-wide default its sandbox backend is
// currently carrying (KonturSandboxes.SetDefaultShape). Per dimension,
// not all-or-nothing: a task that names only CPUs still gets the
// deployment's memory default.
func (s Shape) orDefault(def Shape) Shape {
	if s.CPUs == 0 {
		s.CPUs = def.CPUs
	}
	if s.MemoryMB == 0 {
		s.MemoryMB = def.MemoryMB
	}
	if s.DiskGB == 0 {
		s.DiskGB = def.DiskGB
	}
	return s
}

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
	// agent framework's own default in place, which for both frameworks
	// is no cap at all (agent/claude's defaultMaxTurns has why). What
	// actually bounds a runaway run is MaxRunRuntime below.
	//
	// Whatever a caller sets here is only the starting value on a
	// deployment with a store: RunCycle re-reads model.Config.
	// MaxAgentTurns out of grain_config every cycle, alongside
	// Deps.MaxWorkers/MaxMergers, so a change made in Settings reaches the next
	// run dispatched rather than the next restart.
	MaxAgentTurns int
	// PromptExtension is this deployment's own standing instructions for
	// every run it dispatches -- model.Config.PromptExtension, and
	// model/prompt_extension.go for what the three layers of it are.
	// Empty adds nothing to any prompt, which is what a caller that has
	// never set one gets.
	//
	// Only the deployment-wide layer lives here. The repo's own is read
	// per dispatch, from the task's target (RunDispatch), because it is
	// keyed by something only a task names; the task's own is on the task
	// already. Whatever a caller sets here is only the starting value on
	// a deployment with a store, exactly like MaxAgentTurns above:
	// RunCycle re-reads it out of grain_config every cycle, so an edit in
	// Settings reaches the next run dispatched rather than the next
	// restart.
	PromptExtension string
	// TimeZone is the IANA zone this deployment keeps its wall clock in
	// -- model.Config.TimeZone, and timezone.go for what an empty one
	// means. The one thing this package reads it for is when a wall-clock
	// schedule comes due (reconcileSchedule, over Recurrence.Next): a
	// daily, weekly or monthly cadence names a time on somebody's
	// calendar, and before this it named a time on the container's
	// accidental UTC one.
	//
	// Refreshed from grain_config every cycle, like MaxAgentTurns and
	// PromptExtension above, so moving a deployment's zone retimes the
	// next occurrence of every schedule rather than waiting for a
	// restart.
	TimeZone string
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
	// Zero uses DefaultMaxRunRuntime. There is no "uncapped" value the
	// way MaxAgentTurns' own zero means "no turn cap": an uncapped run
	// is exactly the gap this field exists to close -- all the more so
	// now that it is the only ceiling a default deployment has -- so a
	// deployment that really wants a longer one sets it explicitly
	// instead of switching this off.
	MaxRunRuntime time.Duration

	// Now reads the current time, and exists for the timestamps
	// RunDispatch cannot take from its caller: when a run's agent got its
	// first turn, and when the run finished. Every other moment this
	// package records is the `at` RunCycle hands down -- the moment the
	// cycle began -- and those two are genuinely later than that, by
	// however long setup and then the agent took. Stamping finished_at
	// with `at` too made every run ever recorded read back as zero
	// seconds long; the agent's own start is the line pkg/metrics splits
	// that duration at (Store.SetRunAgentStarted).
	//
	// nil is the wall clock, which is what a real deployment wants. A
	// test driving this package off a fake clock sets it so a run's
	// finish lands on the same timeline as the `at` it dispatched with;
	// dispatch's own retry backoff compares the two.
	Now func() time.Time
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
	//
	// self-debug and bootstrap-playbooks are the capabilities that no
	// longer depend on this, and it is worth reading why: their tools are
	// read-only, so they need no route back to a live run's own
	// conversation, and a forked "grain mcpserver" can therefore build
	// them for itself out of an argument (agent.GrantArgs) plus, for
	// self-debug's task half, a REST client of the daemon. What travels
	// is the name of the grant, not the tools --
	// agent.RunConfig.Grants, set by RunDispatch. selfrepair cannot
	// follow them: its one tool blocks on a human reply in the task's own
	// chat, which is a store handle that process deliberately lacks.
	GrantTools map[string]func(store *model.Store, taskID string) []mcp.Tool
	// GrainSourceDir is the checkout of grain's own source a self-debug
	// run may read -- cmd/grain's own sourceDir (the copy baked into the
	// deployment image, or -upgrade-src-dir's checkout), passed on as
	// agent.RunConfig.GrainSourceDir for the tasks that hold the grant
	// and to nobody else.
	//
	// "" is a deployment with no such checkout: the run still gets the
	// tools, and they answer that there is no source here to read rather
	// than vanishing from its roster (selfdebug.SourceTools).
	GrainSourceDir string
	// SandboxRecreations, when non-nil, is the registry each dispatched
	// run parks itself in so that it can later ask for its own sandbox to
	// be destroyed and rebuilt -- the daemon-side half of pkg/mcp's
	// recreate_sandbox tool. See SandboxRecreations' own doc comment for
	// why the request has to come back to the daemon at all rather than
	// being something the run does inside its sandbox.
	//
	// nil registers nothing and answers every request with "no live run",
	// which is the honest answer for a caller (a test, a one-shot cycle,
	// `grain demo`) that has no UI/API route for such a request to arrive
	// over in the first place.
	SandboxRecreations *SandboxRecreations
	// Pause, when non-nil, is the deployment-wide gate a run that meets
	// its agent's own usage limit closes: every run in flight is
	// cancelled and nothing else is dispatched until the provider's
	// window resets. See Pause for why that is the right response to a
	// limit rather than letting each task discover it in turn.
	//
	// Both halves of it are read from here -- RunDispatch registers each
	// run's own cancellation with it and reports a limit to it,
	// reconcileDispatch asks it whether to dispatch at all -- because
	// Config is what both already have in hand, the same way
	// SandboxRecreations above is reached from two places.
	//
	// nil means a usage limit is reported on the run that met it (its
	// outcome is model.PausedOutcome either way) and pauses nothing,
	// which is what a caller with no loop to pause wants: a test, a
	// one-shot cycle. A deployment always sets one -- without it the
	// next tick dispatches the next task straight into the same refusal.
	Pause *Pause
	// StateRepo answers which repository this deployment keeps its own
	// settings in -- the state repository grain loads its configuration
	// out of and exports its database back into (pkg/staterepo) -- so
	// that every run dispatched can be told, in as many words, whether
	// the checkout it has been handed is that repository or not. See
	// settingsRepoSection in run.go for what is said and why nothing in
	// a sandbox could work it out instead.
	//
	// A function rather than a value because the answer changes under a
	// running daemon: adopting a different repository, from the Settings
	// pane's State tab or from `grain state adopt`, swaps it out
	// mid-process (cmd/grain's stateManager), and a value snapshotted
	// when the daemon started would have every run dispatched afterwards
	// naming the repository this deployment used to run on. That is a
	// worse failure than naming none, since a run has no way to check it.
	//
	// nil, or a function returning the zero RepoRef, says nothing to any
	// run. Both are honest answers rather than a gap: a deployment whose
	// state is local-only has no repository to name -- its settings are
	// not something a pull request can reach at all -- and a caller with
	// no state repository of its own (`grain demo`, a test, anything
	// embedding this package) has nothing to say either.
	StateRepo func() model.RepoRef
}

func (c Config) cancelPollInterval() time.Duration {
	if c.CancelPollInterval > 0 {
		return c.CancelPollInterval
	}
	return 2 * time.Second
}

// DefaultMaxRunRuntime is Config.maxRunRuntime's fallback -- the same
// 120-minute value v1's own AutomationConfig.max_runtime_minutes
// defaulted to, so a deployment moving from v1 gets back the ceiling it
// already had rather than a new, differently-tuned one.
//
// Exported because a run is now told this number (BuildPrompt's
// wall-clock paragraph), which makes it a fact about grain rather than a
// private tuning knob: anything that has to state the budget without a
// live Config in hand -- `grain demo`'s seeded prompt, a test -- names
// this rather than writing "two hours" out again somewhere it can go
// stale.
const DefaultMaxRunRuntime = 120 * time.Minute

func (c Config) maxRunRuntime() time.Duration {
	if c.MaxRunRuntime > 0 {
		return c.MaxRunRuntime
	}
	return DefaultMaxRunRuntime
}

// settingsRepo reads Config.StateRepo, or the zero RepoRef when a caller
// wired none -- and also when the function it wired answers with one, so
// that "no state repository" and "nobody asked" reach the prompt as the
// same silence (settingsRepoSection).
func (c Config) settingsRepo() model.RepoRef {
	if c.StateRepo == nil {
		return model.RepoRef{}
	}
	return c.StateRepo()
}

// now reads Config.Now, or the wall clock when a caller set none.
func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
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
// no CPU, memory or disk of its own to size -- it is a path on the host's
// own filesystem -- so a task that asked for a specific one would
// silently get the host's instead: the same refusal this backend gave
// before, when a shape override went looking for a Reshape it does not
// implement.
func (h *HostSandboxes) Acquire(ctx context.Context, name string, shape Shape) (Sandbox, error) {
	if !shape.IsZero() {
		return nil, fmt.Errorf("orchestrator: sandbox %q asks for %d vCPU/%d MiB/%d GiB but a host-directory sandbox has no shape of its own", name, shape.CPUs, shape.MemoryMB, shape.DiskGB)
	}
	root := filepath.Join(h.baseDir, name)
	// A directory left behind by a previous process using this same
	// -sandbox-dir would otherwise be inherited wholesale, which is the
	// one thing a sandbox per run exists to rule out.
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("orchestrator: clearing sandbox directory %q: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		// A full filesystem is the one failure here that says nothing
		// about this run and everything about the deployment: every
		// task fails identically at setup until somebody reclaims
		// space, so the error a task's own timeline shows is the only
		// place an operator is likely to read about it. Say where the
		// space went and what recovers it, rather than leaving them
		// "no space left on device" against a path.
		if errors.Is(err, syscall.ENOSPC) {
			return nil, fmt.Errorf("orchestrator: creating sandbox directory %q: the filesystem holding %q is full "+
				"(it holds one checkout per run); restarting the daemon reaps the directories runs killed mid-flight "+
				"left behind (HostSandboxes.ReapOrphans), and a disk that is full of live runs needs to grow: %w",
				root, h.baseDir, err)
		}
		return nil, fmt.Errorf("orchestrator: creating sandbox directory %q: %w", root, err)
	}
	sb := &hostSandbox{owner: h, name: name, root: root}
	h.mu.Lock()
	h.live[name] = sb
	h.mu.Unlock()
	return sb, nil
}

// ReapOrphans removes every directory under baseDir that this process is
// not itself holding, and reports how many it removed.
//
// The host-directory half of KonturSandboxes.ReapOrphans, and here for
// the same reason: a directory is created for a run and removed with it
// (Acquire and Release above), so one still on disk while nothing holds
// it belongs to a process that died before its Release could run. Being
// killed mid-run is not exotic for this daemon --
// grain-daemon.service stops its container with `docker stop --time 30`
// while a run's own unwinding is allowed minutes (cmd/grain's
// shutdownDrain), so an upgrade or a restart that lands on a run in
// flight leaves that run behind by design.
//
// What it leaves behind is a whole checkout of the task's repository,
// plus whatever the agent downloaded into it, and until now nothing ever
// removed one: a deployment that restarts often enough accumulates one
// per killed run until the filesystem holding baseDir is full -- at
// which point every later Acquire fails at mkdir with ENOSPC and no task
// can run at all. Reaping at startup is what makes that a restart rather
// than a permanently wedged deployment.
//
// Everything under baseDir is fair game because baseDir is this type's
// alone -- -sandbox-dir's own flag doc calls it "the root directory
// HostSandboxes creates one working directory per run under" -- and
// because the live check above is what keeps a running deployment's own
// sandboxes out of it, should a caller ever run this off startup.
//
// A removal that fails is joined onto the others rather than ending the
// sweep: the directory that cannot be removed is exactly the one worth
// naming, and it is no reason to leave the rest of the disk spent.
func (h *HostSandboxes) ReapOrphans(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(h.baseDir)
	if err != nil {
		// Nothing to reap from a directory that does not exist yet --
		// the ordinary state of a deployment's first start, and not
		// something to report as a failure of the sweep.
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("orchestrator: listing sandbox directories to reap under %q: %w", h.baseDir, err)
	}
	h.mu.Lock()
	live := make(map[string]bool, len(h.live))
	for name := range h.live {
		live[name] = true
	}
	h.mu.Unlock()

	var reaped int
	var errs []error
	for _, entry := range entries {
		if live[entry.Name()] {
			continue
		}
		path := filepath.Join(h.baseDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("removing orphaned sandbox directory %q: %w", path, err))
			continue
		}
		reaped++
	}
	return reaped, errors.Join(errs...)
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

// Rebuild implements SandboxRebuilder: remove this run's directory and
// make an empty one again at the same path -- Acquire's own
// RemoveAll/MkdirAll pair, which is already exactly "destroy whatever is
// under this name and start clean".
//
// It is worth having here even though a host directory cannot break the
// way a VM can (there is no guest to stop answering, no disk of its own
// to fill). What it can be is *wrong* -- a build left half-finished, a
// dependency tree in a state the agent cannot reason about -- and
// starting over is the same answer to that whichever backend a
// deployment runs. A run that reaches for this on one backend and is
// refused on the other would be the more surprising outcome.
func (s *hostSandbox) Rebuild(ctx context.Context) error {
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("orchestrator: removing sandbox directory %q to rebuild it: %w", s.root, err)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("orchestrator: recreating sandbox directory %q: %w", s.root, err)
	}
	return nil
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
