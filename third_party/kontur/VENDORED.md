# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit
`5a63863262e9cfb0a5544f36f4e66d247c4058e5` (2026-08-30) for
bwsalmon/agents#562. It was first vendored at commit `a13a8cc` (2026-08-28,
bwsalmon/agents#351), then partially re-synced to
`3cf4f9286402753add8390302cfb7c1fa82e4f81` (2026-08-30, bwsalmon/agents#477,
three files only), fully re-synced to `71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f`
(2026-08-30, bwsalmon/agents#504, alpine-base support and a native-Go
rewrite of `internal/netshim/setup.go`), fully re-synced again to
`486a8c9a43f4dba1cee5d15fb25cb6068df5c966` (2026-08-30, bwsalmon/agents#510,
a writable qcow2-overlay disk and `kontur exec`), fully re-synced once more
to `57bf95d223edc839ccfa0447a051024fe88229d9` (2026-08-30,
bwsalmon/agents#534, memory hotplug and a mem-agent), and is now fully
re-synced again (every file, a plain recursive copy of upstream `main`) to
pick up two more commits made since:

- `c6d74d5` -- **adds CPU hotplug**, mirroring the existing memory hotplug:
  a new `CHV_CPUS_MAX` env var / `Config.CPUsMax` (defaulting to `CHV_CPUS`
  itself, i.e. no headroom unless explicitly opted into) lets
  `hypervisor.BuildArgs` emit `--cpus boot=N,max=M`, and `kontur resize`
  gained a `-cpus` flag (`APIClient.ResizeCPUs`, the same `vm.resize`
  endpoint memory resize already uses) alongside the existing `-memory-mb`.
  Refactors the `--cpus` line into a new `cpusArg` helper the same way
  `memoryArg` was factored out for memory hotplug -- see "Local patches"
  below for why that mattered here.
- `dd6e306` -- **bakes the guest kernel into the kontur image**, closing
  the last gap in "self-contained": the image already bundled a reference
  guest disk image, but the direct-kernel-boot kernel that disk needs
  still had to come from outside the image every time (a hostPath, a PVC,
  or an init container fetching one at pod start). A new `fetch-kernel`
  Dockerfile stage pins and downloads a `cloud-hypervisor/linux` release
  (`KONTUR_KERNEL_VERSION`) into `/var/lib/kontur/guest/vmlinux`, and
  `internal/config.FromEnv` now defaults `CHV_KERNEL` to that path
  whenever neither `CHV_KERNEL` nor `CHV_FIRMWARE` is set -- `docker run
  kontur` or a bare k8s pod with no other flags now boots a working VM.
  `internal/staticpod/spec.go`'s `Validate` had its auto-derived
  `CHV_CMDLINE` condition changed from "a Kernel was given" to "Firmware
  is unset", since a `konturctl`-created VM with no explicit `-kernel` now
  still boots via direct kernel boot (the image's own default) rather than
  firmware, and still needs that cmdline.
  `deploy/k8s/gke-pod-example.yaml`'s fetch-kernel init container is
  dropped entirely as a result.

Neither of these touches anything `v2/pkg/orchestrator.KonturSandboxes` (or
anything else under `v2/`) actually drives: it only ever sets `CHV_CPUS`/
`CHV_MEMORY_MB` (and, via `staticpod.VMSpec`, an explicit `Kernel`/`Disks`)
through `internal/cli/vm.go`'s existing flags and `internal/dockervm`'s
`docker run` env, never `CHV_CPUS_MAX` or an unset `CHV_KERNEL` -- so a VM
created here keeps a fixed vCPU count (no hotplug headroom) and boots the
kernel this repo's own `packer/kontur` build supplies, exactly as before
this range existed.

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

## Local patches

Three files in this snapshot no longer match upstream verbatim -- fixes
made directly against this vendored copy rather than pulled from
`bwsalmon/kontur`, either because upstream doesn't need them (the disk
`image_type` hints below are specific to how this repo drives
cloud-hypervisor) or because they simply hadn't been upstreamed yet at the
time. This resync (bwsalmon/agents#562) re-diffed all three against the
new vendor point and found none of upstream's changes in this range touch
the same lines, so all three carried forward with no merge conflicts; each
is called out below so a future resync knows to check again rather than
silently losing one to a wholesale copy.

### `internal/hypervisor/args.go` (bwsalmon/agents#478, #510)

Two local additions, both in the same `--disk` argument-building loop,
kept in one file since they are both about the same underlying problem:
cloud-hypervisor needing to be told a disk's actual format rather than
trusting its own auto-detection.

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

This resync's own upstream range (`c6d74d5`) refactored the *adjacent*
`--cpus` line into a `cpusArg` helper but never touched the `--disk` loop
itself, so both halves of the patch carried forward unchanged.

### `internal/staticpod/qcow2.go` (bwsalmon/agents#558)

Pins the writable-overlay qcow2 header's backing-file-format extension to
`"raw"` instead of leaving it unset. `writeQcow2Overlay` hand-writes its
qcow2 headers rather than shelling out to `qemu-img`, and previously left
the backing format unpinned on the assumption that qcow2 readers probe it
from the backing file's own content, same as qcow2 v2 always did --
cloud-hypervisor's reader doesn't, and instead tripped its own
backing-chain-depth guard ("Maximum disk nesting depth exceeded") against
a raw backing file (which every overlay here is backed by -- see
`PrepareWritableDisk`), making every `-disk-readonly=false` VM fail to
boot. Fixing this needed two things together: the extension itself (magic
`0xE2792ACA`, data `"raw"`), and growing `header_length` by 8 bytes first
to reserve the `compression_type` byte + padding every v3 reader expects
at offset 104 whenever `header_length` is greater than 104 -- an
extension placed without that reservation had its own magic byte land on
offset 104 and get misread as an unrecognized compression type. Verified
with `qemu-img info` (a wholly independent qcow2 implementation) confirming
the written backing format is genuinely pinned, not just present; the
regression test (`qcow2_test.go`) shells out to it and skips itself if
`qemu-img` isn't on `PATH`.

This local patch was not documented here when it landed (bwsalmon/agents#558
edited this file directly rather than through a resync); this pass found
it missing during a diff against the previous documented vendor point and
is recording it now, alongside reconfirming it still applies cleanly.

### `internal/dockervm/docker.go`

`Create` force-removes any pre-existing containers under the VM name and
netns-holder name before creating new ones, the same way `Delete` already
tolerates "already gone" containers. VM names are deterministic, reused
fixed slots ("kontur-1", "kontur-2", ...), and `konturctl` only considers
a VM to exist by reading its own local state file, not by asking docker --
so a `Create` interrupted after its `docker run -d` succeeded but before
that state was persisted (a daemon restart, a killed CLI process, a retry
racing a slow docker call) left an orphaned container that the next
`Create` for the same slot collided with ("Conflict... name already in
use"), with no automatic recovery. This reuses the existing `remove()`
helper (`docker rm -f`, tolerating "No such container").

Also not documented here when it landed, for the same reason as the
`qcow2.go` patch above; recorded now for the same reason.

A deployment building `kontur`/`konturctl` from a *fresh*, unpatched
checkout of bwsalmon/kontur (rather than this vendored copy) needs all
three of the above changes until they land upstream, or: any `-disk`
pointed at a raw image will fail to boot; any `-disk-readonly=false` VM's
writable overlay will fail to open at all (both the `args.go` and
`qcow2.go` patches contribute to this); and a docker-backend `vm create`
retry against a VM name with a leftover container from an earlier,
interrupted attempt will fail outright instead of recovering.
