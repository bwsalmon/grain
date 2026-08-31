package orchestrator

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
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
	// its directory. Under Backend/kontur.BackendDocker, keep
	// NamePrefix+the longest slot name to 11 bytes or fewer: netshim
	// names each VM's tap device "tap-"+name (internal/netshim's own
	// config.go), and Linux caps interface names at 15 bytes (IFNAMSIZ-1)
	// -- confirmed by hand, the failure mode a longer prefix hits is
	// `konturctl vm create` itself failing with "tap device name ...
	// exceeds 15 characters", not anything this package can validate
	// ahead of time without duplicating netshim's own naming.
	NamePrefix string
	// StateDir is kontur's VM state directory (kontur.DefaultStateDir if
	// empty), used to look up a slot's VM's external port.
	StateDir string
	// CreateArgs is appended to "konturctl vm create <name> -state-dir
	// <dir>" verbatim when a slot's VM does not exist yet -- guest image,
	// guest SSH port, resource sizing, and anything else a deployment's
	// own "konturctl vm create" needs beyond a name, since this package has
	// no way to know those on its own (see package kontur's doc comment).
	CreateArgs []string
	// SSHUser is the guest account KonturSandboxes runs every tool call
	// as -- passed to `kontur exec` as KONTUR_EXEC_USER. Still an SSH
	// login into the guest, just one originating inside the VM's own
	// container.
	SSHUser string
	// ExecKeyPath is the private key `kontur exec` authenticates to the
	// guest with, as a path *inside the VM's own container* rather than
	// on the host. Required: left empty, `kontur exec` falls back to the
	// dedicated key bwsalmon/kontur's own Dockerfile bakes into the image
	// and authorizes on the guest rootfs that same Dockerfile builds --
	// which a deployment pointing -disk at packer/kontur/build.sh's
	// output is not booting.
	//
	// The images directory internal/dockervm already mounts read-only at
	// /images is the natural place to put it: it needs no change to
	// kontur to be readable there.
	ExecKeyPath string
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
	// NetMode selects how a VM reaches the network: kontur.NetModeFlat
	// (the default, and what an empty value means) or kontur.NetModeNAT.
	//
	// Flat mode is the default because it is the one that matches what
	// this package actually needs. NAT mode's whole apparatus -- a
	// private subnet, an address per VM, an external port per VM, and
	// the BaseIP/BasePort derivation below that exists solely to assign
	// them -- serves callers reaching a guest from *outside* its network
	// namespace, which this package deliberately does not do: its
	// transport execs into the VM's own container (mcp.DockerExecRunner)
	// and the SSH-to-a-forwarded-port transport that needed any of it was
	// removed. Under flat mode docker assigns the address and BaseIP/
	// BasePort are ignored entirely.
	//
	// Flat mode needs a guest image built from kontur's own guest
	// overlays, for the control link "kontur exec" arrives on -- see
	// kontur.NetModeFlat's own doc comment. packer/kontur/build-guest.sh
	// produces one; a deployment pulling a *prebuilt* guest image from
	// GRAIN_KONTUR_IMAGE_BUCKET has to republish it from that build
	// before switching, or every sandbox becomes unreachable.
	NetMode string

	// BaseIP, if set, is the "-ip" ensure passes `konturctl vm create` for
	// slot "1" (model.SlotNames' own 1-based, all-numeric contract); every
	// other slot gets the next IPv4 address after it, offset by its own
	// number minus one. konturctl has no way to do this derivation
	// itself -- internal/staticpod.VMSpec.Validate requires "-ip"
	// literally, with no default -- and CreateArgs is one fixed list
	// shared verbatim across every slot's create call, so without this, a
	// deployment with more than one slot (-max-concurrent > 1) would ask
	// konturctl to give every VM after the first the exact same address
	// on the one bridge they all share. Leave unset for a single-slot
	// deployment content to put a literal "-ip" in CreateArgs itself.
	BaseIP string
	// BasePort, if set, is the "-port" ensure passes for slot "1", the
	// same derivation and for the same reason BaseIP is: every other
	// slot's is BasePort plus its own number minus one.
	BasePort int
	// DefaultCPUs and DefaultMemoryMB (bwsalmon/agents#534), if set, are
	// appended to createArgs' own result as "-cpus"/"-memory-mb" -- the
	// deployment-wide default VM shape (model.Config.SandboxCPUs/
	// SandboxMemoryMB), applied last so it wins out over anything
	// CreateArgs also happens to set, the same reasoning BaseIP/BasePort's
	// own doc comment gives for appending those last too. Zero, the
	// default for both, omits the corresponding flag entirely and leaves
	// bwsalmon/kontur's own `konturctl vm create` default in place.
	DefaultCPUs     int
	DefaultMemoryMB int
}

// netMode returns c.NetMode, treating an empty value as the default,
// kontur.NetModeFlat.
func (c KonturConfig) netMode() string {
	if c.NetMode == "" {
		return kontur.NetModeFlat
	}
	return c.NetMode
}

func (c KonturConfig) stateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return kontur.DefaultStateDir
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
// for slot's VM beyond a name and -state-dir: -backend docker first, the
// only backend this package supports (its transport reaches a guest by
// exec'ing into that VM's docker container, which no other backend gives
// it), so a caller's own CreateArgs never needs to repeat it, then "-net"
// (see NetMode; under the default, flat, the -ip/-port pair below is
// skipped entirely because the container runtime assigns the address),
// then
// cfg.CreateArgs verbatim, then DefaultCPUs/DefaultMemoryMB (if set) as
// "-cpus"/"-memory-mb", then a "-ip"/"-port" pair derived from
// BaseIP/BasePort and slot's own number last -- each later group winning
// out over anything CreateArgs also happens to set, on the theory that a
// deployment configuring one of these more specific knobs at all wants it
// applied consistently, not overridden per slot by a CreateArgs list that
// is otherwise identical across every slot's call.
func (c KonturConfig) createArgs(slot string) ([]string, error) {
	args := append([]string{"-backend", kontur.BackendDocker, "-net", c.netMode()}, c.CreateArgs...)
	if c.DefaultCPUs != 0 {
		args = append(args, "-cpus", strconv.Itoa(c.DefaultCPUs))
	}
	if c.DefaultMemoryMB != 0 {
		args = append(args, "-memory-mb", strconv.Itoa(c.DefaultMemoryMB))
	}
	// Flat mode takes its address from the container runtime, and
	// konturctl rejects "-ip" outright under it rather than ignoring it.
	// BaseIP/BasePort are dropped here rather than treated as a
	// misconfiguration because a deployment's own systemd unit may still
	// carry them from before the switch, and failing every create over a
	// pair of now-meaningless flags would be worse than ignoring them.
	if c.netMode() == kontur.NetModeFlat {
		return args, nil
	}
	if c.BaseIP == "" && c.BasePort == 0 {
		return args, nil
	}
	offset, err := slotOffset(slot)
	if err != nil {
		return nil, err
	}
	if c.BaseIP != "" {
		ip, err := addToIPv4(c.BaseIP, offset)
		if err != nil {
			return nil, fmt.Errorf("kontur: KonturConfig.BaseIP: %w", err)
		}
		args = append(args, "-ip", ip)
	}
	if c.BasePort != 0 {
		args = append(args, "-port", strconv.Itoa(c.BasePort+offset))
	}
	return args, nil
}

// slotOffset parses slot as model.SlotNames does -- a decimal string
// counting up from "1" -- and returns it minus one, the 0-based offset
// BaseIP/BasePort above add to derive slot's own address from the first
// slot's.
func slotOffset(slot string) (int, error) {
	n, err := strconv.Atoi(slot)
	if err != nil {
		return 0, fmt.Errorf("kontur: slot %q is not numeric, required to derive its own -ip/-port from KonturConfig.BaseIP/BasePort (see model.SlotNames): %w", slot, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("kontur: slot %q must be 1 or greater to derive its own -ip/-port from KonturConfig.BaseIP/BasePort (see model.SlotNames)", slot)
	}
	return n - 1, nil
}

// addToIPv4 adds offset to base, the arithmetic BaseIP needs to hand
// slot's own number-minus-one out as the next address after the first
// slot's.
func addToIPv4(base string, offset int) (string, error) {
	ip := net.ParseIP(base).To4()
	if ip == nil {
		return "", fmt.Errorf("invalid IPv4 address %q", base)
	}
	sum := binary.BigEndian.Uint32(ip) + uint32(offset)
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], sum)
	return net.IP(out[:]).String(), nil
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
	// nameLocks holds one *sync.Mutex per VM name, taken around the
	// kontur.Create call inside ensure -- see ensure's own doc comment for
	// why a single KonturSandboxes-wide lock cannot guard that call the
	// way mu guards created/gitCreds.
	nameLocks map[string]*sync.Mutex
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
	return &KonturSandboxes{
		cfg:       cfg,
		created:   map[string]bool{},
		gitCreds:  map[string]gitCredentials{},
		nameLocks: map[string]*sync.Mutex{},
	}
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
	if err := k.ensure(ctx, name, slot); err != nil {
		return nil, err
	}

	runner, err := k.runnerFor(ctx, name)
	if err != nil {
		return nil, err
	}
	return mcp.NewSSHSandboxTools(runner, k.cfg.Workspace), nil
}

// Reshape resizes slot's VM to cpus vCPUs and/or memoryMB MiB of memory
// via "konturctl vm update" -- the per-task shape override
// bwsalmon/agents#534 asked for, called by orchestrator.runOne ahead of a
// task whose own model.Task.SandboxCPUs/SandboxMemoryMB differ from the
// deployment default cfg.DefaultCPUs/DefaultMemoryMB already baked into
// this slot's VM at create time. A zero cpus or memoryMB leaves that
// dimension alone, the same "unset means keep whatever konturctl vm
// update was last told" partial-update contract bwsalmon/kontur's own
// registerVMFlags gives every flag it does not receive -- so a task
// overriding only one of the two never has to know or repeat the other's
// current value.
//
// Unlike ensure/Recreate's own "always leaves a fresh VM behind," a
// Reshape is not undone until the next Recreate rebuilds the VM from
// cfg.createArgs (the deployment default) -- exactly once, right after
// this task's own run, the same boundary every other task's isolation
// already goes through. A task that never sets either field never calls
// this at all (see runOne), so a deployment using no per-task overrides
// sees no behavior change here whatsoever.
//
// Reshape creates slot's VM first (via ensure) if it does not exist yet,
// the same "first ToolsFor call for a slot creates its VM" contract every
// other exported method on this type already gives, and takes the same
// per-name lock ensure's own kontur.Create call does, for the same
// reason: konturctl vm update execs a real, potentially slow subprocess,
// and guarding it with anything wider than a per-name lock would
// serialize unrelated slots' VM operations against each other.
func (k *KonturSandboxes) Reshape(ctx context.Context, slot string, cpus, memoryMB int) error {
	name := k.VMNameFor(slot)
	if err := k.ensure(ctx, name, slot); err != nil {
		return err
	}
	var args []string
	if cpus != 0 {
		args = append(args, "-cpus", strconv.Itoa(cpus))
	}
	if memoryMB != 0 {
		args = append(args, "-memory-mb", strconv.Itoa(memoryMB))
	}
	if len(args) == 0 {
		return nil
	}

	lock := k.lockFor(name)
	lock.Lock()
	defer lock.Unlock()

	if err := kontur.Update(ctx, k.cfg.stateDir(), name, args...); err != nil {
		return fmt.Errorf("orchestrator: reshaping kontur VM %q: %w", name, err)
	}
	return nil
}

// ensure creates name's VM if this KonturSandboxes has not already created
// or observed one for it. A VM whose state kontur.Port can already read
// -- because a previous process's KonturSandboxes created it, or an
// operator ran "konturctl vm create" by hand ahead of time -- counts as
// already existing and is left alone, the same "reuse what's there" choice
// HostSandboxes.RootFor makes for a directory that already exists on disk.
//
// The actual kontur.Create call runs under a lock scoped to name alone
// (lockFor), not k.mu: k.mu only ever guards fast, in-memory map
// operations elsewhere in this package, but kontur.Create execs a real
// "konturctl vm create" subprocess that can run for a long time (a VM
// genuinely booting under KVM, not a fake standing in for one). Guarding
// that call with k.mu itself -- as an earlier version of this method did
// -- would serialize VM creation across every slot in this
// KonturSandboxes, not just repeat calls for the same one, silently
// undoing the concurrency reconcileDispatch's own doc comment promises
// ("HostSandboxes and KonturSandboxes both guard their own per-slot state
// with a mutex keyed by slot") every time more than one slot's VM needs
// creating at once -- confirmed by hand with a fake konturctl slow enough
// to make two concurrent ToolsFor calls for distinct slots visibly wait on
// each other before this change existed (sandboxes_concurrency_test.go's
// TestKonturSandboxesCreatesDistinctSlotsVMsConcurrentlyNotSerially).
func (k *KonturSandboxes) ensure(ctx context.Context, name, slot string) error {
	if k.alreadyCreated(name) {
		return nil
	}

	lock := k.lockFor(name)
	lock.Lock()
	defer lock.Unlock()

	// Re-check now that this name's lock is held exclusively: a concurrent
	// call for the same name may have finished creating it between the
	// fast-path check above and acquiring this lock.
	if k.alreadyCreated(name) {
		return nil
	}
	if kontur.Exists(k.cfg.stateDir(), name) {
		k.markCreated(name)
		return nil
	}
	args, err := k.cfg.createArgs(slot)
	if err != nil {
		return fmt.Errorf("orchestrator: creating kontur VM %q for sandbox: %w", name, err)
	}
	if err := kontur.Create(ctx, k.cfg.stateDir(), name, args...); err != nil {
		return fmt.Errorf("orchestrator: creating kontur VM %q for sandbox: %w", name, err)
	}
	k.markCreated(name)
	return nil
}

// lockFor returns the *sync.Mutex ensure takes around kontur.Create for
// name, creating one under k.mu on first use -- k.mu itself is held only
// long enough to look up or insert into nameLocks, never across the
// subprocess call the returned lock actually guards.
func (k *KonturSandboxes) lockFor(name string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	lock, ok := k.nameLocks[name]
	if !ok {
		lock = &sync.Mutex{}
		k.nameLocks[name] = lock
	}
	return lock
}

func (k *KonturSandboxes) alreadyCreated(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.created[name]
}

func (k *KonturSandboxes) markCreated(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.created[name] = true
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
	if err := k.ensure(ctx, name, slot); err != nil {
		return err
	}
	runner, err := k.runnerFor(ctx, name)
	if err != nil {
		return err
	}
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
// calls, rather than `konturctl vm update` (which happens to do the same
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

	if err := k.ensure(ctx, name, slot); err != nil {
		return fmt.Errorf("orchestrator: recreating kontur VM %q: %w", name, err)
	}
	if _, err := k.runnerFor(ctx, name); err != nil {
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

// sandboxRunner is the transport a slot's four sandbox tools run over:
// the method set mcp.NewSSHSandboxTools and mcp.ConfigureGitCredentialsOverSSH
// both take, satisfied by mcp.SSHRunner and mcp.DockerExecRunner alike.
// Declared here rather than imported because mcp's own equivalent
// (remoteRunner) is unexported -- deliberately, so that package's tests
// can double it -- and runnerFor needs a name for what it returns.
type sandboxRunner interface {
	Run(ctx context.Context, argv []string, stdin string) (stdout, stderr string, exitCode int)
}

// runnerFor returns the transport reaching name's guest, once that guest
// is actually reachable over it -- so a caller holding a runner has
// already waited out the VM's boot.
//
// The wait is waitForGuestExec rather than anything watching a port,
// because there is no port out here to watch: reaching this guest means
// exec'ing into its own container, and the first thing that can be
// observed succeeding is a whole command running inside the guest.
func (k *KonturSandboxes) runnerFor(ctx context.Context, name string) (sandboxRunner, error) {
	if err := k.waitForGuestExec(ctx, name, time.Now().Add(k.cfg.readyTimeout())); err != nil {
		return nil, fmt.Errorf("orchestrator: waiting for kontur VM %q's guest to become reachable: %w", name, err)
	}
	return k.execRunner(name), nil
}

// dockerExecRunner builds the docker-exec transport for name's VM. Its
// ConnectTimeout is left at guestexec's own default rather than tied to
// cfg.readyTimeout: by the time a caller has a runner back, runnerFor has
// already waited out the boot this config's timeouts describe, and what
// is left for this to ride out is only the ordinary case of a guest that
// blinks mid-task -- the same thing SSHRunner covers with its own fixed
// ConnectTimeout=10 rather than anything derived from cfg.
func (k *KonturSandboxes) execRunner(name string) *mcp.DockerExecRunner {
	return &mcp.DockerExecRunner{
		Container: kontur.PodName(name),
		User:      k.cfg.SSHUser,
		KeyPath:   k.cfg.ExecKeyPath,
	}
}

// waitForGuestExec polls a trivial command through the docker-exec
// transport until it succeeds or deadline passes -- the docker-exec
// counterpart to waitForSSHPort, and for the same reason: a VM whose
// container is up is not yet a VM whose guest has finished booting, and
// everything short of that looks like a failure to reach it.
//
// It probes with a runner of its own whose ConnectTimeout is one poll
// interval, so a probe against a guest that is not up yet gives up and
// lets this loop decide whether to keep waiting, rather than each probe
// sitting on guestexec's own 30s default and overshooting the deadline
// this is measuring against. That mirrors what waitForSSHPort gets from
// giving its dialer a readyPollInterval timeout.
//
// Like waitForSSHPort, it fails immediately on a VM container that has
// already exited rather than waiting out the rest of deadline exec'ing
// into something that will never answer.
func (k *KonturSandboxes) waitForGuestExec(ctx context.Context, name string, deadline time.Time) error {
	probe := k.execRunner(name)
	probe.ConnectTimeout = k.cfg.readyPollInterval()
	var lastErr error
	for {
		_, stderr, exitCode := probe.Run(ctx, []string{"true"}, "")
		if exitCode == 0 {
			return nil
		}
		lastErr = guestExecProbeError(probe.Container, stderr, exitCode)
		if status, dead := dockerExitedEarly(ctx, name); dead {
			return fmt.Errorf("VM container %q exited (status %q) before its guest ever ran a command -- check `docker logs %s`: %w", probe.Container, status, probe.Container, lastErr)
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(k.cfg.readyPollInterval()):
		}
	}
}

// guestExecProbeError renders one failed waitForGuestExec probe as the
// error a caller sees if the deadline runs out on it, naming the command
// an operator can run by hand to see the same thing.
func guestExecProbeError(container, stderr string, exitCode int) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = fmt.Sprintf("exited %d with no output", exitCode)
	}
	return fmt.Errorf("`docker exec %s kontur exec -- true`: %s", container, detail)
}

// dockerContainerDead is the set of docker State.Status values
// waitForSSHPort treats as "this container will never answer" rather than
// "not ready yet" -- see dockerExitedEarly's own doc comment. "created" is
// deliberately absent: a container docker has accepted but not yet
// started is still on its way up, same as "running" itself taking a
// moment to start accepting connections.
var dockerContainerDead = map[string]bool{"exited": true, "dead": true}

// dockerExitedEarly reports whether name's own VM container (not the
// "-netns" holder) has already exited -- see kontur.DockerContainerStatus's
// own doc comment for why "vm create" returning success does not mean
// this can't happen. Errors from the status lookup itself (e.g. a
// transient docker daemon hiccup) are treated as "not dead" rather than
// propagated: this is only ever a fast-fail optimization layered on top
// of waitForGuestExec's own deadline, which still applies regardless.
func dockerExitedEarly(ctx context.Context, name string) (status string, dead bool) {
	status, err := kontur.DockerContainerStatus(ctx, name)
	if err != nil {
		return "", false
	}
	return status, dockerContainerDead[status]
}
