package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SlotHealth is one dispatch slot's own sandbox status, as HostSandboxes'
// and KonturSandboxes' own Health methods report it (bwsalmon/agents#536:
// a debugging pane needs to see which sandbox a stuck or failing run is
// actually sitting on, and whether that sandbox itself looks healthy).
// cmd/grain/daemon.go is the only place this type and ui.SandboxSnapshot
// are ever both in scope -- see that file's own sandboxHealthAdapter doc
// comment for why this package does not import ui to spare a caller that
// conversion.
type SlotHealth struct {
	// Slot is the dispatch.Cycle slot identifier this entry is for
	// (model.SlotNames' own "1", "2", ...).
	Slot string
	// Backend names which Sandboxes implementation produced this entry --
	// "host" for HostSandboxes, "kontur" for KonturSandboxes -- since a
	// deployment only ever runs one of the two (run()'s own doc comment:
	// "Exactly one of hostSandboxes/konturSandboxes is non-nil"), but the
	// pane showing this has no other way to know which.
	Backend string
	// Name is the sandbox's own identity within Backend -- the directory
	// HostSandboxes.RootFor returned, or the VM name KonturSandboxes.
	// VMNameFor derived.
	Name string
	// Ready is true once this slot's sandbox has been confirmed reachable
	// -- for HostSandboxes that is just "the directory exists" (RootFor
	// already guarantees it before Health can see the slot at all); for
	// KonturSandboxes it means the VM's pod/container resolved an IP and
	// answered SSH just now, not merely that `konturctl vm create` once
	// succeeded for it.
	Ready bool
	// Error is set instead of Ready when this slot's sandbox could not be
	// reached just now -- kontur.Port/PodIP/DockerPodIP failing, or the
	// guest not answering SSH. Empty when Ready is true.
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
}

// Health implements the same shape KonturSandboxes.Health does: one entry
// per slot whose directory RootFor has already created, in slot order.
// A HostSandboxes directory has no isolation from the host daemon runs on
// (this package's own doc comment), so there is no separate CPU/memory
// reading to give it beyond "yes, its directory exists" -- a deployment
// running HostSandboxes gets its host-wide pressure from
// ui.Config.HostStats instead, which is exactly the number a starved
// HostSandboxes slot would actually be contending over.
func (h *HostSandboxes) Health(ctx context.Context) []SlotHealth {
	h.mu.Lock()
	defer h.mu.Unlock()

	slots := make([]string, 0, len(h.roots))
	for slot := range h.roots {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	out := make([]SlotHealth, 0, len(slots))
	for _, slot := range slots {
		out = append(out, SlotHealth{Slot: slot, Backend: "host", Name: h.roots[slot], Ready: true})
	}
	return out
}

// healthTimeout bounds how long one slot's Health check waits for its
// VM's endpoint to resolve and answer SSH. Deliberately far shorter than
// cfg.readyTimeout (2 minutes by default): that timeout exists to give a
// VM this process just created real wall-clock time to boot before
// ToolsFor gives up on it, but a debugging pane asking "is this sandbox
// healthy right now" wants a VM that is actually down reported as such
// quickly, not held up for the same boot-time grace period.
const healthTimeout = 5 * time.Second

// Health implements the same shape HostSandboxes.Health does: one entry
// per VM this KonturSandboxes has created so far (in this process's own
// lifetime -- see the type's own doc comment on why that, not "every VM
// that exists," is what this package tracks), each with a best-effort
// live /proc/loadavg and /proc/meminfo reading pulled out of the guest
// over the same transport ToolsFor's own tools use. Unlike runnerFor,
// this never waits out a VM's multi-minute boot -- a slot whose VM is not
// reachable within healthTimeout gets Error set instead, which is exactly
// the condition this pane exists to surface (bwsalmon/agents#536), not one
// this method should hide behind a long wait or an error return that would
// take the whole pane down with it.
//
// It does, within that short budget, retry a guest that is not answering
// yet (waitForGuestExec) before giving up: ensure() marks a slot
// "created" the instant kontur.Create's subprocess returns, well before
// the guest inside that container has booted, so a slot can become
// visible to Health() while it is still coming up -- the same brief
// window runnerFor waits out for ToolsFor (bwsalmon/agents#478). Without
// this, that transient state surfaced verbatim as a connection error with
// nothing in the daemon's own logs, since nothing here was actually wrong
// long enough to log (bwsalmon/agents#553).
func (k *KonturSandboxes) Health(ctx context.Context) []SlotHealth {
	k.mu.Lock()
	names := make([]string, 0, len(k.created))
	for name, created := range k.created {
		if created {
			names = append(names, name)
		}
	}
	k.mu.Unlock()
	sort.Strings(names)

	out := make([]SlotHealth, 0, len(names))
	for _, name := range names {
		slot := strings.TrimPrefix(name, k.cfg.NamePrefix)
		out = append(out, k.slotHealth(ctx, slot, name))
	}
	return out
}

// healthRunner returns the transport slotHealth reads a VM's guest stats
// over, without runnerFor's boot-length wait: Health is a status pane,
// and a slot that is not reachable right now is exactly what it exists to
// report rather than something to block on -- see Health's own doc
// comment.
//
// The one wait it keeps is the short retry on a guest that is not
// answering yet, bounded by slotHealth's own healthTimeout budget rather
// than cfg.readyTimeout.
func (k *KonturSandboxes) healthRunner(ctx context.Context, name string) (sandboxRunner, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(healthTimeout)
	}
	if err := k.waitForGuestExec(ctx, name, deadline); err != nil {
		return nil, fmt.Errorf("reaching guest: %w", err)
	}
	return k.execRunner(name), nil
}

func (k *KonturSandboxes) slotHealth(ctx context.Context, slot, name string) SlotHealth {
	health := SlotHealth{Slot: slot, Backend: "kontur", Name: name}

	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	runner, err := k.healthRunner(ctx, name)
	if err != nil {
		health.Error = err.Error()
		return health
	}

	stdout, stderr, exitCode := runner.Run(ctx, []string{"cat", "/proc/loadavg", "/proc/meminfo"}, "")
	if exitCode != 0 {
		if stderr == "" {
			stderr = fmt.Sprintf("exited %d with no output", exitCode)
		}
		health.Error = fmt.Sprintf("reading guest stats: %s", strings.TrimSpace(stderr))
		return health
	}

	health.Ready = true
	health.LoadAverage, health.MemoryUsedMB, health.MemoryTotalMB = parseProcStats(stdout)
	return health
}

// healthPortDialTimeout bounds each dial waitForPortHealthy makes, and how
// long it waits before retrying one that failed -- short enough that
// several attempts fit inside healthTimeout's own budget. Deliberately its
// own constant rather than a reuse of KonturConfig.readyPollInterval
// (2s default): that interval is tuned for spacing out polls across a VM's
// whole multi-minute boot, far coarser than the sub-second docker
// networking race waitForPortHealthy actually needs to ride out.
const healthPortDialTimeout = 500 * time.Millisecond

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
