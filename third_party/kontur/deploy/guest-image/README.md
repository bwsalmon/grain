# Guest disk image

The `guest-image` stage in the top-level `Dockerfile` (packing) plus one
of the two `guest-rootfs-debian`/`guest-rootfs-alpine` stages (building
the rootfs `guest-image` packs, via the `guest-customized` stage in
between -- see "Running a custom setup script" below) build the disk
image baked into the final `kontur` image at
`/var/lib/kontur/guest/disk.img`, used automatically when a VM container
doesn't set `CHV_DISK_IMAGE` (see `internal/config`). `GUEST_DISTRO`
(default `debian`) picks which of the two rootfs stages runs:

- `debian` (default): a minimal Debian rootfs (`debootstrap
  --variant=minbase`) with `systemd-sysv` (for `/sbin/init`), `iproute2`,
  `acpid`, `udev` and `kmod`. `GUEST_SUITE` (default `bookworm`) picks the
  Debian suite. `kmod` is listed explicitly because `--variant=minbase`
  installs priority-required packages and nothing else, so there is no
  `/sbin/modprobe` -- which `kontur-agent.service` uses to load the
  virtio-vsock driver before it starts. `udev` has to be listed
  explicitly too -- `--variant=minbase` skips
  Recommends, and without `systemd-udevd` running, `dev-ttyS0.device`
  never gets marked ready, which stalls boot for
  `DefaultDeviceTimeoutSec` (systemd's default: 90s) before
  `serial-getty@ttyS0.service` gives up on it; `kontur exec` doesn't
  depend on that device unit and isn't affected, but the console would
  otherwise never show a login prompt.
- `alpine`: a minimal Alpine rootfs (`apk add --root`, the same technique
  Alpine's own official base image is built with) with `alpine-base` (for
  `busybox`/`openrc`, standing in for `systemd-sysv`), `iproute2` and
  `acpid`. `GUEST_ALPINE_VERSION` (default `3.20`) picks the Alpine
  version.

Neither carries an SSH server. `kontur exec` reaches the guest over its
virtio-vsock device instead, answered by `kontur-agent`
(`cmd/kontur-agent`, copied into both variants from the Dockerfile's own
`build` stage) -- see "Getting a shell in the guest" below, and
`internal/guestexec` for why that replaced SSH.

Both variants pack their rootfs directly into an ext4 filesystem image
with `mke2fs -d` -- no loop-mount or `chroot` needed on the build host
beyond what `debootstrap`/`apk`/`mke2fs` already do, so `docker build`
doesn't need extra privileges for either.

This is a reference/demo guest, not a production one: any real workload
should bring its own `CHV_DISK_IMAGE` (see the top-level README's
"Configuration" table and `deploy/k8s/pod-example.yaml`) rather than
relying on what's baked in here.

## What's in the overlays

Copied verbatim into the rootfs before it's packed into `disk.img`:
`overlay-common/` on both variants, then whichever of
`overlay-debian/`/`overlay-alpine/` matches `GUEST_DISTRO`.

| File | Purpose |
|---|---|
| `overlay-debian/etc/systemd/system/kontur-agent.service`, `overlay-alpine/etc/init.d/kontur-agent` | Run `kontur-agent` (copied into both variants straight from the top-level Dockerfile's own `build` stage, not part of either overlay) as a restart-always service, and `modprobe` the virtio-vsock driver first. Started in early boot and deliberately not ordered after the network -- see "Getting a shell in the guest". |
| `overlay-common/etc/acpi/events/powerbtn` | `acpid` event config matching the ACPI power button, pointing at `/etc/acpi/powerbtn.sh` -- see "Graceful shutdown" below for why that script's own contents differ per variant. |
| `overlay-debian/etc/acpi/powerbtn.sh` | Runs `systemctl poweroff`. |
| `overlay-alpine/etc/acpi/powerbtn.sh` | Runs `poweroff` (busybox's applet, which signals `/sbin/init` rather than powering off directly -- see "Graceful shutdown"). |
| `overlay-debian/etc/systemd/system/kontur-mem-agent.service`, `overlay-alpine/etc/init.d/kontur-mem-agent` | Run `kontur-mem-agent` (copied into both variants straight from the top-level Dockerfile's own `build` stage, not part of either overlay) as a restart-always service. See the top-level README's "Memory hotplug". |
| `overlay-common/usr/local/libexec/kontur-control-net` | Configures the guest's end of netshim's control link: brings up the second NIC at `169.254.100.2/24` and writes `KONTUR_MEM_AGENT_HOST` to `/run/kontur-control-net.env` for `kontur-mem-agent` to pick up. A guest with no second NIC (the control link disabled, or no netshim at all) has nothing to do and exits successfully, so the same image boots unchanged either way. See the top-level README's "Container networking". |
| `overlay-debian/etc/systemd/system/kontur-control-net.service`, `overlay-alpine/etc/init.d/kontur-control-net` | Run that script once at boot, ordered before `kontur-mem-agent` so the address it writes is in place before the agent starts. |
| `overlay-common/usr/local/libexec/kontur-configure-dns` | Writes `/etc/resolv.conf` from the nameservers on the `ip=` kernel command line (its `dns0`/`dns1` fields). Run by `kontur-configure-dns.service` (Debian) / `/etc/init.d/kontur-configure-dns` (Alpine), in early boot. A command line naming no nameservers leaves the file alone. See "The resolver". |
| `overlay-debian/usr/local/libexec/kontur-configure-net` | Configures the primary NIC from the kernel command line's `ip=` for a guest whose kernel lacks `CONFIG_IP_PNP` -- i.e. a `GUEST_KERNEL_PACKAGE` build. Exits immediately when the interface is already addressed, so it is a no-op under the bundled kernel. See "Networking". |
| `overlay-debian/etc/systemd/system/kontur-net-cmdline.service` | Runs that script after udev settles, early enough that anything needing an address has one. Debian only: it exists to compensate for a distro kernel, which is a Debian-variant option. |
| `overlay-common/usr/local/libexec/kontur-configure-routes` | Installs the routes the container runtime had on the sandbox's own interface, carried down the kernel command line as `kontur.routes=` because `ip=` can carry an address, a netmask and a gateway and nothing else. That is the whole difference between a bridge CNI, where the netmask is right, and a point-to-point one, where the subnet is deliberately not on-link. A command line with no `kontur.routes=` on it (no netshim in front of this guest, or an operator's own `ip=`) leaves the table alone and exits successfully. See the top-level README's "Container networking". |
| `overlay-debian/etc/systemd/system/kontur-net-routes.service`, `overlay-alpine/etc/init.d/kontur-net-routes` | Run that script once at boot, after the address exists and before anything uses it. |
| `overlay-common/usr/local/libexec/kontur-growfs` | Grows the root filesystem onto whatever `/dev/vda` turned out to be, so `CHV_DISK_SIZE_MB` yields free space and not just a bigger block device. See "Growing the filesystem" below. |
| `overlay-debian/etc/systemd/system/kontur-growfs.service`, `overlay-alpine/etc/init.d/kontur-growfs` | Run that script in early boot, before anything starts writing into the space it makes. |

## Enabling services

Debian's `systemd-sysv` and Alpine's `busybox`/`openrc` both need
services enabled by hand in the Dockerfile rather than through their
normal tooling (`systemctl enable`, `rc-update add`), since neither has
a running service manager during the build to ask -- each is just a
symlink, written directly:

- Debian: `kontur-mem-agent.service` and `kontur-control-net.service` are
  symlinked into `multi-user.target.wants/`. `kontur-agent.service`,
  `kontur-net-cmdline.service`, `kontur-net-routes.service` and
  `kontur-growfs.service` go into
  `sysinit.target.wants/` instead, with `DefaultDependencies=no`: the
  first answers `kontur exec` and is wanted before anything that could
  fail later in the boot has had the chance to, the second establishes
  the guest's address, the third gives it the routes that address is
  only usable off its own segment with, and the last grows the root
  filesystem, which has to happen before anything starts writing into
  the space. `acpid.socket`/
  `acpid.path` don't need the same treatment -- `acpid`'s own postinst
  already enables them via `deb-systemd-helper`, which (like `systemctl
  enable`) just writes symlinks and doesn't need a running systemd to do
  it.
- Alpine: nothing is enabled automatically by `apk add` the way Debian's
  postinst scripts do, so every service this guest needs is symlinked by
  hand from `/etc/init.d/<service>` into a runlevel directory --
  `sysinit`/`boot` for the baseline a busybox-init/OpenRC system needs to
  reach a usable state at all (device nodes, `/proc`/`/sys`, hostname,
  clearing `/tmp`, syslog -- Debian's systemd does the equivalent
  internally, with nothing to enable by hand for it) plus `kontur-growfs`,
  `kontur-net-routes` and `kontur-agent`, and `default` for
  `acpid`/`kontur-control-net`/`kontur-mem-agent` themselves. See the
  `guest-rootfs-alpine` stage in the top-level `Dockerfile` for the exact
  list.

## Getting a shell in the guest

`kontur exec` (see the top-level README's "Execing into a VM") always has
a way in, and needs nothing set up: it connects to the VM's virtio-vsock
device, where `kontur-agent` answers. There is no account to log into, no
key to authorize and no daemon to configure, because there is no login
involved -- the agent runs as root and runs each command as whichever
account the request names (`KONTUR_EXEC_USER`, defaulting to root).

That is a deliberate change from how this image used to work, and the
reason is worth knowing before adding sshd back out of habit. Exec used
to be SSH to a daemon in the guest, over netshim's control link, so
everything that brought that link up was load-bearing for being able to
get in at all: a guest whose networking had not come up was one kontur
could not ask *why*. vsock is carried by the virtio device itself, so it
works before the guest has an address, while its network is broken, and
with no NIC attached -- which is exactly when you want a shell.
`kontur-agent` is started in early boot for the same reason, ahead of
anything that could go wrong later in it.

It also means this image ships no SSH server, no host keys, no
`authorized_keys` and no per-boot keypair, all of which existed only to
serve that transport.

### If you want sshd anyway

For an operator's own interactive access, install it the way you would
install anything else in a guest -- in a setup script (see "Running a
custom setup script" below), which is also where the key to authorize
belongs:

```sh
cat > setup.sh <<'EOF'
#!/bin/sh
set -eux
apt-get update
apt-get install -y --no-install-recommends openssh-server
install -d -m0700 /root/.ssh
printf '%s\n' "ssh-ed25519 AAAA... you@example" > /root/.ssh/authorized_keys
chmod 0600 /root/.ssh/authorized_keys
EOF
konturctl guest build -from ghcr.io/bwsalmon/kontur:debian12 -setup setup.sh -t my-guest
```

Reaching it is then your own business, the same as any other service the
guest runs: the guest holds the pod's own address under flat mode, so an
sshd on it is reachable wherever that address is.

Note that a guest built this way needs its host keys scrubbed before the
image is shared, or every VM cloned from it has the same ones.
`konturctl guest build` already does this (see `internal/guestbuild`'s
own `scrub`), which is why the recipe above is the recommended route
rather than committing a running guest by hand.

## Running a custom setup script

To customize the guest beyond what the overlays above do -- installing
extra packages, dropping in config files, enabling services, etc --
there are three mechanisms, and which one to reach for is mostly a
question of *when* the customization should happen rather than what it
can do. The exception is `konturctl guest build`, below, which is the
only one that can do things a container build cannot; it is also the one
to reach for if you are deriving a guest from a published image rather
than building kontur yourself.

`GUEST_SETUP_SCRIPT` is a build arg holding the script's own text (not a
path), run inside the guest rootfs while the image is being built:

```sh
docker build --build-arg GUEST_SETUP_SCRIPT="$(cat my-setup.sh)" -t kontur .
```

The result is baked into `disk.img`, so it is paid once by whoever
builds the image rather than once per host at first boot, and every VM
booted from that image already has it. Anything the script writes is as
public as the image is, so don't use it to place secrets.

One thing not to put in it: a kernel package. `GUEST_KERNEL_PACKAGE`
installs one *before* this script runs, and that ordering is load-bearing
-- see "Networking" for what a kernel installed at the wrong point does
to the initramfs, and the `guest-customized` stage for the mechanics.

It runs in the `guest-customized` stage, which promotes the rootfs to an
image of its own (`FROM scratch` + `COPY --from=guest-rootfs /rootfs/ /`)
and runs the script as an ordinary `RUN`. That is deliberate, and it is
what makes this mechanism usable at all: running something *in a
directory tree* means `chroot`, and a useful chroot needs `/proc` and
`/dev` bind-mounted into it -- `CAP_SYS_ADMIN`, which an ordinary
`docker build` does not have, and without which apt postinsts,
`systemctl enable` and `update-initramfs` variously misbehave or fail
outright. Running it as a container instead gives the script a real
`/proc`, a real `/dev`, working network and the rootfs as `/`, with no
extra privileges -- the same property the `guest-image` stage's
`mke2fs -d` already relies on. What it still doesn't get is a *running*
guest: no booted kernel, no service manager actually running, so
`systemctl enable` works but `systemctl start` does not.

`CHV_SETUP_SCRIPT` (see the top-level README's "Suspend and resume") is
the boot-time counterpart: it boots the actual guest and runs your script
inside it once, so it gets a running kernel, service manager and network
stack, and there's nothing it can't do that an interactively-administered
guest could. Pair it with `CHV_SNAPSHOT_PATH` to pay that cost only once:
the suspended snapshot after the first run stands in for what would
otherwise be a customized `disk.img`.

So: `GUEST_SETUP_SCRIPT` for installing packages and dropping in files,
`CHV_SETUP_SCRIPT` for anything that has to observe the guest actually
running. They compose -- a build-time script can install what a boot-time
one then configures against the live system.

## Deriving a guest from a published image

`GUEST_SETUP_SCRIPT` needs a checkout and a full image build. To
customize a guest you *pulled*, boot it and commit the result:

```sh
konturctl guest build \
  -from ghcr.io/bwsalmon/kontur:debian12 \
  -setup ./my-setup.sh \
  -t my-guest:dev
```

That boots the base image's own guest under cloud-hypervisor, runs
`my-setup.sh` as root inside it over `kontur exec`, scrubs per-boot
identity, powers the guest off, and commits the container. What comes out
is the same *kind* of image as what went in -- kontur, cloud-hypervisor
and a bootable guest disk -- so `docker run` boots it exactly as the base
boots, and it can be the `-from` of another build.

Why bother, when `GUEST_SETUP_SCRIPT` installs packages perfectly well:
that hook runs in a container on the build host's kernel with no service
manager, so it can install and cannot *exercise*. This boots the real
guest, so `systemctl start` works, a docker image can be pulled into the
guest's own cache ahead of first use, and a test suite can be run against
the finished image before it is committed.

What it costs:

- **`/dev/kvm` on whatever runs the build.** It is a `docker run --device
  /dev/kvm --cap-add NET_ADMIN`, not a `docker build` -- deliberately, since
  a build cannot ask for a device without BuildKit's `security.insecure`
  entitlement, and this needs no entitlement, no privileged builder and
  no `mknod`. It does mean no building a customized guest on a machine
  without KVM.
- **The guest gets none of the builder's network context.** Its packages
  are fetched from inside a VM, which has no `HTTPS_PROXY`, no custom CA
  bundle and no registry credentials unless you pass them:
  `-docker-run-arg` is repeatable and goes straight to `docker run`.
- **Each build adds a full copy of the disk as a layer.** `disk.img` is
  one large file in a read-only layer, so the guest's first write copies
  all of it up, and `commit` captures that copy. Deriving two or three
  deep is fine; a long chain wants squashing.

The scrub is not optional and not the caller's to remember: `/etc/machine-id`,
`/var/lib/dbus/machine-id`, any SSH host keys, systemd's random seed,
DHCP leases and `/var/log` are emptied after the setup script and before
shutdown. The host keys are on that list even though this image runs no
sshd, because a setup script is free to install one -- and a shared image
that carried its host keys would hand every VM cloned from it the same
identity. All of it is per-boot identity, and an image is cloned into many
VMs -- a baked machine-id gives every one of them the same journald
identity and DHCP DUID, and baked host keys make host-key verification
meaningless across the fleet. What the setup script installed or left
behind is left alone: that is size rather than correctness, and guessing
would delete things a caller meant to keep.

If the guest never becomes reachable, or its setup script fails,
`-keep-on-failure` leaves the container up and the error names it, so
`docker logs` shows the console and `docker exec <ctr> kontur exec` gets
a shell in the guest.

## Exporting the built guest

The `guest-artifacts` target publishes the built guest for booting
somewhere other than inside this image:

```sh
docker build --target guest-artifacts --output type=local,dest=./out .
```

That yields `disk.img`, plus `vmlinuz` and `initrd.img` when the guest
has its own -- i.e. when it was built with `GUEST_KERNEL_PACKAGE` (or a
`GUEST_SETUP_SCRIPT` that installs a kernel by hand), which a guest whose
workload needs a richer kernel config than the `fetch-kernel` stage's
`cloud-hypervisor/linux` release build carries (overlayfs, cgroup v2,
bridge netfilter, veth, ...) will want. The newest `/boot/vmlinuz-*` and
`/boot/initrd.img-*` in the rootfs are what get published; a guest with
no kernel package installed produces neither.

`GUEST_DISK_EXTRA_MB` adds headroom to that disk, and an image built to
be derived from wants it. The 20% the sizing above adds is space for logs
and runtime growth on a guest that is already finished; a guest
customized by `konturctl guest build` is provisioned *after* the
filesystem is packed, and cannot grow it. Without headroom an
`apt-get install` of anything substantial fails with "You don't have
enough free space in `/var/cache/apt/archives/`" after a completely
clean boot -- which reads as a broken setup script rather than a disk
sized before anyone knew what would go on it. It is not free, and the cost
is easy to misjudge: `truncate` leaves the extra as a hole and a pushed
layer of zeros compresses to almost nothing, but extracting a layer
materializes the hole, so every GB here is a GB on disk on every machine
that pulls the image. Size it against what the guest installs. kontur's
own published `debian12` variant uses 2GB, which is more free space than
the 20% gave and small enough not to dominate the image.

The same pair is copied into the final image beside the disk, and
`internal/config` boots whichever is there: the guest's own kernel when
the image has one, `fetch-kernel`'s `vmlinux` when it doesn't. So a
`GUEST_KERNEL_PACKAGE` build needs no `CHV_KERNEL`/`CHV_INITRAMFS` of its
own to boot correctly, and a default build behaves exactly as it always
has.

The target is `FROM scratch` and contains nothing else, so the exported
directory holds exactly those files rather than a whole builder
filesystem. None of it bloats the `kontur` runtime image: `final` is
still the Dockerfile's last stage and so still the default build target.

## Growing the filesystem

`CHV_DISK_SIZE_MB` (`konturctl vm create -disk-size-mb`; see the
top-level README's "Disk size") sizes the qcow2 overlay before the VM
boots, which grows the *block device* the guest is offered and nothing
else. The ext4 inside it is still exactly what `mke2fs -d` made when the
image was built, so on its own that setting gives a guest an 8GiB
`/dev/vda` with the same few hundred MiB of free space it always had.

`kontur-growfs` closes that on both variants: it runs `resize2fs` on the
root device in early boot, so free space in the guest reflects
`-disk-size-mb` without anyone logging in to do it by hand. Two things
make it a single command rather than a growpart-then-resize dance:

- this image puts ext4 **directly on `/dev/vda`, with no partition
  table** (the `guest-image` stage's `mke2fs` writes to the whole disk),
  so there is no partition whose end has to be moved first;
- ext4 grows **online**, with the filesystem mounted as root, so there is
  no initramfs hook and no unmount to arrange -- which also means a
  `GUEST_KERNEL_PACKAGE` guest needs nothing extra.

It runs on every boot and is a no-op on almost all of them: `resize2fs`
finds the filesystem already as long as the device and reports "Nothing
to do!". It also declines, successfully and with a line on the console
saying so, when the root is mounted read-only (`CHV_DISK_MODE=readonly`),
when the root filesystem isn't ext2/3/4, or when `resize2fs` isn't
installed -- and it treats a failed resize as a smaller disk rather than
a failed boot, since a guest with less free space than asked for is still
a guest you can log into and look at.

Ordering is what the service units add: `sysinit.target` on Debian
(`DefaultDependencies=no`, after `systemd-remount-fs.service`, the same
shape as systemd's own `systemd-growfs-root.service`) and the `boot`
runlevel on Alpine. Either way it is done before anything is writing
into the space it just made.

The Alpine rootfs installs `e2fsprogs-extra` for this, not just
`e2fsprogs`: Alpine splits `resize2fs` out of the base package. Debian's
`e2fsprogs` is unsplit and already there via `debootstrap`. Both rootfs
stages fail the build if `resize2fs` is missing, so a guest that would
silently never grow doesn't get built.

A guest that is *not* this one -- a `CHV_DISK_IMAGE` of your own, with a
partition table or a non-ext4 root -- has to do its own growing, exactly
as before.

## Graceful shutdown

`kontur run`'s shutdown path (see the top-level README's "Shutdown"
section) presses the ACPI power button via the cloud-hypervisor API and
waits for the guest to power off on its own before forcing it. Reacting
to that button press needs *something* running in the guest to catch it
-- normally `systemd-logind` via `dbus`, but pulling those in just for a
power button is significant extra weight for a minimal image, so this
guest uses `acpid` instead (a few hundred KB, no `dbus` dependency) on
both variants. `overlay-common/etc/acpi/events/powerbtn` is the event
config acpid needs to match the button press (same config format on
both variants -- Debian's `acpid` and Alpine's busybox `acpid` applet are
config-compatible), pointing at `/etc/acpi/powerbtn.sh`:

- Debian: that script runs `systemctl poweroff`. `acpid.socket`/
  `acpid.path` are enabled automatically, as described in "Enabling
  services" above.
- Alpine: that script runs `poweroff` (busybox's applet). With busybox as
  PID 1 (`/sbin/init -> /bin/busybox`), `poweroff` doesn't call
  `reboot(2)` directly -- it signals init, which (per `/etc/inittab`'s
  `::shutdown:/sbin/openrc shutdown` line) runs the OpenRC `shutdown`
  runlevel, the same stop-services/unmount/sync sequence `systemctl
  poweroff` triggers on Debian. `acpid` itself is enabled by hand, as
  described in "Enabling services" above (Alpine has no
  `deb-systemd-helper` equivalent that would do it automatically).

## Networking

The guest itself doesn't run a DHCP client: it relies on the kernel's
`ip=` boot-time autoconfiguration (see the top-level README's
`CHV_CMDLINE` default and `deploy/k8s/pod-example.yaml`), same as every
other guest example in this repo, so no extra guest-side networking setup
is needed for a guest's own services to be reachable once a caller has
configured `CHV_CMDLINE` (or let `netshim` derive the `ip=` parameter for
it). This is also why neither variant enables an OpenRC/systemd
`networking` service. Note that `kontur exec` needs none of this: it goes
over vsock, which is why a guest that gets this wrong is still one you
can get into and fix.

That is the *kernel's* built-in autoconfiguration, and it holds only for
a kernel with `CONFIG_IP_PNP` -- which the bundled `fetch-kernel` build
sets and Debian's stock `linux-image-amd64` does not. So a guest built
with `GUEST_KERNEL_PACKAGE` has nothing acting on `ip=` at all, and would
come up with no address and simply never be reachable.
`kontur-net-cmdline.service` closes that: it reads `ip=` out of
`/proc/cmdline` and hands the spec to klibc's `ipconfig(8)`, which
implements the same syntax in userspace. It runs early, and exits
immediately when the interface is already addressed -- so on a guest
booting the bundled kernel it does nothing at all.

`ip=` is also not the whole of what a container's interface carries. It
has one netmask and no route list, so a guest configured from it alone
believes its whole subnet is on-link -- true on a bridge CNI, and
exactly what a point-to-point one (kind's default, GKE's) arranges for
it not to be. `netshim` therefore passes the interface's actual routing
table down as a `kontur.routes=` parameter of kontur's own, and
`kontur-net-routes.service` installs it. Both variants carry it, since
the kernel that acts on `ip=` has no equivalent for routes and never
will. See the top-level README's "Container networking".

Two details `ip=` depends on, both easy to lose:

- The NIC has to keep the name the `ip=` spec gives it. `konturctl`
  hard-codes `eth0` (`internal/staticpod/spec.go`), and udev's
  predictable naming would rename a virtio-net device to `ens2`; the
  `80-net-setup-link.rules` mask in the `guest-rootfs-debian` stage is
  what stops it, for this and for the control link's `eth1` alike.
- That mask has to reach the *initramfs*, whose own snapshot of
  `/etc/udev` is what runs first. It does because `GUEST_KERNEL_PACKAGE`
  is installed after the overlays are in place, so the initramfs its
  postinst generates already has the mask -- see the `guest-customized`
  stage. A setup script that writes udev rules of its own has to run
  `update-initramfs -u` itself.


### The resolver

`debootstrap` copies the *build host's* `/etc/resolv.conf` into the
rootfs, and a build host's resolver is routinely an address that only
exists in its own network namespace -- docker's embedded `127.0.0.11`, a
cloud metadata service, a link-local stub. From a guest on a tap that is
simply unroutable, so an image built that way comes up with completely
open IP egress and no DNS at all: every name lookup hangs until it times
out, which reads as a blocked network rather than as a missing resolver.
It also makes the image unreproducible -- whichever machine built it
contributes its own unreachable resolver to every guest booted from it.

Two things settle it, deliberately at different levels:

- `GUEST_DNS` (a build arg, default `8.8.8.8`) is the baseline written
  into the image's own `/etc/resolv.conf`, so the image is the same
  wherever it was built. Set it empty to leave whatever `debootstrap`
  copied in.
- The nameservers on the guest's `ip=` boot parameter are what a guest
  actually resolves through, and `kontur-configure-dns` writes them over
  that baseline on every boot. They come from `NETSHIM_DNS` (konturctl's
  `-dns`), so a deployment names the resolver its own network has without
  rebuilding anything. `-dns ''` leaves the baseline in place.

Nothing else in a kontur guest writes that file: there is no
systemd-resolved and no DHCP client, since the address is static on the
command line. Neither the kernel's own `CONFIG_IP_PNP` handling nor
klibc's `ipconfig(8)` writes `resolv.conf` either -- the kernel only
exposes what it was given in `/proc/net/pnp`, and `ipconfig` writes
`/run/net-<dev>.conf` and leaves the file to an initramfs script this
guest does not run. Hence a step of its own, applying to every guest
kontur builds, bundled kernel or distro kernel, Debian or Alpine.
