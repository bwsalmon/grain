# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit
`486a8c9a43f4dba1cee5d15fb25cb6068df5c966` (2026-08-30) for
bwsalmon/agents#510. It was first vendored at commit `a13a8cc` (2026-08-28,
bwsalmon/agents#351), then partially re-synced to
`3cf4f9286402753add8390302cfb7c1fa82e4f81` (2026-08-30, bwsalmon/agents#477,
three files only), fully re-synced to `71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f`
(2026-08-30, bwsalmon/agents#504, five commits: alpine-base support and a
native-Go rewrite of `internal/netshim/setup.go`), and is now fully
re-synced again here (every file, a plain recursive copy of upstream
`main`) to pick up six more commits made since:

- `9ec7b08`/`33a68c1` -- a real, hands-on validation pass that fixed a
  missing `udev` package stalling guest console boot, gave netshim's init
  container `privileged: true` under a real standalone kubelet (a real CRI
  runtime turned out to mask `/proc/sys/net` read-only the same way plain
  `docker run` already did, contradicting an earlier, untested assumption
  this same doc used to repeat), and added an upfront `vm create`/`vm
  update` check for VM names that would produce a `tap-<name>` interface
  name over Linux's 15-character limit.
- `f717531`/`33a68c1` -- the first cut of a genuinely writable
  `konturctl`-managed VM disk: a new `-disk-hostpath` flag, and
  `-disk-readonly=false` making `konturctl` copy `-disk` out of the
  shared, always-read-only `-images-hostpath` into a private per-VM copy
  before the VM starts.
- `581540a`/`63e80c2` -- replaced that copy with a small qcow2 overlay
  file (`internal/staticpod/qcow2.go`, a from-scratch encoder -- neither
  `konturctl` nor the kontur image ships `qemu-img`) backed by the shared
  source image, so preparing a writable disk costs a fixed ~256KiB
  regardless of the source image's size instead of scaling with it.
- `fe92b6b`/`63e80c2` -- added `kontur exec` (`internal/guestexec`), a
  fourth `cmd/kontur` mode that execs a command inside the VM guest over
  SSH rather than the (otherwise empty) container around it, meant to be
  wired up as what `kubectl exec` itself runs.

None of this touches anything `packer/kontur/` or `v2/pkg/kontur` depend
on -- confirmed by re-diffing `internal/hypervisor/args.go` (the one file
with a local patch, see below) against upstream at this vendor point: it
carries the same local additions it already did at the previous vendor
point, plus one more this same pass found necessary (see below), and
nothing upstream itself changed in that file across this whole range.

## The `konturctl vm create` hang: root-caused, not reproducible at HEAD

bwsalmon/agents#504 stopped at `71e277a` after attempting this same
re-sync and hitting a real, reproducible-seeming hang: rebuilding
`konturctl` from `40b58af2e8859347ea3f21b6d17368a59222f8d1` (the commit
this doc used to describe as the resync target, three commits behind
where this doc now stops) and re-running this repo's own
`TestKonturSandboxesToolsForAgainstARealDockerBackedVM` against it hung
indefinitely inside `konturctl vm create` itself, confirmed via a
goroutine dump blocked in `os/exec.(*Cmd).Wait`, past image build/pull,
never returning within an 8-minute test timeout.

bwsalmon/agents#510 (this pass) re-ran that exact same test, against
that exact same commit (`40b58af`), on a fresh sandbox with real
`/dev/kvm`, a real Docker daemon, and passwordless `sudo` for
`debootstrap`/`mke2fs` -- the same real, nested-virtualization-backed
path the original hang was found on, not a shortcut around it. It passed
cleanly twice in a row (84s and 104s), with no code changes of any kind
to `40b58af`'s own tree. It was then re-run three more times against the
fully re-synced `486a8c9` this doc now vendors, also cleanly (75-105s
each). Across two hosts and both the reportedly-hanging commit and six
commits past it, `konturctl vm create` never hung once, and no change
between `40b58af` and `486a8c9` touches the code path a default
(`-disk-readonly` not passed, so `true`) `vm create` actually exercises
under the docker backend: `staticpod.PrepareWritableDisk` -- the
function `f717531`/`581540a` added -- returns immediately, before
touching the filesystem at all, whenever `DiskReadOnly` is true (its
default), and `internal/dockervm/docker.go`'s own diff across that range
is a no-op for a read-only disk (`spec.ResolvedDiskImage()` returns
`spec.DiskImage` unchanged, and the new `-v .../disk:/disk` mount is only
appended `if !spec.DiskReadOnly`).

Put together, this doesn't look like a real, reproducible defect in
kontur at `40b58af` (or anywhere in this range) -- it looks like a
one-off flake in bwsalmon/agents#504's own sandbox at the time: a cold
first run of a CPU/disk-heavy nested-KVM workload (guest debootstrap,
Docker image build, and cloud-hypervisor boot all competing for the same
host) is exactly the kind of thing that can stall far outside its normal
envelope without there being a bug in the code actually running. Nothing
here rules out some other, still-unknown host-specific cause for that one
run, but there is no reproduction to root-cause further, and re-running
the exact same test against the exact same commit here found nothing
wrong -- so this re-sync proceeds past `40b58af` rather than holding
there again on the strength of a single unreproduced run.

That said, this pass did find a real, reproducible bug on the
`-disk-readonly=false` path specifically -- see the local patch section
below -- caught only because it was validated against a real VM (a manual
`konturctl vm create -disk-readonly=false ...` against the exact backend
and image this repo's own real test uses) before wiring it into
`v2/scripts/setup.sh`, not assumed to work from the upstream commit
message alone.

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

## One local patch: `internal/hypervisor/args.go` (bwsalmon/agents#478, #510)

Unlike every other file in this snapshot, `internal/hypervisor/args.go` no
longer matches upstream verbatim. It has two local additions, both in the
same `--disk` argument-building loop, kept in one file since they are
both about the same underlying problem: cloud-hypervisor needing to be
told a disk's actual format rather than trusting its own auto-detection.

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
  (`581540a` upstream, see the resync notes above), always named
  `disk.qcow2`. Added and confirmed by hand during this same resync pass
  (bwsalmon/agents#510): forcing `image_type=raw` on that disk too (the
  unconditional behavior before this pass) made cloud-hypervisor refuse
  to open it at all ("Maximum disk nesting depth exceeded"). Fixing the
  `image_type` alone was not enough -- cloud-hypervisor v53.0 also needs
  `backing_files=on` before it will follow a qcow2 file's backing-file
  chain at all (undocumented beyond being listed as a `--disk` option in
  its own `--help`); without it, having a backing file at all counts as
  more "nesting" than the default budget allows, regardless of
  `image_type`. Confirmed end to end with both set: a real
  `-disk-readonly=false` VM (built from this vendored tree, same guest
  image and OCI image `TestKonturSandboxesToolsForAgainstARealDockerBackedVM`
  itself builds) boots, SSHes in, and a write made through the guest
  lands in the overlay (its file size growing from a stock ~256KiB) while
  the shared, always-read-only source image underneath is untouched.
  Without this half of the patch, wiring `-disk-readonly=false` into any
  real deployment (as `v2/scripts/setup.sh` now does, see below) would
  make every kontur VM fail to boot outright.

A deployment building `kontur`/`konturctl` from a *fresh*, unpatched
checkout of bwsalmon/kontur (rather than this vendored copy) needs the
same change to `internal/hypervisor/args.go` until it lands upstream, or
any `-disk` pointed at a raw image will fail to boot, and any
`-disk-readonly=false` VM's writable overlay will fail to open at all.
