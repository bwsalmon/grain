# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy. Always vendor from `main`: grain is
kontur's primary consumer, so a change grain needs belongs on kontur's
`main` and reaches here by a resync, not by being applied to this copy
(see "Local patches" below).

This snapshot is kontur's `main` at
`e2b8b4506babe9c787f6b3943d8a20cfd549eeb1` (2026-09-03), the merge of
[bwsalmon/kontur#37](https://github.com/bwsalmon/kontur/pull/37) on top of
[#36](https://github.com/bwsalmon/kontur/pull/36).

The guest base image grain builds on comes from this same commit:
`ghcr.io/bwsalmon/kontur:debian12-e2b8b4506babe9c787f6b3943d8a20cfd549eeb1`,
published by kontur's own CI for exactly this SHA. That pairing is not a
convention, it is the invariant: `konturctl` is built from the tree here
and talks to a guest built from that image, and the two agree on the
guest-side contract (`kontur-authorized-key`, the control-net overlay,
the mem-agent, the disk modes) only because they are the same commit.
Re-vendoring means moving both, and the SHA appearing in two places is
what makes a mismatch visible rather than a runtime mystery.

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

## This resync: container-capable guests, built by booting one

Two changes, both driven by grain needing a guest that runs docker and
`kind` and a deployment that pulls one rather than building it.

**#36 — a guest can bring its own kernel, and be derived from a
published image.** `GUEST_KERNEL_PACKAGE` installs a distro kernel into
the guest rootfs, in the stage where the rootfs is a running container
rather than during `debootstrap`, so the initramfs its postinst
generates already carries kontur's udev mask -- an initramfs predating
that mask renames the NIC away from `eth0` before the real root mounts,
and konturctl's hard-coded `ip=...:eth0:off` then names a device that
does not exist. `kontur-net-cmdline.service` hands `ip=` to klibc's
`ipconfig` for kernels without `CONFIG_IP_PNP` (Debian's has none), and
no-ops under the bundled kernel. `final` carries the guest's own
vmlinuz/initrd and boots them when both are present. `GUEST_CONSOLE_WRAP=0`
drops the sshd `ForceCommand` that ran every session under a pty --
which grain's own `guest-setup.sh` used to have to undo, because a pty
rewrites newlines as CRLF and merges stderr into stdout, corrupting
every `read_file` and emptying every error.

`konturctl guest build` boots a guest image, runs a setup script inside
it, scrubs per-boot identity (machine-id, host keys, random seed, DHCP
leases, logs), powers it off and commits the container. That is how
grain's guest is now built: `FROM` a published kontur image, provision,
commit -- an ordinary OCI image that is also the sandbox container.

**#37 — the writable disk moved into the VM's own container.** A VM's
overlay was a qcow2 konturctl created on the *host* and bind-mounted in,
which is why a writable `-disk` had to live under `-images-hostpath` and
why `-disk-hostpath` existed. Neither works for a guest that travels
inside an image: there is no host path, so it could only be read-only,
and making it writable would have overlayfs copy the whole multi-gigabyte
image up on the guest's first write, on every VM start.
`CHV_DISK_MODE=overlay|persistent|readonly` replaces the boolean, with
`overlay` the default. What this deletes on grain's side is the whole
`-images-hostpath`/`-disk-hostpath` apparatus in `scripts/setup.sh`.

Note the two deprecated spellings map to *different* modes on purpose:
`CHV_DISK_READONLY=false` (read inside the container) always meant
writing through to the image, so it maps to `persistent`; konturctl's
own `-disk-readonly=false` meant "a private writable disk for this VM",
so it maps to `overlay`. grain passes the latter.

## The previous resync: a per-boot guest exec keypair

Everything between `63b5def` (the previous snapshot) and this branch,
which is upstream `33fe6af` plus
[bwsalmon/kontur#35](https://github.com/bwsalmon/kontur/pull/35).

#35 replaces the keypair kontur's Dockerfile used to bake into the image
-- public half in the guest rootfs, private half in the runtime image --
with one `kontur run` generates per boot and hands the guest on the
kernel command line. That keypair was the reason a grain deployment
could not pull a published guest disk: `scripts/kontur/guest-setup.sh`
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

`scripts/kontur/build-guest.sh` builds this repo's sandbox guest on top of
kontur's own Dockerfile stages, so this class of bug -- systemd renaming a
NIC before the service that configures it can find it -- is one grain
already hit and fixed independently: `scripts/kontur/guest-setup.sh`'s
`GUEST_SETUP_SCRIPT` hook has masked `/etc/systemd/network/99-default.link`
since the previous resync (`9a43152`), for exactly the same symptom on
exactly the same NIC. That mask runs on top of kontur's
guest-rootfs stage and disables the same NamePolicy-driven renaming this
upstream fix masks a layer lower (`80-net-setup-link.rules` itself), so
this resync does not change grain's own guest image's behavior -- the two
fixes overlap rather than stack. It does fix kontur's *own* bundled
default guest disk image (the one a plain `docker run kontur` boots
without `scripts/kontur` in the picture at all), which had no such
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

It is a plain copy, not a git submodule or subtree. Nothing under this repository's own Go tree
depends on it as *code* -- `go.mod` does not require it and nothing
imports `github.com/bwsalmon/kontur`. Its Dockerfile, however, is built:
`scripts/kontur/build-oci-image.sh` builds the runtime image from this
directory, and `scripts/kontur/build-guest.sh` builds the sandbox guest
from the same Dockerfile's `guest-artifacts` target. So a resync here
changes what a deployment actually runs, not just what a task can read.

The Go dependency is still deliberately absent, and `pkg/kontur`'s own
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
