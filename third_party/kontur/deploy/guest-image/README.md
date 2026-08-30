# Guest disk image

The `guest-image` stage in the top-level `Dockerfile` builds the disk image
baked into the final `kontur` image at `/var/lib/kontur/guest/disk.img`,
used automatically when a VM container doesn't set `CHV_DISK_IMAGE` (see
`internal/config`). It's a minimal Debian rootfs (`debootstrap
--variant=minbase`) with `openssh-server`, `systemd-sysv` (for
`/sbin/init`), `iproute2` and `acpid`, packed directly into an ext4
filesystem image with `mke2fs -d` -- no loop-mount or `chroot` needed on
the build host beyond what `debootstrap`/`mke2fs` already do, so `docker
build` doesn't need extra privileges for this stage.

This is a reference/demo guest, not a production one: any real workload
should bring its own `CHV_DISK_IMAGE` (see the top-level README's
"Configuration" table and `deploy/k8s/pod-example.yaml`) rather than
relying on what's baked in here.

## What's in `overlay/`

Copied verbatim into the rootfs before it's packed into `disk.img`:

| File | Purpose |
|---|---|
| `etc/ssh/sshd_config.d/10-console.conf` | Forces every SSH session through the wrapper below and disables password auth (root has no password at all, only whatever key `GUEST_SSH_AUTHORIZED_KEY` bakes in at build time). |
| `usr/local/libexec/kontur-ssh-console-wrap` | Runs the session under `script`, which mirrors its output to `/dev/console` (ttyS0) in addition to the real SSH client. |
| `etc/systemd/system/kontur-ssh-host-keys.service` | Regenerates SSH host keys on first boot (`ssh-keygen -A`, `Before=ssh.service`), since the Dockerfile deletes whatever `openssh-server`'s postinst generated at build time -- otherwise every VM booted from this image would share the same host keys. |
| `etc/acpi/events/powerbtn`, `etc/acpi/powerbtn.sh` | `acpid` event/action pair that runs `systemctl poweroff` on an ACPI power-button press -- see "Graceful shutdown" below. |

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

By default the image ships with no way to authenticate as root at all
(`PermitRootLogin prohibit-password` plus no password set). Pass a public
key at build time to allow key-based root login:

```sh
docker build --build-arg GUEST_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" -t kontur .
```

## Running a custom setup script

To customize the guest beyond what the overlay above does -- installing
extra packages, dropping in config files, enabling services, etc -- pass
a shell script's contents at build time:

```sh
docker build --build-arg GUEST_SETUP_SCRIPT="$(cat my-setup.sh)" -t kontur .
```

The script runs via `chroot` against the rootfs after the overlay is
applied but before it's packed into `disk.img`, the same mechanism
`debootstrap` itself already uses (non-privileged in the sense the top of
this file describes: no extra `docker build` privileges are needed).
Network access works fine (`chroot` doesn't create a new network
namespace, so it's the same connectivity the rest of the build already
has), so `apt-get install` and the like just work. What the script
*doesn't* get is `/proc`/`/sys` (nothing mounts them here, so anything
that reads from them will find those directories empty) or a running
service manager -- enable units by symlinking into `*.target.wants/` by
hand instead, the same way `kontur-ssh-host-keys.service` above is
enabled.

## Graceful shutdown

`kontur run`'s shutdown path (see the top-level README's "Shutdown"
section) presses the ACPI power button via the cloud-hypervisor API and
waits for the guest to power off on its own before forcing it. Reacting
to that button press needs *something* running in the guest to catch it
-- normally `systemd-logind` via `dbus`, but pulling those in just for a
power button is significant extra weight for a minimal image, so this
guest uses `acpid` instead (a few hundred KB, no `dbus` dependency).
`overlay/etc/acpi/events/powerbtn` and `overlay/etc/acpi/powerbtn.sh`
are the event/action pair acpid needs to turn that button press into
`systemctl poweroff`; `acpid.socket`/`acpid.path` are enabled
automatically by `acpid`'s own postinst during `debootstrap` (unlike
`kontur-ssh-host-keys.service` above, which isn't a packaged unit and so
needs enabling by hand in the Dockerfile).

## Networking

The guest itself doesn't run a DHCP client: it relies on the kernel's
built-in `ip=` boot-time autoconfiguration (see the top-level README's
`CHV_CMDLINE` default and `deploy/k8s/pod-example.yaml`), same as every
other guest example in this repo, so no extra guest-side networking setup
was needed for SSH to be reachable once a caller has configured
`CHV_CMDLINE`/`netshim` port forwarding.
