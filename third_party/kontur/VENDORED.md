# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur)'s
`main` branch, pulled through the grain git proxy at commit
`5ed4e0017f5337bc3fde3ab8c29ef42dd1dac848` (2026-08-31). Always vendor
from `main`: grain is kontur's primary consumer, so a change grain needs
belongs on kontur's `main` and reaches here by a resync, not by being
applied to this copy (see "Local patches" below). It was first
vendored at commit `a13a8cc` (2026-08-28, bwsalmon/agents#351), then
partially re-synced to `3cf4f9286402753add8390302cfb7c1fa82e4f81`
(2026-08-30, bwsalmon/agents#477, three files only), fully re-synced to
`71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f` (2026-08-30,
bwsalmon/agents#504, alpine-base support and a native-Go rewrite of
`internal/netshim/setup.go`), fully re-synced again to
`486a8c9a43f4dba1cee5d15fb25cb6068df5c966` (2026-08-30,
bwsalmon/agents#510, a writable qcow2-overlay disk and `kontur exec`),
fully re-synced to `57bf95d223edc839ccfa0447a051024fe88229d9` (2026-08-30,
bwsalmon/agents#534, memory hotplug and a mem-agent), and fully re-synced
again to `5a63863262e9cfb0a5544f36f4e66d247c4058e5` (2026-08-30,
bwsalmon/agents#562, CPU hotplug and a baked-in guest kernel).

## This resync: the local patches went upstream, and the hook landed

**This snapshot has no local patches.** Every file here is byte-for-byte
upstream (`diff -r` against a fresh checkout reports only this file), for
the first time since bwsalmon/agents#478. That is the whole point of this
pass: the three fixes that had been living only in this copy were written
against `bwsalmon/kontur` itself, and the build-time guest setup hook
grain needed was landed there rather than carried here as a fourth.

`5ed4e00` is the merge of bwsalmon/kontur#28 into that repo's `main`, so
a fresh checkout of kontur `main` now has all four commits below and
needs no patching to boot the disks this repo points it at.

The four commits this snapshot adds over `5a63863`:

- `84f683d` -- **`image_type` on every `--disk`** (was this repo's
  `internal/hypervisor/args.go` local patch, bwsalmon/agents#478 and
  #510): `image_type=raw` for raw images, `image_type=qcow2,backing_files=on`
  for the writable overlay, distinguished by the path's suffix. Without
  the first, cloud-hypervisor refuses the first write to sector 0 and the
  guest's root-filesystem mount fails; without the second, it refuses to
  open the overlay at all ("Maximum disk nesting depth exceeded").
- `1c7ac13` -- **the writable overlay's backing format pinned to `raw`**
  (was the `internal/staticpod/qcow2.go` local patch,
  bwsalmon/agents#558), together with the 8 bytes of `header_length`
  growth that reserves `compression_type` and its padding so the
  extension does not land on offset 104. Also brings the `qemu-img`-based
  regression test (`qcow2_test.go`) upstream, which skips itself when
  `qemu-img` isn't on `PATH`.
- `694b3d1` -- **`dockervm.Create` made idempotent** (was the
  `internal/dockervm/docker.go` local patch): it force-removes leftover
  containers under the VM and netns-holder names before creating new
  ones, so a `Create` interrupted between `docker run -d` and konturctl
  persisting that success no longer poisons the (deterministic, reused)
  VM name slot for every later attempt.
- `c21bace` -- **the build-time guest setup hook**, the thing
  `packer/kontur/` was blocked on. A new `GUEST_SETUP_SCRIPT` build arg
  holds a shell script's own text (like `GUEST_SSH_AUTHORIZED_KEY` holds
  a key rather than a filename), run inside the guest rootfs at build
  time by a new `guest-customized` stage. That stage promotes the rootfs
  to an image of its own (`FROM scratch`, `COPY --from=guest-rootfs
  /rootfs/ /`) so the script runs as an ordinary `RUN` -- real `/proc`,
  real `/dev`, working network, rootfs as `/` -- rather than under
  `chroot`, which would have needed `CAP_SYS_ADMIN` an ordinary
  `docker build` does not have and cost kontur the "no extra privileges"
  property its `guest-image` stage relies on for `mke2fs -d`.
  `guest-image` packs that stage instead of the raw rootfs, and a new
  `guest-artifacts` target exports `disk.img` plus the guest's own
  `vmlinuz`/`initrd.img` when a setup script installed a kernel package.
  `final` is still the last stage, so the default build target is
  unchanged.

`packer/kontur/guest-setup.sh` is the script written to be handed to that
hook; `packer/kontur/README.md` has the wiring still to do and what the
hook has not been verified against.

`go build ./...`, `go vet ./...` and `go test ./...` all pass against this
snapshot. The Dockerfile change is **not** verified by a real build: no
image registry is reachable from this environment (the egress policy
denies Docker's blob CDN), so `docker build` cannot resolve a single base
image here. `packer/kontur/README.md`'s "Not yet verified" section lists
what to check on a machine that can build it.

## Why this copy exists

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

## Local patches

None. Keep it that way: grain is kontur's primary consumer, so "upstream
wouldn't want this" is rarely true here, and a fix made against this copy
has to be re-diffed and re-applied by hand on every resync. Two of the
three just retired (`qcow2.go`, `docker.go`) went undocumented in this
file for a resync or more precisely because they were made against this
copy directly. Landing a change on `bwsalmon/kontur`'s `main` and
re-vendoring from it costs a round trip and nothing else.

If one is genuinely unavoidable -- something specific to how this repo
drives cloud-hypervisor and wrong for upstream -- record it here with
what breaks without it and how that was confirmed, so the next wholesale
copy notices it rather than silently dropping it.
