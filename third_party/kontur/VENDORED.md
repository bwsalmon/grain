# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy. Always vendor from `main`: grain is
kontur's primary consumer, so a change grain needs belongs on kontur's
`main` and reaches here by a resync, not by being applied to this copy
(see "Local patches" below).

**This copy is no longer byte-for-byte upstream.** It carries two local
patches -- flat mode's default-route discovery, which every sandbox's
egress depends on, and a guest resolver, which every sandbox's DNS
depends on -- so a resync has to re-apply both until they land upstream.
"Local patches" at the bottom is the whole account.

This snapshot is kontur's `main` at
`dc9230b44c7a582b25d6e73992c6495ef0b532d3` (2026-09-03), the merge of
[bwsalmon/kontur#39](https://github.com/bwsalmon/kontur/pull/39) on top of
[#38](https://github.com/bwsalmon/kontur/pull/38),
[#37](https://github.com/bwsalmon/kontur/pull/37) and
[#36](https://github.com/bwsalmon/kontur/pull/36).

The guest base image grain builds on comes from this same tree:
`scripts/kontur/build-guest.sh` builds it out of the Dockerfile here,
with the two args kontur's CI publishes its "debian12" variant with, and
derives grain's guest from that. It is built rather than pulled for a
reason worth knowing before "just pin the published one" suggests itself
again: a GitHub Actions GITHUB_TOKEN is scoped to its own repository, so
grain's CI can push grain's packages and cannot read kontur's private
ones, whatever the login says.

Building it here is also what makes the invariant hold by construction.
`konturctl` is built from this tree and talks to a guest built from this
tree, and the two agree on the guest-side contract
(`kontur-authorized-key`, the control-net overlay, the mem-agent, the
disk modes) because they are the same files -- rather than because
someone remembered to move a pinned tag and a vendored SHA together.

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
(2026-08-31, flat networking mode and CI, bwsalmon/kontur#29 and #30),
fully re-synced to `384e91a750402a31e5b733ae805617cb30181683`
(2026-09-02, a per-boot guest exec keypair, bwsalmon/kontur#35), and
fully re-synced to `e475be0e24f4c08217bdce2e80383d4daf9a82b3`
(2026-09-03, container-capable guests built by booting one,
bwsalmon/kontur#36, #37 and #38).

## This resync: a VM can be given the disk size it needs

One upstream commit on top of the previous snapshot, `652c32c`
(bwsalmon/kontur#39), and the one this repo asked for: `konturctl vm
create -disk-size-mb`.

grain has had the setting for a while --
`model.Config.SandboxDiskGB`/`model.Task.SandboxDiskGB`, in GiB -- and
`orchestrator.KonturConfig.createArgs` had nowhere real to send it. It
sends `-disk-size-mb` now, converting at that one point. konturctl passes
it through as `CHV_DISK_SIZE_MB`, and `kontur run` sizes the qcow2
overlay the guest writes into before cloud-hypervisor opens it: a fresh
overlay is created that large, and one an earlier boot left behind is
grown in place, which allocates no clusters and leaves everything the
guest wrote where it was.

Three things it refuses rather than does, all of which would cost a guest
its filesystem: shrinking, a size below the disk image the overlay reads
through to, and being used outside `-disk-mode=overlay`, where the disk
is the shared image itself and kontur has no overlay of its own to size.
grain's VMs are in overlay mode -- `scripts/setup.sh` passes
`-disk-mode=overlay` -- so only the first two are reachable from here.

What it sizes is the block device the guest is offered. Growing the
filesystem onto it is the guest's own job, which grain's guest already
does on every boot -- `scripts/kontur/guest-setup.sh`'s `grain-growfs`
unit, `resize2fs /dev/vda`, a no-op on a VM whose disk was not enlarged.

## Previous resync: container-capable guests, built by booting one

Three changes, all driven by grain needing a guest that runs docker and
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

**#38 — a derivable guest gets room to be provisioned into.**
`GUEST_DISK_EXTRA_MB` adds headroom to the guest disk on top of the 20%
the `guest-image` stage sizes it with. That 20% is the right answer for
an image whose guest is finished at build time -- space for logs and
runtime growth, not for installing anything -- but a guest customized by
`konturctl guest build` is provisioned *after* the filesystem is packed
and cannot grow it. `scripts/kontur/build-guest.sh` installs docker,
containerd, `kind`, Go and a toolchain into that guest, which without
headroom fails on `apt-get install` with "You don't have enough free
space in `/var/cache/apt/archives/`" after a completely clean boot --
reading as a broken setup script rather than a disk sized before anyone
knew what would go on it. grain passes 2048, and the number is not
arbitrary: `truncate` leaves the extra as a hole and a layer of zeros
pushes as almost nothing, but extracting a layer materializes the hole,
so every GB here is a GB of runner disk on every pull. 8192 was tried
first and made CI flaky on identical inputs; 2048 leaves the guest about
the free space the old build-time-provisioned image had.

The same PR also fixed `konturctl guest build`'s own diagnostics, which
grain's CI depends on to say anything useful when a guest build fails:
the console excerpt now comes before the error rather than after it (a
log tail otherwise shows the guest reaching `multi-user.target` and hides
the sentence saying what went wrong), and any non-zero exit from the VM
container is reported rather than only 137.

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

Two, and both belong upstream.

### A guest with a nameserver it can reach

`deploy/guest-image/overlay-common/usr/local/libexec/kontur-configure-dns`
and its two service units, the `GUEST_DNS` build arg and the
`NETSHIM_DNS`/`-dns` plumbing behind it (`internal/netshim/config.go`,
`flat.go`, `internal/staticpod`, `internal/dockervm`, `internal/cli`),
plus their tests and the two READMEs.

*What breaks without it.* Every guest resolved through whatever
`/etc/resolv.conf` the machine that built the image happened to have --
`debootstrap` copies the build host's in, and a build host's resolver is
routinely an address that only exists in its own network namespace (on
the Azure VMs GitHub's runners are, the host-only wireserver
`168.63.129.16`; on a docker build, `127.0.0.11`). From a guest on a tap
it is simply unroutable, so a sandbox came up with completely open IP
egress and no DNS at all, and every name lookup hung until it timed out.
Nothing kontur or grain does noticed: `kontur exec` reaches a guest by
address, the git proxy and the UI are literal IPs, and the failure
surfaced only inside a dispatched task, as `apt`/`npm`/`gcloud`/`curl`
timing out -- which reads as a blocked network rather than as a missing
resolver. It also meant the image was not reproducible: whichever machine
built it contributed its own unreachable resolver to every sandbox booted
from it.

*How it was confirmed.* On a live grain sandbox guest (grain/task-195,
grain/task-200): `getent hosts github.com` returned nothing and
`curl https://github.com` timed out, while `8.8.8.8:53` and public `:443`
addresses connected immediately;
`sudo sh -c 'echo "nameserver 8.8.8.8" > /etc/resolv.conf'` restored
resolution on the spot, after which HTTPS to GitHub and to the Google
APIs both worked. `pkg/orchestrator`'s real-VM suite
(`assertGuestResolvesNames`) now asserts both halves against a guest built
from this tree.

*Getting rid of it.* Nothing here is grain-specific -- the resolver is a
setting with a public default, which is what any kontur guest wants -- so
this should land on `bwsalmon/kontur`'s `main` and come back by a resync.
Re-apply it on any resync that predates that, and check
`internal/netshim/config.go` and `deploy/guest-image/` after every one.

### Flat mode's default-route discovery

In `internal/netshim/flat.go` (`DiscoverIdentity`, `defaultGateway`,
`isDefaultDst`) plus its coverage in `flat_test.go`.

*What breaks without it.* A flat-mode guest takes over the identity the
container runtime assigned its namespace, and the last piece of that
identity is the namespace's default route -- passed to the guest as the
gateway field of the `ip=` kernel parameter `FlatGuestConfig` derives.
`DiscoverIdentity` found that route by testing `r.Dst == nil`, which no
route read back off the kernel ever satisfies: the kernel omits `RTA_DST`
on a route whose prefix length is zero, and `vishvananda/netlink` fills
that absence back in the way iproute2 does, synthesizing `0.0.0.0/0`
(`deserializeRoute`). So the gateway came out nil for every guest, the
`ip=` parameter carried an empty gateway field, and every sandbox VM
booted with a routing table that stopped at its own subnet -- no route to
the git proxy's host, a package registry, GitHub or anything else. The
guest is otherwise perfectly healthy, which is why this went unnoticed:
address, MAC and MTU are all correct, `kontur exec` reaches it over the
control link, and every tool call a run makes succeeds. Only the network
is gone. NAT mode is unaffected -- there `konturctl` fills the gateway in
itself, from the bridge CIDR it already knows (`staticpod.VMSpec`).

*How it was confirmed.* On a live grain sandbox guest: `/proc/cmdline`
carried `ip=172.17.0.4:::255.255.0.0::eth0:off` (an empty third field)
and `ip route` had no default route; re-running the guest's own
`/usr/lib/klibc/bin/ipconfig` with the gateway filled in installed
`default via 172.17.0.1 dev eth0`, after which DNS and HTTPS to the
internet both worked. `TestDiscoverIdentity_Gateway` reproduces it
without a VM, against the real kernel: it fails on the old condition and
passes on the new one.

*Getting rid of it.* This is upstream's bug, not something specific to
how grain drives kontur, so it should land on `bwsalmon/kontur`'s `main`
and come back here by a resync -- at which point this section loses an
entry. The patch is deliberately shaped to make that easy: two
small unexported helpers, one doc comment and their tests, no
grain-specific behaviour.
Until then, re-apply it on any resync that predates the upstream fix, and
check `internal/netshim/flat.go` after every resync.

Otherwise: keep this section at "None". grain is kontur's primary
consumer, so "upstream wouldn't want this" is rarely true here, and a fix
made against this copy has to be re-diffed and re-applied by hand on
every resync. Two of the three just retired (`qcow2.go`, `docker.go`)
went undocumented in this file for a resync or more precisely because
they were made against this copy directly. Landing a change on
`bwsalmon/kontur`'s `main` and re-vendoring from it costs a round trip
and nothing else.

If one is genuinely unavoidable -- something specific to how this repo
drives cloud-hypervisor and wrong for upstream -- record it here with
what breaks without it and how that was confirmed, so the next wholesale
copy notices it rather than silently dropping it.
