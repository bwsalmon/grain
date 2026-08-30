# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit `3cf4f9286402753add8390302cfb7c1fa82e4f81`
(2026-08-30) for bwsalmon/agents#477. It was first vendored at commit
`a13a8cc` (2026-08-28, bwsalmon/agents#351) and re-synced here to pick up
one upstream commit made since: `85dba5a` ("Add GUEST_SETUP_SCRIPT build
arg for custom guest image setup", bwsalmon/agents#433), which
`packer/kontur/provision.sh`'s own `SANDBOX_SETUP_SCRIPT` (bwsalmon/agents#477)
takes as its precedent -- see that script's own comment on the section it
adds. Only the three files that commit touched (`Dockerfile`, `README.md`,
`deploy/guest-image/README.md`) were re-copied; nothing else in this
snapshot has been re-diffed against upstream. `bwsalmon/kontur` is private, and an
ordinary dispatched task's sandbox has no route to it -- the proxy is
default-deny per repo (`grain/proxy/allowlist.py`), and prior tasks
(bwsalmon/agents#267, #274) found it unreachable for exactly that reason.
This copy exists so that limitation stops blocking work in this repo: a
task that needs to confirm `kontur`/`konturctl`'s actual CLI flags, its VM
state file shape, or anything else about how it behaves can read the
source here (e.g. `internal/cli/vm.go`) instead.

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

## One local patch: `image_type=raw` on every disk (bwsalmon/agents#478)

Unlike every other file in this snapshot, `internal/hypervisor/args.go` no
longer matches upstream verbatim: its `BuildArgs` now appends
`,image_type=raw` to every `--disk` argument it builds. Confirmed by hand
against a real `cloud-hypervisor` v53.0 binary under real KVM: without it,
`cloud-hypervisor`'s own disk-format auto-detection refuses the first
write to sector 0 ("Attempting to write to sector 0 on a disk without
specifying image_type"), which fails the guest's root-filesystem mount
before it ever gets to userspace -- confirmed against the exact same disk
image booting cleanly once the flag is added. Every disk this runtime is
ever pointed at is a plain raw image (`config.go`'s own
`defaultDiskImage`, and every guest `packer/kontur/build.sh` produces), so
there is no real tradeoff to gate this behind a new flag over -- see the
patched file's own comment for the full reasoning.

A deployment building `kontur`/`konturctl` from a *fresh*, unpatched
checkout of bwsalmon/kontur (rather than this vendored copy) needs the
same one-line change to `internal/hypervisor/args.go` until it lands
upstream, or any `-disk` pointed at a raw image will fail to boot the same
way.
