package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SandboxHealth is one live sandbox's status, as HostSandboxes' and
// KonturSandboxes' own Health methods report it (bwsalmon/agents#536:
// the UI's System pane needs to see which sandbox a stuck or failing
// run is actually sitting on, and whether that sandbox itself looks
// healthy).
// cmd/grain/daemon.go is the only place this type and ui.SandboxSnapshot
// are ever both in scope -- see that file's own sandboxHealthAdapter doc
// comment for why this package does not import ui to spare a caller that
// conversion.
//
// It reports *live* sandboxes, which is a change of meaning rather than
// of name: it used to be one row per dispatch slot, always the same rows,
// each either holding a run or idle. A sandbox now exists only while a
// run does, so an idle deployment reports nothing at all, and every row
// here belongs to something actually running.
type SandboxHealth struct {
	// Sandbox is the sandbox's own name -- the run's ID (dispatch.RunID),
	// which is also what model.Run.Sandbox records, so a row here joins
	// straight onto the run it belongs to.
	Sandbox string
	// Backend names which Sandboxes implementation produced this entry --
	// "host" for HostSandboxes, "kontur" for KonturSandboxes -- since a
	// deployment only ever runs one of the two (`grain daemon`'s own
	// -kontur-sandboxes flag picks between them), but the pane showing
	// this has no other way to know which.
	Backend string
	// Name is the sandbox's own identity within Backend -- the directory
	// on disk, or the kontur VM name.
	Name string
	// Ready is true once this sandbox has been confirmed reachable -- for
	// HostSandboxes that is just "the directory exists"; for
	// KonturSandboxes it means the VM's guest answered a command just
	// now, not merely that `konturctl vm create` once succeeded for it.
	Ready bool
	// Error is set instead of Ready when the sandbox could not be reached
	// just now -- the guest not answering. Empty when Ready is true.
	Error string
	// LoadAverage is the sandbox's own "/proc/loadavg" 1/5/15-minute
	// figures, space-joined verbatim, or empty when this backend has no
	// way to read it (HostSandboxes: nothing separates its own usage from
	// the host daemon's, which ui.Config.HostStats already reports).
	LoadAverage string
	// MemoryUsedMB and MemoryTotalMB are the sandbox's own memory, read
	// the same way ui.HostPressure's are for the daemon's host -- 0/0
	// when unavailable.
	MemoryUsedMB, MemoryTotalMB int
	// DiskUsedMB and DiskTotalMB are the sandbox's own root filesystem,
	// in MiB, from `df` inside the guest -- 0/0 when unavailable, the
	// same as the memory pair above.
	//
	// Reported in MiB rather than in the GiB
	// model.Config.SandboxDiskGB is expressed in, so that this stays a
	// plain integer at the sizes a sandbox actually reaches: the useful
	// reading is "3 GiB of 20 used", and a GiB-granular integer would
	// round the interesting half of that to zero. It is the figure the
	// disk-size setting is there to move -- a run that fails part way
	// through a build is often a guest that has simply filled its root
	// filesystem, which nothing in this pane could previously show.
	DiskUsedMB, DiskTotalMB int
}

// Health implements the same shape KonturSandboxes.Health does: one entry
// per sandbox currently held by a live run, in name order. A
// HostSandboxes directory has no isolation from the host daemon runs on
// (this package's own doc comment), so there is no separate CPU/memory
// reading to give it beyond "yes, its directory exists" -- a deployment
// running HostSandboxes gets its host-wide pressure from
// ui.Config.HostStats instead, which is exactly the number a starved
// sandbox would actually be contending over.
func (h *HostSandboxes) Health(ctx context.Context) []SandboxHealth {
	h.mu.Lock()
	names := make([]string, 0, len(h.live))
	roots := make(map[string]string, len(h.live))
	for name, sb := range h.live {
		names = append(names, name)
		roots[name] = sb.root
	}
	h.mu.Unlock()
	sort.Strings(names)

	out := make([]SandboxHealth, 0, len(names))
	for _, name := range names {
		out = append(out, SandboxHealth{Sandbox: name, Backend: "host", Name: roots[name], Ready: true})
	}
	return out
}

// healthTimeout bounds how long one sandbox's Health check waits for its
// VM's endpoint to resolve and answer SSH. Deliberately far shorter than
// cfg.readyTimeout (2 minutes by default): that timeout exists to give a
// VM this process just created real wall-clock time to boot before
// Acquire gives up on it, but a pane asking "is this sandbox
// healthy right now" wants a VM that is actually down reported as such
// quickly, not held up for the same boot-time grace period.
const healthTimeout = 5 * time.Second

// Health implements the same shape HostSandboxes.Health does: one entry
// per VM currently held by a live run, each with a best-effort live
// /proc/loadavg, /proc/meminfo and `df` reading pulled out of the guest
// over the same transport that run's own tools use. Unlike Acquire, this never
// waits out a VM's multi-minute boot -- a sandbox not reachable within
// healthTimeout gets Error set instead, which is exactly the condition
// this pane exists to surface (bwsalmon/agents#536), not one this method
// should hide behind a long wait or an error return that would take the
// whole pane down with it.
//
// It does, within that short budget, retry a guest that is not answering
// yet (waitForGuestReady): a sandbox becomes visible here the moment
// Acquire registers it, and a VM can still be finishing its boot then --
// the same brief window Acquire itself waits out (bwsalmon/agents#478).
// Without this, that transient state surfaced verbatim as a connection
// error with nothing in the daemon's own logs, since nothing was actually
// wrong long enough to log (bwsalmon/agents#553).
func (k *KonturSandboxes) Health(ctx context.Context) []SandboxHealth {
	k.mu.Lock()
	names := make([]string, 0, len(k.live))
	vmNames := make(map[string]string, len(k.live))
	for name, sb := range k.live {
		names = append(names, name)
		vmNames[name] = sb.vmName
	}
	k.mu.Unlock()
	sort.Strings(names)

	out := make([]SandboxHealth, 0, len(names))
	for _, name := range names {
		out = append(out, k.sandboxHealth(ctx, name, vmNames[name]))
	}
	return out
}

// healthRunner returns the transport sandboxHealth reads a VM's guest
// stats over, without runnerFor's boot-length wait: Health is a status
// pane, and a sandbox that is not reachable right now is exactly what it
// exists to report rather than something to block on -- see Health's own
// doc comment.
//
// The one wait it keeps is the short retry on a guest that is not
// answering yet, bounded by sandboxHealth's own healthTimeout budget
// rather than cfg.readyTimeout.
func (k *KonturSandboxes) healthRunner(ctx context.Context, name string) (sandboxRunner, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(healthTimeout)
	}
	if err := k.waitForGuestReady(ctx, name, deadline); err != nil {
		return nil, fmt.Errorf("reaching guest: %w", err)
	}
	return k.execRunner(name), nil
}

func (k *KonturSandboxes) sandboxHealth(ctx context.Context, sandbox, name string) SandboxHealth {
	health := SandboxHealth{Sandbox: sandbox, Backend: "kontur", Name: name}

	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	runner, err := k.healthRunner(ctx, name)
	if err != nil {
		health.Error = err.Error()
		return health
	}

	// One command rather than a `cat` and a `df`: this whole check runs
	// inside healthTimeout, and a second round trip over the same
	// transport would spend a second guest login's worth of it for one
	// more line of output. "kontur exec" hands its argv to the guest's
	// own shell (mcp.DockerExecRunner.Run), so the two run in one shell
	// here rather than as separate calls. `df -Pk` is the POSIX
	// single-line-per-filesystem form, which is what keeps
	// parseDiskStats' own "the line whose last field is /" reliable
	// against a long device name that plain `df` would wrap.
	//
	// The `&&` and the `|| true` are what keep the exit status meaning
	// what it did before this line grew a second command: the two /proc
	// files are what says the guest is answering at all, so their status
	// is the one reported, while a `df` that fails (a busybox build
	// without -P, say) leaves the disk figure at 0/0 -- "unavailable",
	// which the pane already has a shape for -- rather than turning a
	// perfectly reachable sandbox into an errored row. The order matters
	// too: parseProcStats reads /proc/loadavg off the first line, so the
	// `cat` has to come first regardless.
	stdout, stderr, exitCode := runner.Run(ctx,
		[]string{"sh", "-c", "cat /proc/loadavg /proc/meminfo && { df -Pk / || true; }"}, "")
	if exitCode != 0 {
		if stderr == "" {
			stderr = fmt.Sprintf("exited %d with no output", exitCode)
		}
		health.Error = fmt.Sprintf("reading guest stats: %s", strings.TrimSpace(stderr))
		return health
	}

	health.Ready = true
	health.LoadAverage, health.MemoryUsedMB, health.MemoryTotalMB = parseProcStats(stdout)
	health.DiskUsedMB, health.DiskTotalMB = parseDiskStats(stdout)
	return health
}

// parseProcStats picks the three /proc/loadavg fields and MemTotal/
// MemAvailable out of "cat /proc/loadavg /proc/meminfo"'s combined
// output. MemAvailable, not MemFree, is the kernel's own estimate of
// memory a new process could actually get without swapping (man 5 proc)
// -- the same number `free`'s "available" column shows -- so
// MemoryUsedMB is MemTotal minus that, not minus MemFree.
func parseProcStats(output string) (loadAverage string, usedMB, totalMB int) {
	lines := strings.Split(output, "\n")
	if len(lines) > 0 {
		if fields := strings.Fields(lines[0]); len(fields) >= 3 {
			loadAverage = strings.Join(fields[:3], " ")
		}
	}

	var totalKB, availKB int
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB = val
		case "MemAvailable:":
			availKB = val
		}
	}
	if totalKB > 0 {
		totalMB = totalKB / 1024
		usedMB = (totalKB - availKB) / 1024
	}
	return loadAverage, usedMB, totalMB
}

// parseDiskStats picks the root filesystem's size and usage out of the
// same combined output parseProcStats reads -- the "df -Pk /" half of it
// (see sandboxHealth, which asks for both in one command).
//
// The row is found by shape rather than by position: the line whose last
// field is "/" and which has df's own six POSIX columns
// (filesystem, 1024-blocks, used, available, capacity, mounted-on). That
// survives the header line, a shell warning printed ahead of the output,
// and the /proc lines the same stream carries, none of which look like
// that. Used, not "total minus available", so the reading matches what
// `df` itself reports: the two differ by the reserved blocks only root
// may use, and it is `df`'s answer an operator will compare this pane
// against.
//
// 0/0 when no such line is there at all -- a guest whose `df` does not
// take -P, or a busybox build that words its output differently -- which
// SandboxHealth.DiskUsedMB documents as "unavailable" and the pane shows
// as a dash, rather than an error that would take a perfectly healthy
// sandbox's whole row down with it.
func parseDiskStats(output string) (usedMB, totalMB int) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 || fields[5] != "/" {
			continue
		}
		totalKB, err1 := strconv.Atoi(fields[1])
		usedKB, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || totalKB <= 0 {
			continue
		}
		return usedKB / 1024, totalKB / 1024
	}
	return 0, 0
}
