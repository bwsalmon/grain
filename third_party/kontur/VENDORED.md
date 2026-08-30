# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit
`57bf95d223edc839ccfa0447a051024fe88229d9` (2026-08-30) for
bwsalmon/agents#534. It was first vendored at commit `a13a8cc` (2026-08-28,
bwsalmon/agents#351), then partially re-synced to
`3cf4f9286402753add8390302cfb7c1fa82e4f81` (2026-08-30, bwsalmon/agents#477,
three files only), fully re-synced to `71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f`
(2026-08-30, bwsalmon/agents#504, alpine-base support and a native-Go
rewrite of `internal/netshim/setup.go`), fully re-synced again to
`486a8c9a43f4dba1cee5d15fb25cb6068df5c966` (2026-08-30, bwsalmon/agents#510,
a writable qcow2-overlay disk and `kontur exec`), and is now fully
re-synced once more (every file, a plain recursive copy of upstream
`main`) to pick up six more commits made since:

- `67f32f1` -- shims `/bin/sh` and `/bin/bash` through to the guest, so a
  container step that execs a shell against this image (rather than
  `kontur` itself) still finds one.
- `fc91301` -- validates kontur end to end on a real GKE cluster and adds
  a walkthrough for it; no code changes this repo's own build depends on.
- `eab7efd`/`5745172` -- adds suspend/resume for a one-time guest setup
  script and merges the two guest setup-script mechanisms that had grown
  up side by side into one.
- `d5782ae` -- **adds memory hotplug, on by default with a small starting
  size.** `kontur run`'s VM now boots small (`CHV_MEMORY_MB` defaults to
  256 MiB instead of a fixed 2048) and can grow later via
  cloud-hypervisor's virtio-mem hotplug device, up to a new
  `CHV_MEMORY_MAX_MB` ceiling (defaults to whichever is larger of 2048 or
  `CHV_MEMORY_MB` itself, so raising the starting size alone never
  produces a nonsensical "max below min"). A new `kontur resize` mode
  (`kubectl exec <pod> -c <container> -- kontur resize -memory-mb=N`)
  triggers a live grow/shrink from outside the guest, the only direction
  cloud-hypervisor's own API supports.
- `bbd1f3d` -- **adds automatic guest-driven memory hotplug ("mem-agent"),**
  the guest-initiated half `d5782ae` left manual: a host-side listener
  (`internal/memagent`, behind `CHV_MEM_AGENT`, default off) that grows a
  VM by `CHV_MEM_AGENT_STEP_MB` whenever the guest-side daemon
  (`cmd/kontur-mem-agent`, watching `/proc/pressure/memory`) reports
  pressure.

**Neither hotplug commit touches what this repo actually depends on.**
`v2/pkg/orchestrator.KonturSandboxes` (the code bwsalmon/agents#534 wires a
sandbox-shape setting into) only ever drives `konturctl vm create`/`vm
update`'s existing `-cpus`/`-memory-mb` flags (`internal/cli/vm.go`,
unchanged across this whole range) -- confirmed by diffing `vm.go` and
`internal/staticpod/spec.go`/`manifest.go` against the previous vendor
point: `staticpod.VMSpec` gained no new field for `MemoryMaxMB` or
hotplug, and both backends (`internal/staticpod/manifest.go`,
`internal/dockervm/docker.go`) still set only `CHV_MEMORY_MB` from
`VMSpec.MemoryMB`, leaving `CHV_MEMORY_MAX_MB`/`CHV_MEMORY_HOTPLUG` unset
-- which the new default computation above resolves back to
`max(MemoryMB, 2048)`, i.e. a VM sized by `-memory-mb` boots at exactly
that size, the same as before this range existed; it does not boot small
and grow into it, since `CHV_MEMORY_MB` is always given explicitly by
either backend rather than left to `kontur run`'s own small-by-default
fallback. The hotplug/mem-agent machinery is real and live for a
deployment that runs `kontur` directly (outside `konturctl`), but it is
simply unreached by anything under `v2/`.

None of this range touches anything `packer/kontur/` or `v2/pkg/kontur`
depend on either, for the same reason prior resyncs confirmed the same
thing: re-diffing `internal/hypervisor/args.go` (the one file with a
local patch, see below) against upstream at this vendor point shows
upstream's own restructuring (an early-return `--restore` branch for
`kontur resize`'s snapshot support, and a `memoryArg` helper factored out
of the `--memory` line) is additive around the local patch, not a change
to it -- the local patch's own two behaviors (forcing `image_type=raw` on
a plain disk, `image_type=qcow2,backing_files=on` on the qcow2 writable
overlay) still apply verbatim to the same `--disk` loop, just with a
`memoryArg(cfg)` call replacing the inline `fmt.Sprintf` on the line
above it that neither half of the local patch ever touched.

`bwsalmon/kontur` is private, and an ordinary dispatched task's sandbox
has no route to it -- the proxy is default-deny per repo
(`grain/proxy/allowlist.py`), and prior tasks (bwsalmon/agents#267, #274)
found it unreachable for exactly that reason. This copy exists so that
limitation stops blocking work in this repo: a task that needs to confirm
`kontur`/`konturctl`'s actual CLI flags, its VM state file shape, or
anything else about how it behaves can read the source here (e.g.
`internal/cli/vm.go`) instead.

It is a plain copy, not a git submodule or subtree, and it is not wired
into any build here -- `v2/go.mod` does not depend on it, and nothing under
`v2/` imports `github.com/bwsalmon/kontur`. `v2/pkg/kontur`'s own package
doc comment already explains why: reading kontur's on-disk state and
shelling out to its CLI is a shallower dependency than importing its
module graph would be, and that reasoning doesn't change just because the
source is now sitting in this repo too. Treat this as documentation, not a
dependency -- it can go stale (there is no automation re-pulling it), so
anything safety- or correctness-critical should still be confirmed against
a live `kontur vm create -h` (or a fresh vendor) rather than assumed
current.

`go build ./...`, `go vet ./...` and `go test ./...` all pass against this
snapshot (real-hardware tests that need `/dev/kvm`, a real Docker daemon,
or `crictl`/a standalone kubelet skip themselves the same way they always
have when that hardware isn't present -- unrelated to this resync).

## One local patch: `internal/hypervisor/args.go` (bwsalmon/agents#478, #510)

Unlike every other file in this snapshot, `internal/hypervisor/args.go`
(and its own test, `args_test.go`, kept in step with it) no longer match
upstream verbatim. It has two local additions, both in the same `--disk`
argument-building loop, kept in one file since they are both about the
same underlying problem: cloud-hypervisor needing to be told a disk's
actual format rather than trusting its own auto-detection.

- `image_type=raw` on every disk whose path does not end in `.qcow2`
  (bwsalmon/agents#478). Confirmed by hand against a real
  `cloud-hypervisor` v53.0 binary under real KVM: without it,
  `cloud-hypervisor`'s own disk-format auto-detection refuses the first
  write to sector 0 ("Attempting to write to sector 0 on a disk without
  specifying image_type"), which fails the guest's root-filesystem mount
  before it ever gets to userspace -- confirmed against the exact same
  disk image booting cleanly once the flag is added. Every disk this
  runtime was originally ever pointed at is a plain raw image
  (`config.go`'s own `defaultDiskImage`, and every guest
  `packer/kontur/build.sh` produces), so there was no real tradeoff to
  gate this behind a new flag over.

- `image_type=qcow2,backing_files=on` on a disk whose path *does* end in
  `.qcow2` -- specifically, the per-VM writable overlay
  `staticpod.PrepareWritableDisk` creates for `-disk-readonly=false`
  (`581540a` upstream), always named `disk.qcow2`. Added and confirmed by
  hand during the bwsalmon/agents#510 resync: forcing `image_type=raw` on
  that disk too (the unconditional behavior before that pass) made
  cloud-hypervisor refuse to open it at all ("Maximum disk nesting depth
  exceeded"). Fixing the `image_type` alone was not enough --
  cloud-hypervisor v53.0 also needs `backing_files=on` before it will
  follow a qcow2 file's backing-file chain at all (undocumented beyond
  being listed as a `--disk` option in its own `--help`); without it,
  having a backing file at all counts as more "nesting" than the default
  budget allows, regardless of `image_type`. Confirmed end to end with
  both set: a real `-disk-readonly=false` VM boots, SSHes in, and a write
  made through the guest lands in the overlay (its file size growing from
  a stock ~256KiB) while the shared, always-read-only source image
  underneath is untouched. Without this half of the patch, wiring
  `-disk-readonly=false` into any real deployment would make every
  kontur VM fail to boot outright.

A deployment building `kontur`/`konturctl` from a *fresh*, unpatched
checkout of bwsalmon/kontur (rather than this vendored copy) needs the
same change to `internal/hypervisor/args.go` until it lands upstream, or
any `-disk` pointed at a raw image will fail to boot, and any
`-disk-readonly=false` VM's writable overlay will fail to open at all.
