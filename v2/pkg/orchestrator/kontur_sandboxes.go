package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

// KonturConfig is what KonturSandboxes needs to create and reach one
// bwsalmon/kontur-managed VM per dispatch slot. StateDir and
// RuntimeEndpoint default to kontur.DefaultStateDir and
// kontur.DefaultRuntimeEndpoint (the same defaults cmd/mcpserver's own
// -kontur-state-dir/-cri-runtime-endpoint flags use) when left zero.
type KonturConfig struct {
	// NamePrefix names each slot's VM as NamePrefix+slot, so a fixed set
	// of slots (Deps.Slots) maps onto a fixed, predictable set of VM
	// names -- the same relationship HostSandboxes has between a slot and
	// its directory.
	NamePrefix string
	// StateDir is kontur's VM state directory (kontur.DefaultStateDir if
	// empty), used to look up a slot's VM's external port.
	StateDir string
	// RuntimeEndpoint is the containerd CRI socket (kontur.
	// DefaultRuntimeEndpoint if empty), used to resolve a slot's VM's pod
	// IP via crictl. Ignored when Backend is kontur.BackendDocker, which
	// has no CRI to ask.
	RuntimeEndpoint string
	// Backend selects the value `kontur vm create -backend` builds each
	// slot's VM with, and which of pkg/kontur's two ways of finding a
	// VM's reachable address resolveEndpoint uses to match. Empty means
	// kontur's own default, the static-pod backend under a standalone
	// kubelet, resolved via kontur.PodIP (crictl). kontur.BackendDocker
	// runs the VM directly against a local docker daemon instead --
	// bwsalmon/agents#353's ask, since it needs neither `konturctl setup`
	// nor containerd/CNI/kubelet on the host -- and is resolved via
	// kontur.DockerPodIP (docker inspect) instead.
	Backend string
	// CreateArgs is appended to "kontur vm create <name> -state-dir
	// <dir>" verbatim when a slot's VM does not exist yet -- guest image,
	// guest SSH port, resource sizing, and anything else a deployment's
	// own "kontur vm create" needs beyond a name, since this package has
	// no way to know those on its own (see package kontur's doc comment).
	CreateArgs []string
	// SSHUser is the username KonturSandboxes authenticates to each VM
	// as.
	SSHUser string
	// SSHKey is the path to the private key KonturSandboxes authenticates
	// to each VM with.
	SSHKey string
	// Workspace is the working directory run_command/read_file/edit_file/
	// write_file operate in on each VM.
	Workspace string
	// ReadyTimeout bounds how long ToolsFor waits for a freshly created
	// VM's pod to reach Ready and get a network IP before giving up
	// (2 minutes if zero). It has no effect on a slot whose VM already
	// existed when ToolsFor was first called for it.
	ReadyTimeout time.Duration
	// ReadyPollInterval is how often ToolsFor retries PodIP while waiting
	// out ReadyTimeout (2 seconds if zero).
	ReadyPollInterval time.Duration
}

func (c KonturConfig) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return kontur.DefaultStateDir
}

func (c KonturConfig) runtimeEndpoint() string {
	if c.RuntimeEndpoint != "" {
		return c.RuntimeEndpoint
	}
	return kontur.DefaultRuntimeEndpoint
}

func (c KonturConfig) readyTimeout() time.Duration {
	if c.ReadyTimeout > 0 {
		return c.ReadyTimeout
	}
	return 2 * time.Minute
}

func (c KonturConfig) readyPollInterval() time.Duration {
	if c.ReadyPollInterval > 0 {
		return c.ReadyPollInterval
	}
	return 2 * time.Second
}

// createArgs returns the full argument list ensure passes to kontur.Create
// beyond a name and -state-dir: -backend cfg.Backend first, when set, so a
// caller's own CreateArgs never needs to repeat it, followed by
// cfg.CreateArgs verbatim.
func (c KonturConfig) createArgs() []string {
	if c.Backend == "" {
		return c.CreateArgs
	}
	return append([]string{"-backend", c.Backend}, c.CreateArgs...)
}

// KonturSandboxes hands out mcp tools that run over SSH against a real
// bwsalmon/kontur-managed VM, one per slot, created on first use and
// reused across cycles after that -- matching HostSandboxes' own
// contract, that a slot's sandbox persists across cycles and resetting
// one between tasks is the caller's job, not this type's. Nothing here
// ever deletes a VM; a deployment that wants slots torn down (e.g. at
// shutdown) calls kontur.Delete itself, the same way nothing tears down a
// HostSandboxes directory either.
type KonturSandboxes struct {
	cfg KonturConfig

	mu       sync.Mutex
	created  map[string]bool
	gitCreds map[string]gitCredentials
}

// gitCredentials is what ConfigureGitCredentials remembers per slot, so
// Recreate can reapply it to the fresh VM it just built without its own
// caller (which only knows about slots, not credentials -- see cycle.go's
// recreatingSandboxes) having to carry it around.
type gitCredentials struct {
	remoteURL, token string
}

// NewKonturSandboxes returns a KonturSandboxes configured by cfg.
func NewKonturSandboxes(cfg KonturConfig) *KonturSandboxes {
	return &KonturSandboxes{cfg: cfg, created: map[string]bool{}, gitCreds: map[string]gitCredentials{}}
}

// VMNameFor returns the kontur VM name ToolsFor uses for slot, so
// something outside this package (an operator provisioning a VM's guest
// image ahead of time, say) can predict it without calling ToolsFor
// itself.
func (k *KonturSandboxes) VMNameFor(slot string) string {
	return k.cfg.NamePrefix + slot
}

// ToolsFor implements Sandboxes: it ensures slot's VM exists (creating it
// via kontur.Create on first use for that slot, within this
// KonturSandboxes' own lifetime -- see the type doc comment on why that is
// not the same as "exists," and why that distinction does not matter
// here), resolves its SSH endpoint via pkg/kontur, and returns
// mcp.NewSSHSandboxTools against it.
func (k *KonturSandboxes) ToolsFor(ctx context.Context, slot string) ([]mcp.Tool, error) {
	name := k.VMNameFor(slot)
	if err := k.ensure(ctx, name); err != nil {
		return nil, err
	}

	host, port, err := k.resolveEndpoint(ctx, name)
	if err != nil {
		return nil, err
	}

	runner := &mcp.SSHRunner{User: k.cfg.SSHUser, Host: host, Port: port, KeyPath: k.cfg.SSHKey}
	return mcp.NewSSHSandboxTools(runner, k.cfg.Workspace), nil
}

// ensure creates name's VM if this KonturSandboxes has not already created
// or observed one for it. A VM whose state kontur.Port can already read
// -- because a previous process's KonturSandboxes created it, or an
// operator ran "kontur vm create" by hand ahead of time -- counts as
// already existing and is left alone, the same "reuse what's there" choice
// HostSandboxes.RootFor makes for a directory that already exists on disk.
func (k *KonturSandboxes) ensure(ctx context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.created[name] {
		return nil
	}
	if _, err := kontur.Port(k.cfg.stateDir(), name); err == nil {
		k.created[name] = true
		return nil
	}
	if err := kontur.Create(ctx, k.cfg.stateDir(), name, k.cfg.createArgs()...); err != nil {
		return fmt.Errorf("orchestrator: creating kontur VM %q for sandbox: %w", name, err)
	}
	k.created[name] = true
	return nil
}

// ConfigureGitCredentials ensures slot's VM exists and points its git at
// remoteURL via token, the same one-time, per-slot setup a deployment
// does for a HostSandboxes slot directly via mcp.ConfigureGitCredentials
// (root/.gitconfig -- see that func's own doc comment) -- SSH, over the
// same endpoint ToolsFor resolves, is KonturSandboxes' only path to a
// slot's VM filesystem, so this is that setup's equivalent for this
// backend. Safe to call before any ToolsFor call for slot: like ToolsFor,
// it creates the VM on first use and waits out cfg.readyTimeout for it to
// become reachable.
//
// remoteURL and token are also remembered for slot, so a later Recreate
// call can reapply them to the fresh VM it just built without its own
// caller (dispatch, which knows only about slots, not credentials) having
// to carry them.
func (k *KonturSandboxes) ConfigureGitCredentials(ctx context.Context, slot, remoteURL, token string) error {
	name := k.VMNameFor(slot)
	if err := k.ensure(ctx, name); err != nil {
		return err
	}
	host, port, err := k.resolveEndpoint(ctx, name)
	if err != nil {
		return err
	}
	runner := &mcp.SSHRunner{User: k.cfg.SSHUser, Host: host, Port: port, KeyPath: k.cfg.SSHKey}
	if err := mcp.ConfigureGitCredentialsOverSSH(runner, remoteURL, token); err != nil {
		return fmt.Errorf("orchestrator: configuring git credentials on kontur VM %q: %w", name, err)
	}
	k.mu.Lock()
	k.gitCreds[slot] = gitCredentials{remoteURL: remoteURL, token: token}
	k.mu.Unlock()
	return nil
}

// Recreate tears slot's VM down and rebuilds it from scratch -- the
// isolation boundary bwsalmon/agents#353 asked for between one dispatched
// task and the next, the same "destroy then create" shape v1's own
// HostAdapter.recreate() gives a sandbox (grain/adapter/base.py), applied
// here per task rather than on v1's own weekly-or-wedged schedule. It is
// deliberately delete-then-create, the same two primitives ensure already
// calls, rather than `kontur vm update` (which happens to do the same
// thing for -backend docker, per bwsalmon/kontur's own internal/cli
// doc comment, but only there): that keeps this package's only path to
// building a VM in one place, cfg.createArgs(), instead of leaning on a
// kontur subcommand whose "recreate" behavior is backend-specific and not
// this package's to depend on.
//
// If ConfigureGitCredentials was ever called for slot, Recreate reapplies
// it to the rebuilt VM before returning -- a fresh VM has none of the
// previous one's filesystem, credentials included, and dispatch has no
// other opportunity to redo that setup between tasks.
func (k *KonturSandboxes) Recreate(ctx context.Context, slot string) error {
	name := k.VMNameFor(slot)

	if err := kontur.Delete(ctx, k.cfg.stateDir(), name); err != nil {
		return fmt.Errorf("orchestrator: deleting kontur VM %q to recreate it: %w", name, err)
	}
	k.mu.Lock()
	delete(k.created, name)
	k.mu.Unlock()

	if err := k.ensure(ctx, name); err != nil {
		return fmt.Errorf("orchestrator: recreating kontur VM %q: %w", name, err)
	}
	if _, _, err := k.resolveEndpoint(ctx, name); err != nil {
		return fmt.Errorf("orchestrator: waiting for recreated kontur VM %q to become ready: %w", name, err)
	}

	k.mu.Lock()
	creds, ok := k.gitCreds[slot]
	k.mu.Unlock()
	if ok {
		if err := k.ConfigureGitCredentials(ctx, slot, creds.remoteURL, creds.token); err != nil {
			return fmt.Errorf("orchestrator: reconfiguring git credentials on recreated kontur VM %q: %w", name, err)
		}
	}
	return nil
}

// resolveEndpoint reads name's assigned port and polls PodIP until it
// resolves or cfg.readyTimeout runs out -- a VM this process just created
// takes real wall-clock time to boot, get scheduled, and reach Ready
// before crictl has a pod to ask about, which a single PodIP call (as
// cmd/mcpserver's own, always-preexisting-VM wiring makes do with) would
// almost always lose the race with, so ToolsFor waits it out here instead
// of surfacing that as a caller-visible error on every freshly created VM.
func (k *KonturSandboxes) resolveEndpoint(ctx context.Context, name string) (host string, port int, err error) {
	port, err = kontur.Port(k.cfg.stateDir(), name)
	if err != nil {
		return "", 0, err
	}

	deadline := time.Now().Add(k.cfg.readyTimeout())
	for {
		if k.cfg.Backend == kontur.BackendDocker {
			host, err = kontur.DockerPodIP(ctx, name)
		} else {
			host, err = kontur.PodIP(ctx, k.cfg.runtimeEndpoint(), name)
		}
		if err == nil {
			return host, port, nil
		}
		if time.Now().After(deadline) {
			return "", 0, fmt.Errorf("orchestrator: waiting for kontur VM %q to become ready: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(k.cfg.readyPollInterval()):
		}
	}
}
