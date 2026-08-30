# Guest disk image

The `guest-image` stage in the top-level `Dockerfile` (packing) plus one
of the two `guest-rootfs-debian`/`guest-rootfs-alpine` stages (building
the rootfs `guest-image` packs) build the disk image baked into the
final `kontur` image at `/var/lib/kontur/guest/disk.img`, used
automatically when a VM container doesn't set `CHV_DISK_IMAGE` (see
`internal/config`). `GUEST_DISTRO` (default `debian`) picks which of the
two rootfs stages runs:

- `debian` (default): a minimal Debian rootfs (`debootstrap
  --variant=minbase`) with `openssh-server`, `systemd-sysv` (for
  `/sbin/init`), `iproute2`, `acpid` and `udev`. `GUEST_SUITE` (default
  `bookworm`) picks the Debian suite. `udev` has to be listed explicitly
  -- `--variant=minbase` skips Recommends, and without `systemd-udevd`
  running, `dev-ttyS0.device` never gets marked ready, which stalls boot
  for `DefaultDeviceTimeoutSec` (systemd's default: 90s) before
  `serial-getty@ttyS0.service` gives up on it; SSH itself doesn't depend
  on that device unit and isn't affected, but the console (this guest's
  only other point of entry) would otherwise never show a login prompt.
- `alpine`: a minimal Alpine rootfs (`apk add --root`, the same technique
  Alpine's own official base image is built with) with `openssh-server`,
  `alpine-base` (for `busybox`/`openrc`, standing in for `systemd-sysv`),
  `iproute2` and `acpid`. `GUEST_ALPINE_VERSION` (default `3.20`) picks
  the Alpine version.

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
| `overlay-common/etc/ssh/sshd_config.d/10-console.conf` | Forces every SSH session through the wrapper below and disables password auth (root has no password at all, only whatever key `GUEST_SSH_AUTHORIZED_KEY` bakes in at build time). |
| `overlay-common/usr/local/libexec/kontur-ssh-console-wrap` | Runs the session under `script`, which mirrors its output to `/dev/console` (ttyS0) in addition to the real SSH client. |
| `overlay-common/etc/acpi/events/powerbtn` | `acpid` event config matching the ACPI power button, pointing at `/etc/acpi/powerbtn.sh` -- see "Graceful shutdown" below for why that script's own contents differ per variant. |
| `overlay-debian/etc/acpi/powerbtn.sh` | Runs `systemctl poweroff`. |
| `overlay-debian/etc/systemd/system/kontur-ssh-host-keys.service` | Regenerates SSH host keys on first boot (`ssh-keygen -A`, `Before=ssh.service`), since the Dockerfile deletes whatever `openssh-server`'s postinst generated at build time -- otherwise every VM booted from this image would share the same host keys. |
| `overlay-alpine/etc/acpi/powerbtn.sh` | Runs `poweroff` (busybox's applet, which signals `/sbin/init` rather than powering off directly -- see "Graceful shutdown"). |
| `overlay-debian/etc/systemd/system/kontur-mem-agent.service`, `overlay-alpine/etc/init.d/kontur-mem-agent` | Run `kontur-mem-agent` (copied into both variants straight from the top-level Dockerfile's own `build` stage, not part of either overlay) as a restart-always service. See the top-level README's "Memory hotplug". |

The Alpine variant has no equivalent of `kontur-ssh-host-keys.service`:
unlike Debian's, Alpine's `openssh-server` package doesn't generate host
keys at install time at all (nothing to delete), and its own `sshd`
OpenRC init script (`checkconfig` -> `generate_host_keys`) already runs
`ssh-keygen -A` on every start, filling in whatever's missing -- so the
same first-boot regeneration Debian needs a dedicated unit for comes for
free on Alpine.

## Enabling services

Debian's `systemd-sysv` and Alpine's `busybox`/`openrc` both need
services enabled by hand in the Dockerfile rather than through their
normal tooling (`systemctl enable`, `rc-update add`), since neither has
a running service manager during the build to ask -- each is just a
symlink, written directly:

- Debian: `kontur-ssh-host-keys.service` and `kontur-mem-agent.service`
  are both symlinked into `multi-user.target.wants/`. `acpid.socket`/
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
  internally, with nothing to enable by hand for it), and `default` for
  `sshd`/`acpid`/`kontur-mem-agent` themselves. See the
  `guest-rootfs-alpine` stage in the top-level `Dockerfile` for the exact
  list.

## Why SSH output goes to the console

kontur's "run" mode (`cmd/kontur`) streams the VM's serial console
straight to the container's own stdout/stderr, so `kubectl logs` on a VM
container *is* that VM's console output. The guest has no other log
shipper, and there is no direct network path into a VM's pod from outside
Kubernetes port-forwarding/netshim's forwarded port -- so mirroring SSH
session output onto the console is what makes SSH activity on this guest
observable the same way everything else about the VM already is, without
adding a separate logging pipeline inside the guest.

## Getting SSH access

Root login is key-only (`PermitRootLogin prohibit-password` plus no
password set). By default this image already authorizes one key: the
public half of a keypair the top-level `Dockerfile`'s `exec-keypair`
stage generates at build time, whose private half is baked into the
outer `kontur` image so `kontur exec` (see the top-level README's
"Execing into a VM") always has a way in without any of this section's
setup. Pass your own public key at build time too, to allow your own
key-based root login *alongside* that one:

```sh
docker build --build-arg GUEST_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" -t kontur .
```

This works the same way on both `GUEST_DISTRO` variants.

## Running a custom setup script

To customize the guest beyond what the overlays above do -- installing
extra packages, dropping in config files, enabling services, etc -- use
`CHV_SETUP_SCRIPT` (see the top-level README's "Suspend and resume")
instead of a build-time mechanism: it boots the actual guest and runs
your script over SSH once, which -- unlike a build-time `chroot` -- gets
a running kernel, service manager, and network stack to work with, so
there's nothing it can't do that an interactively-administered guest
could. Pair it with `CHV_SNAPSHOT_PATH` to pay that cost only once: the
suspended snapshot after the first run stands in for what would
otherwise be a customized `disk.img` baked at build time.

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
built-in `ip=` boot-time autoconfiguration (see the top-level README's
`CHV_CMDLINE` default and `deploy/k8s/pod-example.yaml`), same as every
other guest example in this repo, so no extra guest-side networking setup
was needed for SSH to be reachable once a caller has configured
`CHV_CMDLINE`/`netshim` port forwarding. This is also why neither
variant enables an OpenRC/systemd `networking` service.
