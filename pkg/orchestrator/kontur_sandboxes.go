package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
)

// KonturConfig is what KonturSandboxes needs to create and reach one
// bwsalmon/kontur-managed VM per run. StateDir defaults to
// kontur.DefaultStateDir when left zero.
type KonturConfig struct {
	// StateDir is kontur's VM state directory (kontur.DefaultStateDir if
	// empty), where kontur records one "<name>.json" per VM.
	StateDir string
	// CreateArgs is appended to "konturctl vm create <name> -state-dir
	// <dir>" verbatim -- guest image,
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
	// on the host.
	//
	// Optional, and normally empty. `kontur run` generates a keypair for
	// each guest it boots and hands the guest the public half on its
	// kernel command line, so `kontur exec`'s own default path already
	// holds a key that guest authorizes (bwsalmon/kontur's
	// internal/guestkey). Set this only for a custom guest image that
	// authorizes a key of its own instead; the images directory
	// internal/dockervm mounts read-only at /images is the natural place
	// to put one.
	//
	// This used to be required, and grain used to generate and distribute
	// that keypair itself, because kontur's baked-in key only worked when
	// the guest disk and the runtime image came out of the same
	// `docker build` -- which a deployment pointing -disk at its own
	// guest build was not doing. The keypair moving into the boot removed
	// the mismatch and the deployment-side key management with it.
	ExecKeyPath string
	// Workspace is the working directory run_command/read_file/edit_file/
	// write_file operate in on each VM.
	Workspace string
	// ReadyTimeout bounds how long Acquire waits for a freshly created
	// VM's guest to become reachable before giving up (2 minutes if
	// zero). Every VM is freshly created now, so unlike before this
	// applies to every Acquire rather than only the first for a slot.
	ReadyTimeout time.Duration
	// ReadyPollInterval is how often Acquire re-probes the guest while
	// waiting out ReadyTimeout (2 seconds if zero).
	ReadyPollInterval time.Duration
	// NetMode selects how a VM reaches the network: kontur.NetModeFlat
	// (the default, and what an empty value means) or kontur.NetModeNAT.
	//
	// Flat mode is the default because it is the one that matches what
	// this package actually needs. NAT mode's whole apparatus -- a
	// private subnet, an address per VM, an external port per VM, and
	// the IP/Port below that exist solely to assign them -- serves
	// callers reaching a guest from *outside* its network
	// namespace, which this package deliberately does not do: its
	// transport execs into the VM's own container (mcp.DockerExecRunner)
	// and the SSH-to-a-forwarded-port transport that needed any of it was
	// removed. Under flat mode docker assigns the address and IP/Port are
	// ignored entirely.
	//
	// Flat mode needs a guest image built from kontur's own guest
	// overlays, for the control link "kontur exec" arrives on -- see
	// kontur.NetModeFlat's own doc comment. scripts/kontur/build-guest.sh
	// produces one; a deployment pulling a *prebuilt* guest image from
	// GRAIN_KONTUR_IMAGE_BUCKET has to republish it from that build
	// before switching, or every sandbox becomes unreachable.
	NetMode string

	// IP and Port, if set, are the "-ip" and "-port" every VM is created
	// with under NAT mode. konturctl requires "-ip" literally, with no
	// default (internal/staticpod.VMSpec.Validate), and CreateArgs is one
	// fixed list shared across every create call, so a deployment running
	// NAT mode needs somewhere to say them.
	//
	// They are passed verbatim, the same value to every VM. They used to
	// be a *base* that each slot's own number was added to, on the
	// reasoning that concurrent VMs shared one bridge and so needed
	// distinct addresses. Under the docker backend -- the only one this
	// package builds VMs under -- they do not share anything: internal/
	// dockervm.Create gives every VM its own netns-holder container that
	// the VM joins with "--network container:", so each has its own
	// bridge and its own private subnet, and Port only ever reaches
	// NETSHIM_VMS inside that namespace rather than being published on
	// the host. The derivation was guarding a collision that shape makes
	// impossible; it dates from the static-pod backend, where a pod's
	// containers genuinely did share a namespace.
	//
	// Worth confirming against a real NAT-mode host before leaning on it:
	// the reasoning above is from kontur's own source, not from two VMs
	// observed coming up on the same address. Flat mode, the default,
	// ignores both fields entirely.
	IP   string
	Port int
	// DefaultCPUs, DefaultMemoryMB and DefaultDiskGB
	// (bwsalmon/agents#534, grain/task-41) seed the deployment-wide
	// default VM shape (model.Config.SandboxCPUs/SandboxMemoryMB/
	// SandboxDiskGB) a run that requested no shape of its own falls
	// back to -- appended to createArgs' own result as
	// "-cpus"/"-memory-mb"/"-disk-size-mb", last, so they win out over
	// anything CreateArgs also happens to set; a run that did request a
	// shape overrides them per dimension (Shape.orDefault). Zero, the
	// default for all three, means this deployment named no size of its
	// own and falls through to grain's own DefaultShape -- every create
	// passes all three flags either way (createArgs).
	//
	// They are only the *starting* value: the shape actually applied to
	// each create lives on KonturSandboxes itself, where
	// SetDefaultShape replaces it while the daemon runs, so changing it
	// in Settings reaches the next sandbox built rather than only the
	// next restart.
	DefaultCPUs     int
	DefaultMemoryMB int
	DefaultDiskGB   int
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

// mibPerGiB converts Shape.DiskGB into the MiB `konturctl vm create
// -disk-size-mb` takes. grain's own knob is in GiB because a sandbox's
// disk is chosen in whole gigabytes; kontur's is in MiB because that is
// the granularity CHV_DISK_SIZE_MB sizes a qcow2 at. Neither is wrong,
// so the conversion lives at the one place they meet (createArgs).
const mibPerGiB = 1024

// DefaultShape is the size a sandbox VM is built at when nothing else
// named one -- grain's own kontur.DefaultCPUs/DefaultMemoryMB/
// DefaultDiskGB, filled in per dimension by createArgs after a run's own
// request and the deployment-wide default have both had their say.
//
// It exists so that `konturctl vm create` is never asked to pick a size:
// an unset dimension used to mean "leave the flag off", which handed the
// answer to whichever kontur is vendored (2 vCPU/2048 MiB) and, for
// disk, to however large the guest image a deployment built happens to
// be. Both are real numbers a run lives inside, and neither was chosen
// for the job an agent's sandbox does, so grain names all three itself
// and always passes them.
func DefaultShape() Shape {
	return Shape{CPUs: kontur.DefaultCPUs, MemoryMB: kontur.DefaultMemoryMB, DiskGB: kontur.DefaultDiskGB}
}

// createArgs returns the full argument list Acquire passes to
// kontur.Create for a sandbox's VM beyond a name and -state-dir:
// -backend docker first, the only backend this package supports (its
// transport reaches a guest by exec'ing into that VM's docker container,
// which no other backend gives it), so a caller's own CreateArgs never
// needs to repeat it, then "-net" (see NetMode; under the default, flat,
// the -ip/-port pair below is skipped entirely because the container
// runtime assigns the address), then cfg.CreateArgs verbatim, then the
// VM's size, then -ip/-port last -- each later group winning out over
// anything CreateArgs also happens to set, on the theory that a
// deployment configuring one of these more specific knobs at all wants it
// applied consistently rather than overridden by a CreateArgs list that
// is otherwise identical across every create call.
//
// shape is the size this VM is actually to be created at -- the run's
// own requested size (model.Task's SandboxCPUs/SandboxMemoryMB/
// SandboxDiskGB) already resolved per dimension against the deployment
// default (Shape.orDefault, in create, against whatever default the
// sandboxes currently carry), and resolved once more here against
// grain's own DefaultShape, so that a dimension nobody named still
// reaches kontur as a number rather than as a missing flag. This is
// where a per-task override takes effect now:
// a sandbox is built for one run, so the moment it is created is the one
// moment its size is decided. It used to be applied afterwards, by a
// `konturctl vm update` against a slot's already-created VM, and undone
// by the recreate that followed the run -- both of which existed only
// because the VM outlived the task.
func (c KonturConfig) createArgs(shape Shape) []string {
	shape = shape.orDefault(DefaultShape())
	args := append([]string{"-backend", kontur.BackendDocker, "-net", c.netMode()}, c.CreateArgs...)
	args = append(args, "-cpus", strconv.Itoa(shape.CPUs), "-memory-mb", strconv.Itoa(shape.MemoryMB))
	// -disk-size-mb sizes the writable qcow2 overlay the VM's own
	// container creates for it (bwsalmon/kontur's config.PrepareOverlay),
	// which is otherwise made exactly as large as the guest image it is
	// backed by. kontur takes MiB and grain's own knob is in GiB
	// (model.Config.SandboxDiskGB, Shape.DiskGB), so the conversion
	// happens here, at the single point the two vocabularies meet.
	//
	// Only the overlay is ever resized, never the guest image every other
	// VM booting it shares, so this needs the VM to be in kontur's
	// overlay disk mode -- which scripts/setup.sh asks for by name
	// (-disk-mode=overlay, also kontur's own default), and which a create
	// rejects outright rather than ignoring if it is not.
	//
	// It is passed on every create, the same way -cpus and -memory-mb
	// above are: a deployment that has never chosen a disk size gets
	// grain's own DefaultShape rather than the guest image's own size,
	// which is a few hundred megabytes of slack a build-heavy run spends
	// (scripts/kontur/build-guest.sh packs disk.img to the rootfs plus
	// 20%). kontur grows an overlay to the size asked for and refuses to
	// shrink one, so a VM never comes up smaller than this.
	args = append(args, "-disk-size-mb", strconv.Itoa(shape.DiskGB*mibPerGiB))
	// Flat mode takes its address from the container runtime, and
	// konturctl rejects "-ip" outright under it rather than ignoring it.
	// IP/Port are dropped here rather than treated as a misconfiguration
	// because a deployment's own systemd unit may still carry them from
	// before the switch, and failing every create over a pair of
	// now-meaningless flags would be worse than ignoring them.
	if c.netMode() == kontur.NetModeFlat {
		return args
	}
	if c.IP != "" {
		args = append(args, "-ip", c.IP)
	}
	if c.Port != 0 {
		args = append(args, "-port", strconv.Itoa(c.Port))
	}
	return args
}

// KonturSandboxes builds one bwsalmon/kontur-managed VM per run, reached
// by exec'ing into that VM's own docker container, and deletes it when
// the run is done.
//
// It used to hold one VM per slot, created on first use and reused across
// every task dispatched onto that slot -- which meant isolating one task
// from the next had to be added on top, as a delete-and-recreate after
// each run plus a startup pass to rebuild whatever a crashed process left
// behind. Neither exists now: a VM is created for a run and destroyed
// with it, so a task cannot inherit anything from the one before it
// because there is nothing left to inherit. What remains of the startup
// pass is ReapOrphans, which deletes VMs rather than rebuilding them.
type KonturSandboxes struct {
	cfg KonturConfig

	mu   sync.Mutex
	live map[string]*konturSandbox
	// defaults is the deployment-wide default VM shape the next Acquire
	// falls back to, seeded from cfg's own DefaultCPUs/DefaultMemoryMB
	// and replaceable while the daemon runs (SetDefaultShape). It lives
	// here rather than in cfg because cfg is read unsynchronised by
	// every live sandbox's own goroutine; this one field is the only
	// piece of a KonturSandboxes' configuration that changes after
	// construction, so it is the only one that needs mu.
	defaults Shape
}

// NewKonturSandboxes returns a KonturSandboxes configured by cfg.
func NewKonturSandboxes(cfg KonturConfig) *KonturSandboxes {
	return &KonturSandboxes{
		cfg:      cfg,
		live:     map[string]*konturSandbox{},
		defaults: Shape{CPUs: cfg.DefaultCPUs, MemoryMB: cfg.DefaultMemoryMB, DiskGB: cfg.DefaultDiskGB},
	}
}

// SetDefaultShape replaces the deployment-wide default VM shape every
// later Acquire resolves a run's own request against -- what
// model.Config.SandboxCPUs/SandboxMemoryMB/SandboxDiskGB mean once they
// have been changed in Settings, applied to the next sandbox built
// rather than only to the next process (cmd/grain/daemon.go's
// liveConfig, which calls this once per reconcile tick when any of them
// has changed).
//
// A zero dimension means "no deployment default", exactly as it does at
// construction: the create falls through to grain's own DefaultShape for
// that dimension rather than leaving the flag off. Sandboxes already
// built are untouched -- a VM's size is decided when it is created.
func (k *KonturSandboxes) SetDefaultShape(shape Shape) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.defaults = shape
}

// defaultShape reads back whatever default SetDefaultShape last set.
func (k *KonturSandboxes) defaultShape() Shape {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.defaults
}

// maxVMNameLen is how long a kontur VM name may be under the docker
// backend: netshim derives "tap-<name>" and "ctl-<name>" from it, and
// Linux caps an interface name at 15 bytes (IFNAMSIZ-1). Checked in
// VMNameFor so an over-long name fails with something that says what the
// budget is and what is spending it, rather than as `konturctl vm create`
// refusing a tap device name several layers down.
const maxVMNameLen = 11

// VMNamePrefix is prepended to a sandbox's own name to make its VM name,
// so every VM this deployment owns is identifiable as its own -- which is
// what the startup sweep (ReapOrphans) matches on to find VMs a previous
// process left behind.
//
// A constant rather than configuration, because there is nothing left to
// configure. maxVMNameLen leaves 11 bytes for the whole name and a run id
// ("<task id>-<attempt>", dispatch.RunID) needs most of them, so the only
// prefixes that fit at all are one or two bytes -- a choice too narrow to
// be worth an operator's attention, and one where a wrong answer is not a
// preference but a daemon that cannot build a single VM. It was
// -kontur-vm-name-prefix until now, defaulting (in scripts/setup.sh
// and terraform/gcp alike) to "kontur-", which fit while a VM was
// named after a *slot* ("kontur-1") and stopped fitting the moment one
// was named after its run.
//
// Two bytes rather than one: this is the whole of what tells a grain VM
// apart from anything else on the host, and ReapOrphans deletes what it
// matches. Deployments that must not reap each other's VMs get that from
// separate -kontur-state-dir values, which is what ReapOrphans actually
// lists from.
const VMNamePrefix = "g-"

// maxRunNameLen is how much of maxVMNameLen is left for the run's own
// name once VMNamePrefix has taken its share.
//
// Nine, one of which "<task id>-<attempt>" (dispatch.RunID) spends on its
// own separator -- so eight digits of task id and attempt combined, and
// "999999-99" sits exactly at the limit. Task ids are a monotonically
// increasing counter (Store.NewTaskID), so that is a ceiling a long-lived
// deployment climbs toward rather than a fixed fact; see
// TestVMNameBudgetCoversRealisticRunIDs for where it actually bites.
const maxRunNameLen = maxVMNameLen - len(VMNamePrefix)

// VMNameFor returns the kontur VM name for a sandbox, so something
// outside this package can predict it without calling Acquire.
func (k *KonturSandboxes) VMNameFor(sandbox string) (string, error) {
	name := VMNamePrefix + sandbox
	if len(name) > maxVMNameLen {
		return "", fmt.Errorf("orchestrator: kontur VM name %q is %d bytes, over the %d-byte limit netshim's tap "+
			"device naming imposes -- a sandbox is named after its run (\"<task id>-<attempt>\", dispatch.RunID), "+
			"and %q leaves %d bytes for one",
			name, len(name), maxVMNameLen, VMNamePrefix, maxRunNameLen)
	}
	return name, nil
}

// Acquire implements Sandboxes: create this run's VM, wait for its guest
// to answer, and hand back a handle that deletes it on Release.
//
// A VM that already exists under this name is deleted first rather than
// adopted. Adopting one was the old behaviour and the right one while a
// name meant a slot -- "reuse what's there," the same choice a directory
// that already existed got -- but a name means a run now, so anything
// already carrying it is either a previous process's leftover or a
// half-built VM from a failed Acquire. Either way its filesystem is not
// something this run should start from.
func (k *KonturSandboxes) Acquire(ctx context.Context, sandbox string, shape Shape) (Sandbox, error) {
	name, err := k.VMNameFor(sandbox)
	if err != nil {
		return nil, err
	}
	if err := k.create(ctx, name, shape); err != nil {
		// Even a failed create can leave a VM behind -- `konturctl vm
		// create` brings up a netns holder and the VM's own container in
		// separate steps, so a failure between them is a half-built VM
		// with nothing holding a handle to it.
		k.deleteQuietly(ctx, name)
		return nil, err
	}

	// A container found already dead is the one failure worth one retry:
	// internal/dockervm.Create starts a VM's containers with a plain
	// "docker run -d" and no restart policy, so a host that rebooted
	// mid-create leaves one behind that will never answer. Rebuilding
	// costs a boot; not rebuilding costs the run.
	runner, err := k.runnerFor(ctx, name)
	if err != nil {
		var deadErr *guestContainerDeadError
		if !errors.As(err, &deadErr) {
			k.deleteQuietly(ctx, name)
			return nil, err
		}
		if err := k.create(ctx, name, shape); err != nil {
			k.deleteQuietly(ctx, name)
			return nil, fmt.Errorf("orchestrator: kontur VM %q's container was found dead, rebuilding it: %w", name, err)
		}
		if runner, err = k.runnerFor(ctx, name); err != nil {
			k.deleteQuietly(ctx, name)
			return nil, err
		}
	}

	sb := &konturSandbox{owner: k, name: sandbox, vmName: name, shape: shape, runner: runner}
	k.mu.Lock()
	k.live[sandbox] = sb
	k.mu.Unlock()
	return sb, nil
}

// create deletes whatever is under name and builds it again. The delete
// is skipped when kontur has no state for the name at all: with no saved
// state to read a backend off, "konturctl vm delete" falls back to the
// static-pod backend and tries to unlink a manifest path (bwsalmon/
// kontur's own internal/cli runVMDelete), never reaching the docker
// backend this package builds every VM under -- so it accomplishes
// nothing at best, and at worst fails against a root-owned
// /etc/kubernetes/manifests the daemon does not run as.
func (k *KonturSandboxes) create(ctx context.Context, name string, shape Shape) error {
	if kontur.Exists(k.cfg.stateDir(), name) {
		if err := kontur.Delete(ctx, k.cfg.stateDir(), name); err != nil {
			return fmt.Errorf("orchestrator: deleting stale kontur VM %q before rebuilding it: %w", name, err)
		}
	}
	if err := kontur.Create(ctx, k.cfg.stateDir(), name, k.cfg.createArgs(shape.orDefault(k.defaultShape()))...); err != nil {
		return fmt.Errorf("orchestrator: creating kontur VM %q: %w", name, err)
	}
	return nil
}

// acquireCleanupTimeout bounds deleteQuietly's own detached delete, the
// same trade konturSandbox.Release makes with runCleanupTimeout and for
// the same two reasons: unbounded, a wedged `konturctl vm delete` would
// pin the dispatch goroutine that is already on its way out, and a VM
// that outlives the timeout is ReapOrphans' problem at the next startup.
// Shorter than runCleanupTimeout because nothing is waiting on this VM --
// it never became usable, so there is no guest to shut down gracefully.
const acquireCleanupTimeout = 30 * time.Second

// deleteQuietly tears down a VM that never became usable, so a failed
// Acquire does not leave one running with no handle to release it. The
// error is dropped deliberately: the caller is already failing for a
// reason it can act on, and "the cleanup after that also failed" would
// replace it with a less useful one. ReapOrphans catches whatever this
// misses at the next startup.
//
// It runs on a context detached from the caller's cancellation, exactly
// as konturSandbox.Release does. kontur.Delete execs konturctl through
// exec.CommandContext, so on an already-cancelled ctx the delete does not
// merely fail -- it never runs at all, and the VM is left behind. That is
// not a rare shutdown-only case: ctx is cancelled whenever the daemon is
// stopping *or* a task was closed mid-run, so the ordinary way an Acquire
// is interrupted is also the way it would leak. Detaching here is what
// makes "a failed Acquire leaves nothing behind" true on the path that
// most needs it.
func (k *KonturSandboxes) deleteQuietly(ctx context.Context, name string) {
	if !kontur.Exists(k.cfg.stateDir(), name) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), acquireCleanupTimeout)
	defer cancel()
	_ = kontur.Delete(ctx, k.cfg.stateDir(), name)
}

// ReapOrphans deletes every VM under VMNamePrefix in this deployment's
// own state directory,
// and reports how many it removed. A daemon calls it once, at startup,
// before anything can dispatch -- the same moment, and for the same
// reason, orchestrator.RecoverOrphanedRuns reconciles the run rows a dead
// process left behind: at that instant no VM can belong to this process,
// so any that exists is a leftover.
//
// This is what is left of the startup pass that used to rebuild every
// slot's VM. That pass existed because a slot's VM was meant to outlive
// the process and had to be made clean again; these are meant to have
// been deleted already, and the only question is whether something died
// before that could happen.
func (k *KonturSandboxes) ReapOrphans(ctx context.Context) (int, error) {
	names, err := kontur.List(k.cfg.stateDir())
	if err != nil {
		return 0, fmt.Errorf("orchestrator: listing kontur VMs to reap: %w", err)
	}
	var reaped int
	var errs []error
	for _, name := range names {
		if !strings.HasPrefix(name, VMNamePrefix) {
			continue
		}
		if err := kontur.Delete(ctx, k.cfg.stateDir(), name); err != nil {
			errs = append(errs, fmt.Errorf("deleting orphaned kontur VM %q: %w", name, err))
			continue
		}
		reaped++
	}
	return reaped, errors.Join(errs...)
}

// konturSandbox is one run's VM.
type konturSandbox struct {
	owner  *KonturSandboxes
	name   string
	vmName string
	// shape is the size this run asked its VM to be, kept so Rebuild can
	// create the replacement at the same one. Acquire's own resolution
	// against the deployment default (Shape.orDefault) happens inside
	// create, so this stays the run's own request rather than a snapshot
	// of the default that was in force when the VM was first built.
	shape  Shape
	runner sandboxRunner
}

func (s *konturSandbox) Name() string { return s.name }

// VMName implements vmNamedSandbox: the actual kontur VM name
// (VMNameFor's own result), not the run's own name Name returns above --
// what agent.RunConfig.KonturVM needs to be, so agent/claude's forked
// "mcpserver -kontur-vm" names the VM kontur itself knows, not this
// sandbox's internal key.
func (s *konturSandbox) VMName() string { return s.vmName }

// Tools implements Sandbox. The runner was resolved by Acquire, which
// does not return until the guest has answered a command, so there is no
// boot left to wait out here.
func (s *konturSandbox) Tools(ctx context.Context) ([]mcp.Tool, error) {
	return mcp.NewSSHSandboxTools(s.runner, s.owner.cfg.Workspace), nil
}

// ConfigureGitCredentials points this VM's guest at the proxy over the
// same transport every tool call uses -- what mcp.ConfigureGitCredentials
// does with a plain file write for a host directory.
func (s *konturSandbox) ConfigureGitCredentials(ctx context.Context, remoteURL, token string) error {
	if err := mcp.ConfigureGitCredentialsOverSSH(s.runner, remoteURL, token); err != nil {
		return fmt.Errorf("orchestrator: configuring git credentials on kontur VM %q: %w", s.vmName, err)
	}
	return nil
}

// PlaceFile implements SandboxPlacer: a capability's placement written
// into this VM's own filesystem over the same transport every tool call
// uses -- what applyPlacements does with os.MkdirAll/os.WriteFile for a
// host directory, and what a kontur VM had no route for at all until now
// (SandboxPlacer's own doc comment).
func (s *konturSandbox) PlaceFile(ctx context.Context, path, content, mode string) error {
	if err := mcp.PlaceFileOverSSH(ctx, s.runner, path, content, mode); err != nil {
		return fmt.Errorf("orchestrator: placing %s on kontur VM %q: %w", path, s.vmName, err)
	}
	return nil
}

// Rebuild implements SandboxRebuilder: delete this run's VM and create
// it again, at the same name and the same size, waiting for the new
// guest to answer a command before returning.
//
// It is Acquire's own create-and-wait pair, reused rather than
// reimplemented -- create deletes whatever is under the name first,
// which is exactly the destroy half of this operation.
//
// Nothing here replaces s.runner, and nothing needs to. The transport
// reaches a guest by exec'ing into the VM's own container, whose name is
// derived from the VM name alone (kontur.PodName), so the runner this
// sandbox already holds -- and the separate one a run's forked mcpserver
// holds, which this process could not reach to replace anyway -- address
// the new VM the moment it exists. runnerFor's result is discarded for
// that reason: what is wanted from it is the wait.
//
// A VM whose container is found dead is not retried the way Acquire
// retries it. Acquire is rebuilding on behalf of a run that has no other
// recourse; here the caller is an agent that just asked for a rebuild
// and can read the failure and ask again.
func (s *konturSandbox) Rebuild(ctx context.Context) error {
	if err := s.owner.create(ctx, s.vmName, s.shape); err != nil {
		return err
	}
	if _, err := s.owner.runnerFor(ctx, s.vmName); err != nil {
		return err
	}
	return nil
}

// Release deletes the VM. This is the isolation boundary itself, not a
// tidy-up after one: what makes the next run unable to see this one's
// checkout, credentials or leftover processes is that none of it exists
// any more.
func (s *konturSandbox) Release(ctx context.Context) error {
	s.owner.mu.Lock()
	delete(s.owner.live, s.name)
	s.owner.mu.Unlock()
	if !kontur.Exists(s.owner.cfg.stateDir(), s.vmName) {
		return nil
	}
	if err := kontur.Delete(ctx, s.owner.cfg.stateDir(), s.vmName); err != nil {
		return fmt.Errorf("orchestrator: deleting kontur VM %q: %w", s.vmName, err)
	}
	return nil
}

// sandboxRunner is the transport a sandbox's four tools run over:
// the method set mcp.NewSSHSandboxTools and mcp.ConfigureGitCredentialsOverSSH
// both take, satisfied by mcp.DockerExecRunner.
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

// execRunner builds the docker-exec transport for name's VM. Its
// ConnectTimeout is left at guestexec's own default rather than tied to
// cfg.readyTimeout: by the time a caller has a runner back, runnerFor has
// already waited out the boot this config's timeouts describe, and what
// is left for this to ride out is only the ordinary case of a guest that
// blinks mid-task.
func (k *KonturSandboxes) execRunner(name string) *mcp.DockerExecRunner {
	return &mcp.DockerExecRunner{
		Container: kontur.PodName(name),
		User:      k.cfg.SSHUser,
		KeyPath:   k.cfg.ExecKeyPath,
	}
}

// waitForGuestExec polls a trivial command through the docker-exec
// transport until it succeeds or deadline passes: a VM whose container is
// up is not yet a VM whose guest has finished booting, and everything
// short of that looks like a failure to reach it.
//
// It probes with a runner of its own whose ConnectTimeout is one poll
// interval, so a probe against a guest that is not up yet gives up and
// lets this loop decide whether to keep waiting, rather than each probe
// sitting on guestexec's own 30s default and overshooting the deadline
// this is measuring against.
//
// It fails immediately on a VM container that has already exited rather
// than waiting out the rest of deadline exec'ing into something that will
// never answer.
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
			return &guestContainerDeadError{fmt.Errorf("VM container %q exited (status %q) before its guest ever ran a command -- check `docker logs %s`: %w", probe.Container, status, probe.Container, lastErr)}
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

// guestContainerDeadError is what waitForGuestExec returns when it gives
// up specifically because the VM's own container has already exited or
// died, never for any other probe failure (a timeout, a guest still
// booting, ...) -- see dockerExitedEarly. Acquire matches on this type
// alone (errors.As) to decide whether a failure is worth one rebuild, so
// it stays its own type rather than a sentinel value or a string match
// against the formatted message below.
type guestContainerDeadError struct{ err error }

func (e *guestContainerDeadError) Error() string { return e.err.Error() }
func (e *guestContainerDeadError) Unwrap() error { return e.err }

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
// dockerExitedEarly treats as "this container will never answer" rather
// than "not ready yet" -- see its own doc comment. "created" is
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
