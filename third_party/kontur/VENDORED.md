# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit `71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f`
(2026-08-30) for bwsalmon/agents#504. It was first vendored at commit
`a13a8cc` (2026-08-28, bwsalmon/agents#351), then partially re-synced to
`3cf4f9286402753add8390302cfb7c1fa82e4f81` (2026-08-30, bwsalmon/agents#477,
three files only), and is now fully re-synced here (every file, a plain
recursive copy of upstream `main`) to pick up five more commits made
since: alpine-base support for the outer `kontur` image and guest image
(`398491d`/`f772d71`, bwsalmon/agents#497/#498) and a rewrite of
`internal/netshim/setup.go` to use native Go networking instead of
shelling out to `ip`/`iptables` (`223f574`, bwsalmon/agents#499). None of
this touches anything `packer/kontur/` or `v2/pkg/kontur` depend on --
confirmed by re-diffing `internal/hypervisor/args.go` (the one file with
a local patch, see below) against upstream at both the old and new vendor
points: identical at both, so the patch below is the only diff from
upstream in this whole tree.

Upstream has moved further since (`9ec7b08`/`f717531`, commit
`40b58af2e8859347ea3f21b6d17368a59222f8d1` at time of writing) with two
more real, hands-on-confirmed fixes -- a missing `udev` package stalling
guest boot, and a `-disk-readonly=false`/`-disk-hostpath` mechanism
letting `konturctl vm create` give a VM a genuinely persistent writable
disk (`-images-hostpath` itself is always read-only, so without an
explicit opt-in a VM's own root filesystem could not reliably persist a
write at all). That re-sync was attempted as part of this same change and
reverted: rebuilding `konturctl` from that commit and running the exact
same real, nested-virtualization-backed boot this repo's own
`TestKonturSandboxesToolsForAgainstARealDockerBackedVM` already exercises
against the *previous* vendor point hung indefinitely inside `konturctl
vm create` itself (confirmed via a goroutine dump: blocked in
`os/exec.(*Cmd).Wait`, past image build/pull, never returning within an
8-minute test timeout) -- with no further explanation surfaced before
reverting. This snapshot is therefore deliberately held at
`71e277a`, the last commit confirmed (by this same real test) to actually
boot a guest and complete a real SSH-backed `run_command` end to end,
rather than advanced to a commit that has not been. A future task picking
up `-disk-readonly`/`-disk-hostpath` (worth having: see the reasoning
above) needs to root-cause that hang first, ideally against a fresh
vendor pull tested the same way.

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
