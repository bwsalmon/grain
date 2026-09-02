# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy. Always vendor from `main`: grain is
kontur's primary consumer, so a change grain needs belongs on kontur's
`main` and reaches here by a resync, not by being applied to this copy
(see "Local patches" below).

This snapshot is kontur's `main` at
`384e91a750402a31e5b733ae805617cb30181683` (2026-09-02), the merge of
[bwsalmon/kontur#35](https://github.com/bwsalmon/kontur/pull/35). It was
briefly vendored from that PR's branch while it was open, since the grain
changes alongside it cannot build or run without it; re-vendoring from
`main` after the merge produced a byte-identical tree, verified with
`diff -r` against a fresh checkout.

The snapshot history: it was first vendored at commit `a13a8cc` (2026-08-28, bwsalmon/agents#351), then
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
bwsalmon/agents#562, CPU hotplug and a baked-in guest kernel), fully
re-synced to `5ed4e0017f5337bc3fde3ab8c29ef42dd1dac848` (2026-08-31, the
three local patches going upstream plus the build-time guest setup hook),
and fully re-synced to `9a43152b09807814ba1a364fab313a72183f9bac`
(2026-08-31, flat networking mode and CI, bwsalmon/kontur#29 and #30).

## This resync: a per-boot guest exec keypair

Everything between `63b5def` (the previous snapshot) and this branch,
which is upstream `33fe6af` plus
[bwsalmon/kontur#35](https://github.com/bwsalmon/kontur/pull/35).

#35 replaces the keypair kontur's Dockerfile used to bake into the image
-- public half in the guest rootfs, private half in the runtime image --
with one `kontur run` generates per boot and hands the guest on the
kernel command line. That keypair was the reason a grain deployment
could not pull a published guest disk: `packer/kontur/guest-setup.sh`
had to bake *this* deployment's own SSH key in at build time, so there
was no such thing as a generic published guest image. With the key
arriving at boot there is nothing deployment-specific left in the disk,
and `ensure_kontur_guest_build` can become a pull.

It also removes a trap grain had already worked around: the baked
keypair's two halves only matched when the guest image and the runtime
image came out of the same `docker build`, which is why grain managed a
keypair of its own (`ensure_kontur_ssh_key`, `-kontur-exec-key`) instead
of using kontur's. That is all deleted here.

`konturctl vm create -guest-user` is the one new input: kontur authorizes
root, and grain execs as `debian`, so the account has to be named. It
sets `KONTUR_EXEC_USER` on the VM container, where `kontur run` reads it
to authorize the account and `kontur exec` reads it to log in as.

## Previous resync: mask udev's NIC rename for the flat-mode control link

`63b5def` is the merge of a single upstream commit, `6d69560`, into
`main`. **This snapshot still has no local patches** -- every file here is
byte-for-byte upstream, verified with `diff -r` against a fresh checkout
(which reports only this file). Keep it that way; see "Local patches"
below. Only `Dockerfile` and `README.md` changed; no Go source is touched
by this resync, so `go build ./...`, `go vet ./...` and `go test ./...`
are unaffected and remain passing as they were at `9a43152`.

`6d69560` fixes a real bug found by testing kontur end to end against a
real docker daemon and real KVM (a GCE VM with nested virtualization
enabled): flat mode's guest-side control link never came up, so `kontur
exec` timed out and `kontur-mem-agent` never got a target. The guest's
second NIC boots as `eth1`, which is what `kontur-control-net`'s default
`KONTUR_CONTROL_IFACE` (and, for the first NIC, the flat-mode `ip=`
kernel parameter) assume -- but systemd-udevd's stock
`80-net-setup-link.rules` renames it to a PCI-slot-based name (`ens3`)
before `kontur-control-net.service` runs, so its `-e /sys/class/net/eth1`
check silently found nothing and exited 0 without ever configuring an
address. The first NIC was unaffected only because the kernel's own `ip=`
autoconfiguration applies before udev renames it, and the rename doesn't
clear an address once set. Fixed in the Dockerfile's Debian guest-rootfs
stage by masking `80-net-setup-link.rules` (a `/dev/null` symlink under
`/etc/udev/rules.d`, which overrides the same filename under
`/lib/udev/rules.d`), so every NIC keeps the kernel's own enumeration
order instead of being renamed. Alpine's guest isn't affected -- OpenRC
plus mdev doesn't do udev's predictable-naming renaming in the first
place.

`packer/kontur/build-guest.sh` builds this repo's sandbox guest on top of
kontur's own Dockerfile stages, so this class of bug -- systemd renaming a
NIC before the service that configures it can find it -- is one grain
already hit and fixed independently: `packer/kontur/guest-setup.sh`'s
`GUEST_SETUP_SCRIPT` hook has masked `/etc/systemd/network/99-default.link`
since the previous resync (`9a43152`), for exactly the same symptom on
exactly the same NIC. That mask runs on top of kontur's
guest-rootfs stage and disables the same NamePolicy-driven renaming this
upstream fix masks a layer lower (`80-net-setup-link.rules` itself), so
this resync does not change grain's own guest image's behavior -- the two
fixes overlap rather than stack. It does fix kontur's *own* bundled
default guest disk image (the one a plain `docker run kontur` boots
without `packer/kontur` in the picture at all), which had no such
workaround.

## Why this copy exists

`bwsalmon/kontur` is private, and an ordinary dispatched task's sandbox
has no route to it -- the proxy is default-deny per repo
(`grain/proxy/allowlist.py`), and prior tasks (bwsalmon/agents#267, #274)
found it unreachable for exactly that reason. This copy exists so that
limitation stops blocking work in this repo: a task that needs to confirm
`kontur`/`konturctl`'s actual CLI flags, its VM state file shape, or
anything else about how it behaves can read the source here (e.g.
`internal/cli/vm.go`) instead.

It is a plain copy, not a git submodule or subtree. Nothing under `v2/`
depends on it as *code* -- `v2/go.mod` does not require it and nothing
imports `github.com/bwsalmon/kontur`. Its Dockerfile, however, is built:
`packer/kontur/build-oci-image.sh` builds the runtime image from this
directory, and `packer/kontur/build-guest.sh` builds the sandbox guest
from the same Dockerfile's `guest-artifacts` target. So a resync here
changes what a deployment actually runs, not just what a task can read.

The Go dependency is still deliberately absent, and `v2/pkg/kontur`'s own
package doc comment explains why: reading kontur's on-disk state and
shelling out to its CLI is a shallower dependency than importing its
module graph would be, and that reasoning doesn't change just because the
source is now sitting in this repo too. As *documentation* this copy can
still go stale -- there is no automation re-pulling it -- so anything
safety- or correctness-critical should be confirmed against a live
`kontur vm create -h` (or a fresh vendor) rather than assumed current.

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
