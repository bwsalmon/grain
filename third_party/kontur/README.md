# kontur

`kontur` is a single, self-contained OCI image and Go binary that boots a
single [cloud-hypervisor](https://www.cloudhypervisor.org/) VM as its
entrypoint, configured entirely from environment variables so it can be
driven directly from a Kubernetes pod spec. The image bakes in everything
a VM needs to boot -- a reference guest disk image and a matching guest
kernel (see "Guest disk image and kernel" below) -- so `docker run
kontur` or a bare Kubernetes pod using it, with no other flags or
volumes, already boots a working VM; point `CHV_DISK_IMAGE`/`CHV_KERNEL`
(or `CHV_FIRMWARE`) at a node-local image cache, hostPath, or PVC instead
when the built-in reference guest isn't the guest you actually want to
run. This runtime never fetches images at boot time, so there is nothing
on the startup path except launching the VMM.

The same image also sets up the pod-local networking that lets several
such VMs share one pod's IP, as an init container. See "How it works"
below for how one binary/image ends up serving both roles.

A separate binary, `konturctl`, is the operator-facing CLI: it runs on
the node itself (not inside a container) to install a standalone kubelet
and manage the VM pods `kontur` runs. See "Operating a node" below.

## How it works

The image contains three things:

- `kontur` (this repo, Go): a single binary with five modes, selected by
  the first argument (`run`, the default if none is given; `netshim`;
  `exec`; `resize`; or `sleep`):
  - **`run`** reads configuration from the environment, execs
    `cloud-hypervisor` with the resulting arguments, streams the guest's
    serial console to the container's stdout/stderr so it shows up under
    `kubectl logs`, turns `SIGTERM`/`SIGINT` into a graceful VM shutdown,
    and, if configured, runs a one-time setup script and suspends the VM
    once it completes so a later run can resume it instead of booting
    fresh; see "Suspend and resume" below.
  - **`netshim`** sets up the pod-local networking (bridge, taps, NAT)
    that lets several VM containers in the same pod share the pod's IP;
    see "Pod-local networking" below. With `NETSHIM_MODE=flat` it instead
    splices a single guest straight onto the namespace's own segment, to
    take over the address and MAC the container runtime assigned; see
    "Flat mode" below. Either way it's meant to run once, to completion,
    as an init container.
  - **`exec`** runs a command inside the VM guest itself, over SSH to its
    already-running `sshd`, rather than in this otherwise-empty
    container -- meant to be `kubectl exec`'s own command, so that ends
    up in the guest too; see "Execing into a VM" below.
  - **`resize`** live-resizes an already-running VM's memory and/or
    vCPU count via cloud-hypervisor's own API, within the ranges `run`
    configured at boot -- also meant to be `kubectl exec`'s own command
    (it needs to reach the API socket inside this container); see
    "Memory hotplug" and "CPU hotplug" below.
  - **`sleep`** just blocks until killed. It exists purely so
    `-backend docker` (see "Operating a node" below) can hold a network
    namespace open with the `kontur` image itself, without needing a
    coreutils `sleep` binary the image doesn't otherwise carry.

  `netshim` talks to the kernel directly via netlink/nftables (see
  "Pod-local networking" below) rather than shelling out to `ip`/
  `iptables`, so none of the five modes needs anything beyond the two
  statically linked binaries themselves -- the image's base is `scratch`
  (see "Building" below).
- `cloud-hypervisor`: the actual VMM, fetched from the upstream static
  release build and pinned by checksum in the `Dockerfile`.
- A reference guest disk image and a matching guest kernel (see "Guest
  disk image and kernel" below): a minimal Debian system with `sshd`,
  plus a kernel that can boot it via cloud-hypervisor's direct-kernel-boot
  path, both baked into the image so a VM container works out of the box
  without a separately-managed disk image or kernel.

A VM pod is one `netshim`-mode init container plus one or more `run`-mode
containers, all using the *same* `kontur` image -- just invoked with
different `args` (`["netshim"]` vs `["run"]`). See
[`deploy/k8s/pod-example.yaml`](deploy/k8s/pod-example.yaml).

`run` boots the VM with a single `cloud-hypervisor` invocation (no
separate create/boot API calls), so the only startup cost is the VM boot
itself. It also starts cloud-hypervisor's local HTTP API (over a unix
socket) purely for lifecycle control -- shutdown and, in the future,
health checks -- not for driving the boot.

### Fast startup

- No image fetching: `CHV_DISK_IMAGE` and `CHV_KERNEL`/`CHV_FIRMWARE` are
  expected to already be on the local filesystem, e.g. via a hostPath or a
  PVC backed by a node-local image cache -- or, left unset, default to the
  reference guest disk image and kernel already baked into the `kontur`
  image itself (see "Guest disk image and kernel"), so there is nothing to
  provision out of band just to get a VM running.
- Direct kernel boot (`CHV_KERNEL`, the default) skips firmware/UEFI
  entirely, since the guest kernel is known ahead of time; only fall back
  to `CHV_FIRMWARE` (e.g. an OVMF/CLOUDHV build) for guests that need to
  run their own bootloader -- the bundled reference guest has no
  bootloader of its own, so it only ever boots via `CHV_KERNEL`.

### Shutdown

On `SIGTERM`/`SIGINT`, `kontur run`:

1. Calls the cloud-hypervisor API's `vm.power-button`, simulating an ACPI
   power button press so the guest can shut down cleanly.
2. Waits up to `CHV_SHUTDOWN_TIMEOUT` for the process to exit on its own
   (cloud-hypervisor exits automatically once the guest powers off).
3. Falls back to the API's `vmm.shutdown` (forceful, no guest cooperation
   needed), then `SIGTERM`, then `SIGKILL`, each bounded by a short grace
   period, until the process is gone.

The container's own exit code mirrors cloud-hypervisor's, so a crashed VM
is visible to Kubernetes as a failed container.

Step 1 only shuts the guest down cleanly if something inside it is
listening for the ACPI power button event -- the bundled guest image
does, via `acpid` (see `deploy/guest-image/README.md`'s "Graceful
shutdown"), but a custom `CHV_DISK_IMAGE` needs to bring its own
`acpid`/`systemd-logind` or equivalent, or every shutdown will pay the
full `CHV_SHUTDOWN_TIMEOUT` before falling through to step 3.

### Suspend and resume

`CHV_SETUP_SCRIPT`, if set, is run once inside the guest over SSH (the
same machinery `kontur exec` uses, see "Execing into a VM" below) right
after a fresh boot finishes. There's a single script for this regardless
of whether you want its result snapshotted: `kontur` always boots the VM
to run it, and `CHV_SNAPSHOT_PATH` is what makes suspending afterward
optional rather than a separate mechanism.

- With `CHV_SNAPSHOT_PATH` set, once the script exits zero, `kontur run`
  pauses the VM, snapshots its full state (memory included) to
  `CHV_SNAPSHOT_PATH` via the cloud-hypervisor API's
  `vm.pause`/`vm.snapshot`, and resumes it -- so this run carries on as
  if nothing happened. The next `kontur run` that finds a complete
  snapshot already at `CHV_SNAPSHOT_PATH` boots by restoring it
  (`--restore source_url=file://...,resume=true`) instead of booting
  fresh, which skips both the normal boot path and the setup script
  entirely -- the restored guest already reflects whatever the script
  did. `CHV_SNAPSHOT_PATH` must be an absolute path whose parent
  directory already exists, and, to actually be picked up on a later
  run, needs to live somewhere that survives the current container going
  away (a hostPath or PVC, the same as a persistent `CHV_DISK_IMAGE`).
- Without `CHV_SNAPSHOT_PATH`, the script just runs again on every fresh
  boot instead -- useful for a script that's cheap and idempotent, or
  for guest customization where there's no persistent path available to
  snapshot to.

A snapshot is only ever published by renaming a completed staging
directory into its final path, so a reader never observes a partial one
even if the process is killed mid-snapshot.

## Configuration

`kontur run`'s configuration is entirely via environment variables:

| Variable              | Required | Default                          | Description |
|------------------------|:--------:|-----------------------------------|--------------|
| `CHV_DISK_IMAGE`       | no       | the guest image baked into this image (`/var/lib/kontur/guest/disk.img`) | Path to the primary disk image. |
| `CHV_DISK_MODE`        | no       | `overlay`                         | How the primary disk is attached: `overlay` (the guest writes into a thin qcow2 of its own, created in this container, leaving the image untouched), `persistent` (the guest writes through to the image itself) or `readonly`. |
| `CHV_DISK_OVERLAY_PATH`| no       | `/var/lib/kontur/overlay/disk.qcow2` | Where `overlay` mode puts that qcow2. Inside the container's own writable layer, so a restart keeps it and only removing the container discards it. |
| `CHV_DISK_READONLY`    | no       | —                                 | Deprecated, replaced by `CHV_DISK_MODE`: `true` means `readonly`, `false` means `persistent`. Setting both is an error. |
| `CHV_EXTRA_DISKS`      | no       | —                                 | Comma-separated additional disks: `path[:ro\|rw]`. |
| `CHV_KERNEL`           | no       | the kernel baked into this image (`/var/lib/kontur/guest/vmlinux`), unless `CHV_FIRMWARE` is set | Path to a kernel for direct boot (PVH/`vmlinux`). Mutually exclusive with `CHV_FIRMWARE`. |
| `CHV_INITRAMFS`        | no       | —                                 | Path to an initramfs, used with `CHV_KERNEL`. |
| `CHV_CMDLINE`          | no       | `console=ttyS0 root=/dev/vda rw` | Kernel command line, used with `CHV_KERNEL`. |
| `CHV_FIRMWARE`         | no       | —                                 | Path to firmware (e.g. `CLOUDHV.fd`) for firmware boot, instead of `CHV_KERNEL`'s default. Mutually exclusive with `CHV_KERNEL`. |
| `CHV_CPUS`             | no       | `2`                               | Boot vCPU count. |
| `CHV_CPUS_MAX`         | no       | `CHV_CPUS`                        | Ceiling `CHV_CPUS` can grow to via hotplug. See "CPU hotplug" below. |
| `CHV_MEMORY_MB`        | no       | `256`                             | Guest memory at boot, in MiB. See "Memory hotplug" below. |
| `CHV_MEMORY_MAX_MB`    | no       | `2048`, or `CHV_MEMORY_MB` if larger | Ceiling `CHV_MEMORY_MB` can grow to via hotplug. See "Memory hotplug" below. |
| `CHV_MEMORY_HOTPLUG`   | no       | `true`                            | Attach a memory hotplug device sized for growth up to `CHV_MEMORY_MAX_MB`. See "Memory hotplug" below. |
| `CHV_MEMORY_SHARED`    | no       | `true`                            | Shared guest memory (required for some device backends, and for `CHV_MEMORY_HOTPLUG`). |
| `CHV_NET`              | no       | —                                 | Passed through verbatim as `--net`, e.g. `tap=eth0,mac=...`. |
| `CHV_API_SOCKET`       | no       | `/run/cloud-hypervisor/api.sock` | Path to the cloud-hypervisor API socket. |
| `CHV_BINARY_PATH`      | no       | `/usr/local/bin/cloud-hypervisor` | Path to the cloud-hypervisor binary. |
| `CHV_EXTRA_ARGS`       | no       | —                                 | Extra CLI args appended verbatim, space-separated (e.g. `--watchdog`). |
| `CHV_SHUTDOWN_TIMEOUT` | no       | `20s` (Go duration syntax)        | How long to wait for a graceful guest shutdown before forcing it. |
| `CHV_SETUP_SCRIPT`     | no       | —                                 | Shell script run once inside the guest over SSH after a fresh boot. If `CHV_SNAPSHOT_PATH` is also set, suspended to it on success; otherwise reruns on every fresh boot. See "Suspend and resume". |
| `CHV_SNAPSHOT_PATH`    | no       | —                                 | Absolute path to suspend the VM's state to on success of `CHV_SETUP_SCRIPT`, and to restore it from if a complete snapshot is already there at startup. See "Suspend and resume". |
| `CHV_MEM_AGENT`        | no       | `false`                           | Let the guest-side memory-pressure agent baked into the disk image trigger hotplug growth on its own. Requires `CHV_MEMORY_HOTPLUG=true`. See "Memory hotplug". |
| `CHV_MEM_AGENT_ADDR`   | no       | `169.254.100.1:30090`            | Address the guest's pressure signals are received on; must be reachable from the guest and match what it was built to signal. See "Memory hotplug". |
| `CHV_MEM_AGENT_STEP_MB`| no       | `256`                             | How much a single pressure signal grows the guest by, capped at `CHV_MEMORY_MAX_MB`. See "Memory hotplug". |
| `CHV_MEM_AGENT_COOLDOWN` | no     | `30s` (Go duration syntax)        | Minimum time between two guest-triggered resizes. See "Memory hotplug". |

Every configured path -- `CHV_DISK_IMAGE` and `CHV_KERNEL` (including
their defaults) and, if set, `CHV_INITRAMFS`/`CHV_FIRMWARE` -- must
already exist on disk: `kontur run` checks this at startup and fails fast
with a clear error rather than letting cloud-hypervisor fail deeper into
boot.

## Memory hotplug

`CHV_MEMORY_HOTPLUG` is on by default, and `CHV_MEMORY_MB` (the guest's
size at boot) defaults to a deliberately small `256`: the VMM doesn't pay
for a large memory footprint from the very first boot the way a fixed,
large `CHV_MEMORY_MB` always did, and the guest can still grow up to
`CHV_MEMORY_MAX_MB` (`2048` by default) later, on demand. This uses
cloud-hypervisor's virtio-mem-based hotplug device (`--memory
hotplug_method=virtio-mem`) rather than its ACPI-based DIMM hotplug --
virtio-mem needs no udev rule in the guest to online newly added memory
and, unlike ACPI hotplug, also supports shrinking back down again, at the
cost of requiring a guest kernel built with `CONFIG_VIRTIO_MEM` (Linux
5.8+) to actually make use of it. A guest without that support still
boots fine at `CHV_MEMORY_MB`; it just never grows past it.

Growth can be triggered two ways: manually, from outside the guest, or
automatically, from inside it.

`kontur resize` (`kubectl exec <pod> -c <container> -- kontur resize
-memory-mb=1024`) is the manual trigger: it asks cloud-hypervisor's API
to resize the guest directly to a size between `CHV_MEMORY_MB` and
`CHV_MEMORY_MAX_MB`. The guest's own virtio-mem driver then onlines (or
offlines, if resizing down) whatever changed on its own, automatically --
no udev rule or other guest-side action needed, unlike ACPI-based
hotplug. Unlike the two boot-time sizes, this only takes effect after the
VM is already running -- there's no way to reach a container's API socket
before that.

`CHV_MEM_AGENT=true` turns on the automatic trigger: `kontur run` starts
a small listener (`internal/memagent`) alongside the VM, and the guest
disk image baked by this repo's Dockerfile (both the Debian and Alpine
variants, see `deploy/guest-image/README.md`) already ships a matching
guest-side daemon, `kontur-mem-agent`, installed and enabled by default
(as a `systemd`/OpenRC service) regardless of `CHV_MEM_AGENT`'s own
setting -- it's a no-op if nothing is listening on the other end. That
daemon polls `/proc/pressure/memory` every `KONTUR_MEM_AGENT_INTERVAL`
(`10s` default) and, once "some avg10" reaches `KONTUR_MEM_AGENT_THRESHOLD`
(`10.00` default), opens a plain TCP connection to its own default
route's gateway -- the same address `CHV_CMDLINE`'s `ip=` boot parameter
already configured, i.e. this VM's own `kontur run` container, reachable
directly since they share netshim's bridge network (see "Pod-local
networking" below) -- on `KONTUR_MEM_AGENT_PORT` (`30090` default,
matching `CHV_MEM_AGENT_ADDR`'s own default port) and writes a single
`PRESSURE <value>` line. Setting `KONTUR_MEM_AGENT_HOST` in the guest overrides that default-route
target with an explicit address, which is what flat mode needs: there the
default route leads out to the container network rather than to this VM's
own `kontur run` container. The reference guest image sets it from its
control link automatically -- see "Flat mode" below.

`internal/memagent` grows the guest by
`CHV_MEM_AGENT_STEP_MB` on each signal it receives (capped at
`CHV_MEMORY_MAX_MB`, rate-limited to once per `CHV_MEM_AGENT_COOLDOWN` so
a guest still catching up on an in-flight grow doesn't trigger another
one immediately) and calls the same `vm.resize` API `kontur resize` does.

This is deliberately a first pass at "pressure": a single fixed PSI
threshold and a single fixed growth step, with no guest-side backoff
beyond the host's own cooldown, no shrinking back down when pressure
subsides, and no authentication on the listener beyond it only being
reachable from whatever already shares the pod's network namespace (the
guest itself, and anything else in the same pod). Bounded blast radius --
the worst a misbehaving or compromised guest can do is grow itself up to
a ceiling the operator already chose (`CHV_MEMORY_MAX_MB`) -- but not a
sophisticated policy. The guest agent's target address and port are also
fixed at image build time (baked into `kontur-mem-agent`'s defaults,
since the guest has no way to learn a nonstandard one at boot beyond
`CHV_CMDLINE`'s own kernel `ip=` parameter), so overriding
`CHV_MEM_AGENT_ADDR`'s *port* away from `30090`, or `NETSHIM_BRIDGE_CIDR`
away from its own default gateway, needs a matching guest-side override
of `KONTUR_MEM_AGENT_PORT` (e.g. via `CHV_SETUP_SCRIPT` dropping in an
env override and restarting the service) to keep working -- there is
currently no plumbing to pass that through automatically. Multiple
VMs sharing one pod (see "Pod-local networking" below) also all share one
network namespace, so only one of their `kontur run` containers can
actually bind `CHV_MEM_AGENT_ADDR` at a time; this has not been extended
to disambiguate which VM a signal came from, so treat automatic growth as
a single-VM-per-pod feature for now.

`CHV_MEMORY_MAX_MB` itself, unlike the live size, can only be set at boot
(`kontur run`'s own `CHV_MEMORY_MAX_MB`/`CHV_MEMORY_MB`): cloud-hypervisor
sizes the hotplug device's address space once, from `hotplug_size` at
`--memory`, and has no way to grow that window later without a restart.
Size `CHV_MEMORY_MAX_MB` for whatever this VM might legitimately need at
its busiest, not just its typical case -- and size the *container's* own
memory request/limit (e.g. a Kubernetes pod's `resources.requests.memory`)
to `CHV_MEMORY_MAX_MB` too, not `CHV_MEMORY_MB`: cloud-hypervisor's own
process memory grows along with the guest's as it's hotplugged, so a
container limit sized only for the small starting footprint will get the
VM OOM-killed by the node the first time it actually grows.

Set `CHV_MEMORY_HOTPLUG=false` to disable all of this and get the old,
fixed-size behavior back -- `CHV_MEMORY_MB` is then simply the VM's whole
memory, unchangeable for its lifetime, same as before this existed.

Combining this with "Suspend and resume" above works -- a snapshot
captures whatever the guest's memory looks like at pause time, hotplugged
or not, and a restored VM keeps whatever size that was, still adjustable
afterwards the same way via `kontur resize` -- but is untested beyond
what unit tests cover, since cloud-hypervisor's own snapshot/restore
support is known to have rough edges in general (see its own docs) and
neither feature here was built with the other specifically in mind. If
you rely on both together, verify that combination yourself rather than
assuming it works exactly like either one alone.

## CPU hotplug

`CHV_CPUS` sets the guest's vCPU count at boot, same as always. `CHV_CPUS_MAX`
is new: set it above `CHV_CPUS` and the guest can grow up to that many vCPUs
later, on demand, via cloud-hypervisor's ACPI-based CPU hotplug. Left at its
default (`CHV_CPUS` itself, i.e. no headroom), nothing changes from before
this existed -- a fixed vCPU count for the VM's whole lifetime.

Unlike memory's virtio-mem device, CPU hotplug has no separate
`hotplug_method` or enable flag: cloud-hypervisor treats any `CHV_CPUS_MAX`
above `CHV_CPUS` as hotplug-capable on its own, and it needs an ACPI GED
device the guest kernel already brings up, not a driver built specifically
for it (`CONFIG_ACPI_REDUCED_HARDWARE_ONLY`, or Linux 5.5+).

`kontur resize` (`kubectl exec <pod> -c <container> -- kontur resize
-cpus=4`) asks cloud-hypervisor's API to resize the guest to a vCPU count
between `1` and `CHV_CPUS_MAX`. It can be combined with `-memory-mb` in the
same invocation to resize both together. Unlike memory hotplug, there is
currently no automatic trigger analogous to `CHV_MEM_AGENT` -- growing and
shrinking the vCPU count is manual only.

Growing and shrinking behave differently, and both are guest-driven rather
than instant:

- Growing creates the extra vCPU threads and advertises them to the guest,
  but -- unlike virtio-mem's memory, which onlines itself automatically --
  a guest kernel must online each one itself (e.g. `echo 1 | tee
  /sys/devices/system/cpu/cpu{4,5,6,7}/online`) before it actually uses
  them. A guest image with no such automation still boots and runs fine at
  its current count; it just never grows on its own.
- Shrinking marks the excess vCPUs for removal and the guest ejects them in
  the background with no command needed from inside it, but this only
  *completes* once the guest acknowledges that ejection. A second `kontur
  resize -cpus=...` call made before a prior shrink finishes is rejected by
  cloud-hypervisor itself with a 429 ("a cpu removal is still pending")
  rather than queued or merged -- surfaced here as a plain error from
  `APIClient.ResizeCPUs` -- so wait for one shrink to finish before
  requesting another rather than retrying blindly.

Combining this with "Suspend and resume" above -- unlike the same
combination with memory hotplug, which remains untested -- has been
verified directly against the real `cloud-hypervisor` v53.0 binary under
KVM, including the awkward case: pausing and snapshotting while a vCPU
removal was still pending (immediately after a shrink `vm.resize`, with
no wait for the guest to finish ejecting it). In every case tried --
growing then immediately suspending, and shrinking then immediately
suspending with the removal still pending -- `vm.pause`/`vm.snapshot`/
`vm.resume` all succeeded regardless, and restoring from the resulting
snapshot came back `Running` with `boot_vcpus` at the post-resize target
and no leftover "pending removal" state: a further `kontur resize`
against the restored VM succeeded immediately rather than hitting the
429 described above. cloud-hypervisor's own snapshot format captures
`boot_vcpus` as already updated to the target the same instant
`vm.resize` is accepted (see `APIClient.ResizeCPUs`'s doc comment) and
apparently resolves any in-flight removal as part of snapshotting itself
-- kontur doesn't need to wait out a pending removal, or otherwise guard
Suspend against one, before it's safe to call.

## Execing into a VM

`kubectl exec`/`docker exec` normally lands inside the container itself
-- for kontur, that's just the `scratch` image wrapping `cloud-hypervisor`,
not the actual workload running inside the guest. `kontur exec` (see
`internal/guestexec`) closes that gap by running the given command inside
the guest instead, over SSH to its already-running `sshd` (see "Guest
disk image" below), the same way `run` already makes the guest's console
show up under `kubectl logs`:

```sh
kubectl exec -it <pod> -c <container> -- kontur exec -- <command> [args...]
```

Leaving off `-- <command>` (i.e. just `kontur exec`) opens an interactive
login shell instead, the same as an ordinary `ssh <host>` with no command.
Explicitly running `kontur exec` this way handles any command; see
"Shimming sh and bash" below for a way to reach the same guest session
via plain `sh`/`bash`, without needing to know `kontur exec` exists at
all, for the common case of an interactive shell or a single `-c
<command>`.

| Variable                      | Required | Default                          | Description |
|--------------------------------|:--------:|-----------------------------------|--------------|
| `KONTUR_EXEC_ADDR`             | yes      | —                                 | The guest's own address, e.g. `169.254.100.2:22` -- the same address `CHV_CMDLINE`'s `ip=` configures on the guest, reachable directly since this container shares netshim's network namespace (see "Pod-local networking" below). `konturctl vm` sets this automatically. |
| `KONTUR_EXEC_USER`             | no       | `root`                            | The guest account to log in as. `konturctl vm create -guest-user` sets this, and does so on the VM container rather than only on the exec, because `kontur run` reads the same variable to tell the guest which account to authorize this boot's generated key for -- one setting for both halves, since a key authorized for an account nobody logs in as is as useless as a login with no key. |
| `KONTUR_EXEC_KEY`              | no       | `/etc/kontur/exec_id_ed25519`     | Private key authorized on the guest for `KONTUR_EXEC_USER`. The default is written there by `kontur run`, which generates a fresh keypair for each guest it boots and passes the public half on the kernel command line (see `internal/guestkey`); the reference guest image installs it before sshd starts. A custom `CHV_DISK_IMAGE` either carries the same `kontur-authorized-key` service, or authorizes a key of its own and points this at its private half. |
| `KONTUR_EXEC_CONNECT_TIMEOUT`  | no       | `30s` (Go duration syntax)        | How long to keep retrying the initial connection -- the guest's `sshd` may not be up yet immediately after the container starts. |

This depends on `CHV_NET` actually giving the guest a reachable address
(e.g. via `netshim`, as `konturctl vm` sets up automatically) -- a guest
booted with no network device at all has no path in for `kontur exec`
either.

## Shimming sh and bash

`docker exec`/`kubectl exec` resolve the command they're given by name
inside the container's own filesystem and run that directly -- never
through the container's entrypoint -- so without `kontur exec` above, an
end user (or a tool built on top of `docker`/`kubectl` that isn't aware
of kontur specifically) reaching for the shell that's normally just
*there*, e.g.:

```sh
kubectl exec -it <pod> -c <container> -- sh
docker exec -it <container> bash -c 'tail -n100 /var/log/app.log'
```

would just get "executable file not found in $PATH": the container ships
no shell of its own (see "Building" below). `/bin/sh`, `/bin/bash`, and
their `/usr/bin` equivalents are symlinks to the same `kontur` binary
instead (see the Dockerfile's `final` stage), which tells them apart
from its four real modes by `argv[0]` (see `cmd/kontur`) and forwards
into the guest the same way `kontur exec` does.

This can't support arbitrary `sh`/`bash` invocations the way a real
shell binary would -- there's no way to plumb an arbitrary local argv
through the guest's SSH `ForceCommand` wrapper, which only ever receives
a single command string (see `internal/guestexec`'s `ShellCommandLine`).
Only two shapes are recognized: no arguments at all (an interactive login
shell, the same as bare `kontur exec`), and `-c <command>` (optionally
with other short flags fused in front of the `c`, e.g. `-ec`), which
covers what `docker exec`/`kubectl exec`-based tooling actually generates
in practice. Anything else -- a script file argument, positional
arguments after `-c`'s command, `--login` on its own, and so on -- is
reported as an error naming `kontur exec` as the alternative that does
support it, rather than silently behaving unlike a real `sh`/`bash`
would.

## Building

```sh
docker build -t kontur .
```

The `cloud-hypervisor` release version and checksums are pinned via
`CLOUD_HYPERVISOR_VERSION` and the `sha256` values in the `Dockerfile`'s
`fetch-chv` stage; bump both together when upgrading.

The `kontur` image's own base is fixed to `scratch` and isn't a build
option: both `kontur` and `cloud-hypervisor` are statically linked, and
`netshim` mode sets up the bridge/taps/NAT rules via netlink/nftables Go
libraries rather than exec'ing `ip`/`iptables`, so nothing here needs a
real userland to run in -- there's no smaller-but-still-working option to
trade off against, unlike the guest disk image below. The cost is
`ip`/`iptables`-style debuggability: there's no shell or package manager
in the final image to `apk add` a debugging tool into.

The guest disk image baked into `kontur` (not the `kontur` image's own
base above) *is* configurable, via `GUEST_DISTRO`: `debian` (the
default, via `debootstrap`) or `alpine` (via `apk`) --

```sh
docker build --build-arg GUEST_DISTRO=alpine -t kontur .
```

-- trading off the same way `BASE_DISTRO` used to for the outer image:
the Alpine guest is smaller (its `disk.img` comes out roughly an order
of magnitude smaller than the Debian guest's) at the cost of being a
less commonly-run-as-a-guest distro. See
[`deploy/guest-image/README.md`](deploy/guest-image/README.md) for the
guest disk image build (`guest-image`/`guest-rootfs-*` stages), how the
two variants differ beyond package manager, and their own build args
(`GUEST_SUITE`/`GUEST_ALPINE_VERSION`, `GUEST_SSH_AUTHORIZED_KEY`,
`GUEST_SETUP_SCRIPT`, `GUEST_KERNEL_PACKAGE`, `GUEST_CONSOLE_WRAP`).

`GUEST_SETUP_SCRIPT` is worth calling out here, since it's what makes
that guest more than a reference one: it holds a shell script's own text,
run inside the guest rootfs at build time (in the `guest-customized`
stage, as an ordinary `RUN` rather than under `chroot` -- see that README
for why the distinction matters), so packages and config files can be
baked into `disk.img` without maintaining a separate guest image build.
Paired with the `guest-artifacts` target --

```sh
docker build --target guest-artifacts --output type=local,dest=./out .
```

-- which exports `disk.img` plus the guest's own `vmlinuz`/`initrd.img`
when a setup script installed a kernel package, one `docker build`
produces everything needed to boot a customized guest elsewhere. `final`
is still the last stage, so the default build target is unaffected.

## Running locally

```sh
docker run --rm --device=/dev/kvm kontur
```

`CHV_DISK_IMAGE` and `CHV_KERNEL` are both left unset here, so this boots
the reference guest disk image and kernel baked into `kontur` itself --
see "Guest disk image and kernel" below for what that gets you (mainly:
SSH in, once `CHV_NET`/`netshim` gives it a reachable address, and its
session output shows up right here in the container's own output). Point
`CHV_DISK_IMAGE`/`CHV_KERNEL` at paths under a mounted volume instead to
boot a different guest:

```sh
docker run --rm \
  --device=/dev/kvm \
  -e CHV_DISK_IMAGE=/images/disk.img \
  -e CHV_KERNEL=/images/vmlinux \
  -v /path/to/images:/images:ro \
  kontur
```

## Guest disk image and kernel

The disk image baked into `kontur` at `/var/lib/kontur/guest/disk.img`
(built by the `Dockerfile`'s `guest-image` stage, from whichever of the
`guest-rootfs-debian`/`guest-rootfs-alpine` stages `GUEST_DISTRO`
selected -- see "Building" above) is a minimal Debian or Alpine system
with `sshd`, meant as a reference/demo guest usable without managing a
separate disk image -- not a production guest; bring your own
`CHV_DISK_IMAGE` for that. Its `sshd` is configured so that every SSH
session's output is also mirrored onto the guest's serial console
(`/dev/console`, i.e. `ttyS0`) in addition to reaching the actual SSH
client: since `kontur run`'s stdout/stderr *is* that console (`kubectl
logs` on the VM container), this is what makes SSH activity on the guest
observable the same way everything else about the VM already is, without
a separate log shipper inside it. See
[`deploy/guest-image/README.md`](deploy/guest-image/README.md) for
exactly how (the `ForceCommand`/`script` wrapper), how to get SSH access
at all (root has no password; `GUEST_SSH_AUTHORIZED_KEY` bakes in a key
at build time, alongside the per-boot keypair `kontur run` generates for
`kontur exec`'s own use -- see "Execing into a VM" above), and how host
keys avoid being shared across every VM booted from the image.

The kernel baked in alongside it, at `/var/lib/kontur/guest/vmlinux`
(built by the `Dockerfile`'s `fetch-kernel` stage), is a pinned
`cloud-hypervisor/linux` release build -- the same known-good, PVH-entry
kernel with virtio-pci/virtio-blk/virtio-net/virtio-mem already built in
that this repo's own testing has used for direct kernel boot elsewhere
(see "Validated on a real VM with nested virtualization" below), just
fetched and checksummed at image build time (`KONTUR_KERNEL_VERSION`
build arg) instead of at pod start. It's what actually boots the bundled
disk image above; bring your own `CHV_KERNEL` (or `CHV_FIRMWARE`, for a
guest with its own bootloader -- the bundled reference guest has none) if
your own guest needs a different one.

## Running on Kubernetes

See [`deploy/k8s/pod-example.yaml`](deploy/k8s/pod-example.yaml) for a
worked example. Two things any deployment needs to account for:

- **KVM access**: the node needs `/dev/kvm` (nested virtualization enabled,
  if the cluster itself runs on VMs) and the container needs access to it —
  either through a device plugin such as
  [`kubevirt/kubernetes-device-plugins`](https://github.com/kubevirt/kubernetes-device-plugins)'s
  `kvm` plugin (preferred: no elevated pod privileges needed), or by running
  the container `privileged: true` with `/dev/kvm` bind-mounted.
- **Graceful termination**: set `terminationGracePeriodSeconds` comfortably
  above `CHV_SHUTDOWN_TIMEOUT` so Kubernetes doesn't `SIGKILL` the container
  before `kontur run` finishes its own shutdown sequence.

See [`deploy/k8s/gke.md`](deploy/k8s/gke.md) for a GKE-specific,
tested walkthrough (enabling nested virtualization on the node pool,
pushing the image, and a self-contained
[`gke-pod-example.yaml`](deploy/k8s/gke-pod-example.yaml) that needs no
other cluster setup).

## Local testing with a static kubelet

See [`deploy/static-kubelet/`](deploy/static-kubelet/README.md) for
scripts that install a standalone kubelet (containerd + kubelet running
static pods from a manifest directory, no apiserver) on a single node,
plus a local image registry so `kontur` can be built and run there
straight from a working tree.

## Pod-local networking (`netshim` mode)

A pod normally gets one IP, but a pod can run several VMs -- one per
`run`-mode container -- each of which wants its own inbound listener.
`netshim` mode, meant to run once as an init container, sets this up:

- A Linux bridge (`kontur0` by default) inside the pod's network
  namespace.
- One tap device per VM, attached to that bridge, named `tap-<name>` for
  a VM called `<name>`.
- nftables DNAT rules (via the `google/nftables` Go library, in a
  dedicated `kontur` table) forwarding each VM's own external port on the
  pod IP to a single fixed port inside that VM (`NETSHIM_GUEST_PORT`),
  plus a MASQUERADE rule so VM-initiated outbound traffic leaves via the
  pod's own IP.

Because init containers share the pod's network namespace with the
containers that follow them, everything `netshim` mode creates is already
in place by the time the VM containers start. Each VM container then just
needs `CHV_NET=tap=tap-<name>` (matching the name it was given in
`NETSHIM_VMS`) -- the tap already exists and is attached to the bridge, so
cloud-hypervisor uses it as-is rather than creating and configuring a new
one. The guest itself is responsible for configuring the same IP
`netshim` mode was told about (e.g. via a kernel `ip=` boot parameter in
`CHV_CMDLINE`) and for listening on `NETSHIM_GUEST_PORT`; `netshim` mode
only sets up the host side.

| Variable                 | Required | Default        | Description |
|---------------------------|:--------:|-----------------|--------------|
| `NETSHIM_VMS`             | yes      | —               | Comma-separated `name:ip:port` entries, one per VM, e.g. `web:169.254.100.2:30080,worker:169.254.100.3:30081`. |
| `NETSHIM_BRIDGE`          | no       | `kontur0`       | Name of the bridge to create. |
| `NETSHIM_BRIDGE_CIDR`     | no       | `169.254.100.1/24` | The bridge's own address and, implicitly, the subnet every VM's `ip` in `NETSHIM_VMS` must fall within. |
| `NETSHIM_EXTERNAL_IFACE`  | no       | `eth0`          | The pod's primary interface, i.e. the one carrying the pod IP. |
| `NETSHIM_GUEST_PORT`      | no       | `80`            | The single port every VM is expected to listen on internally. |

`netshim` mode is idempotent: if Kubernetes retries a failed init
container, a second run leaves the same bridge/taps/rules in place rather
than erroring on things that already exist.

## Flat mode (`NETSHIM_MODE=flat`)

The NAT mode above shares one namespace's IP between several VMs. Flat
mode does the opposite: exactly one VM per network namespace, spliced
directly onto whatever segment the container runtime already put that
namespace on, where it takes over the address *and* MAC the runtime
assigned. From outside there is still exactly one endpoint, so the VM
behaves like an ordinary container -- `-p`, `--network`, compose
membership, Kubernetes Services -- because all of those are properties of
the sandbox rather than of anything `netshim` installs.

What it builds:

- One tap, `tap-<name>`, with its MTU copied from the external interface.
- A **splice**: on each of the external interface and the tap, an ingress
  qdisc plus a match-everything filter whose action is `mirred egress
  redirect` at the other one. Redirect steals the frame rather than
  cloning it, so nothing is copied and no frame leaves the kernel.
- Optionally a control link -- a bridge holding `NETSHIM_CONTROL_CIDR`
  plus a `ctl-<name>` tap -- see below.

What it deliberately does *not* build: no `net.ipv4.ip_forward` write, no
nftables table, no routing and no NAT. `netshim` is control plane only in
both modes (it programs kernel state and exits; nothing of it is ever in
the data path), but flat mode leaves the kernel far less to do per packet
-- one tc action, instead of a conntrack lookup, a DNAT rewrite, a
routing decision and a bridge FDB lookup.

**Why a splice rather than a bridge.** A Linux bridge cannot have one MAC
on two ports -- the FDB entry would flap -- so bridging the tap to the
veth forces the guest onto a *different* MAC, and the segment then sees a
second endpoint appear behind a port it already authorized. That is
harmless on `docker0`, which learns whatever shows up, and dropped by the
port security enforced on most cloud fabrics and managed CNIs. A splice
has no forwarding database to confuse, so the guest can present the exact
MAC the runtime assigned and nothing upstream can tell the difference.
This is also why `kata-containers` defaults to its own `tcfilter`
internetworking model rather than the `bridged` one it deprecated.

The classifier is `u32` with no selectors (which matches everything)
rather than the more direct `matchall`: `matchall` needs
`CONFIG_NET_CLS_MATCHALL`, which plenty of minimal kernels leave out, and
its absence surfaces as a bare `ENOENT` from the filter add with nothing
to say the classifier was the missing piece.

**The guest's configuration is discovered, not passed in.** The VM
container shares the namespace, so it reads the address, MAC and MTU back
off the external interface itself and synthesizes its own `--net` (with
`mac=` and `mtu=`) and kernel `ip=` parameter at boot. That behaves
identically whether the sandbox came from `docker run`, a kubelet or
anything else, and leaves no second copy of the identity to drift out of
date. It works because `netshim` leaves that interface addressed: the
splice steals its ingress, so the namespace's own stack can never receive
a reply over it anyway. An explicit `ip=` in `CHV_CMDLINE` still wins, for
an operator overriding the derived identity on purpose.

**The control link.** Because the guest now answers to the namespace's
own address, anything *inside* the namespace dialing that address reaches
its own stack instead. `kontur exec` and the memory agent therefore need a
second, private NIC. `netshim` builds the host side (a bridge at
`NETSHIM_CONTROL_CIDR`, plus a `ctl-<name>` tap), and the guest brings its
own second interface up at the next address in that subnet
(`169.254.100.2` by default).

That last part cannot be derived from the boot command line the way the
first NIC is -- the kernel's `ip=` parameter configures a single
interface -- so it is fixed at image build time instead, the same way
`kontur-mem-agent`'s own target is. The reference guest image does it in
`kontur-control-net` (see `deploy/guest-image/`), a one-shot service that
no-ops when there is no second NIC, so the same image still boots
unchanged in NAT mode. A third-party guest image needs its own equivalent;
without one, flat mode still works, but nothing inside the namespace can
reach the guest.

Setting `NETSHIM_CONTROL_CIDR=""` omits the control link entirely, which
costs `kontur exec` and the memory agent and nothing else; `konturctl`
then leaves `KONTUR_EXEC_ADDR` unset rather than emitting an address
nothing answers on.

The memory agent needs the same treatment for a different reason: it
signals whichever host address it is given, defaulting to its own default
route's gateway. That default is the `kontur run` container's bridge
address in NAT mode and *wrong* in flat mode, where the default route
leads out to the container network. `kontur-control-net` therefore also
writes `KONTUR_MEM_AGENT_HOST` (see `cmd/kontur-mem-agent`) pointing at
the control link, which both guests' service definitions pick up.

| Variable                 | Required | Default        | Description |
|---------------------------|:--------:|-----------------|--------------|
| `NETSHIM_MODE`            | no       | `nat`           | `nat` for the shared-IP mode above, `flat` for this one. |
| `NETSHIM_VM`              | yes      | —               | The single VM's name, in place of `NETSHIM_VMS`: flat mode needs no address or port for it. |
| `NETSHIM_CONTROL_CIDR`    | no       | `169.254.100.1/24` | The control link's address and subnet. Empty disables the control link. |
| `NETSHIM_BRIDGE`          | no       | `kontur0`       | Name of the control link's bridge. |
| `NETSHIM_EXTERNAL_IFACE`  | no       | `eth0`          | The interface whose identity the guest takes over. |

Under `-backend docker`, flat mode also drops `netshim`'s `--privileged`:
with no `/proc/sys/net` write to make it needs only `--cap-add NET_ADMIN
--cap-add NET_RAW --device /dev/net/tun`. The device grant is not
optional -- the netlink library creates a tap by opening `/dev/net/tun`
rather than over rtnetlink, and docker's device cgroup denies it
otherwise. A pod has no per-device grant to give, so the static pod
manifest stays privileged in both modes.

Known gaps, none of which flat mode closes on its own:

- **DNS.** Docker's embedded resolver listens on `127.0.0.11`, the
  *namespace's loopback*, which is not on the wire -- so other containers
  resolve the VM by name, but the guest cannot resolve them.
- **IPv4 only**, as with the NAT path.
- **Single queue.** The tap is created without `IFF_MULTI_QUEUE`, so
  `num_queues` cannot be raised without also handing cloud-hypervisor
  file descriptors instead of a tap name.
- **No re-addressing.** Setup is one-shot, so if the runtime ever
  re-addresses the container the guest's `ip=` goes stale and nothing
  notices.


## Operating a node (`konturctl` CLI)

`cmd/konturctl` builds a single static binary, meant to run on the node
itself (not inside a container, unlike `kontur`), that wraps VM lifecycle
management into a day-to-day workflow, against either of two backends:

- `-backend static-pod` (the default): renders a static pod manifest for
  the standalone kubelet described in
  [`deploy/static-kubelet/`](deploy/static-kubelet/README.md) to run.
- `-backend docker`: runs the same containers directly against a local
  docker daemon instead -- no containerd, CNI or kubelet involved at all.
  See "Docker backend" below.

- `konturctl setup [-kubelet-version] [-static-pod-path]`: installs and
  starts the standalone kubelet described in
  [`deploy/static-kubelet/`](deploy/static-kubelet/README.md) -- containerd,
  the CNI plugin config, and kubelet itself. It's the same `install.sh`
  that directory documents, embedded into the `konturctl` binary so it
  can run from just that one binary, without a checkout of this repo on
  the node. Must run as root. Only needed for `-backend static-pod`.
- `konturctl vm create <name> -disk ... -ip ... -port ... [-backend static-pod|docker] [-net nat|flat] [flags]`:
  starts one VM (a `netshim`-mode init container plus a single `run`-mode
  container, the same shape as
  [`manifests/kontur-static-pod.yaml`](deploy/static-kubelet/manifests/kontur-static-pod.yaml))
  under the chosen backend. Under `-backend static-pod` this writes a
  manifest to the kubelet's static pod directory, and the standalone
  kubelet notices the new file and starts the pod within a few seconds --
  no `kubectl apply`, since there's no apiserver to apply to. Under
  `-backend docker` it runs `docker run` directly instead; see "Docker
  backend" below.
- `konturctl vm update <name> [flags]`: changes the flags given and
  leaves the rest as they were, keeping whichever backend the VM was
  created with (there's no `-backend` flag here -- migrating a running VM
  from one backend to the other isn't supported, delete and re-create
  instead). Under `-backend static-pod` this re-renders the VM's manifest
  and kubelet tears down and recreates the pod; under `-backend docker`
  `konturctl` itself does the equivalent teardown/recreate.
- `konturctl vm delete <name>`: removes a VM -- its manifest (kubelet
  tears the pod down) or its containers, whichever backend it was created
  with. Safe to run again if the VM is already gone.
- `konturctl vm list`: lists the VMs `konturctl` currently knows about,
  regardless of backend.

`-images-hostpath` (default `/var/lib/vm-images`) is always mounted
read-only under both backends -- it's a shared, node-local image cache
several VMs may read `-disk`/`-kernel`/`-firmware` from concurrently, so
it's never made writable.

A VM that needs a writable root filesystem gets one from `-disk-mode`,
which is `overlay` unless told otherwise: the VM's own container creates
a small qcow2 backed by `-disk` and boots the guest from that. The
guest's writes land in the overlay as new qcow2 clusters; anything it
hasn't written yet reads straight through to the shared image
underneath. Creating it costs a fixed few hundred KiB regardless of the
image's size, so booting a VM never copies a multi-gigabyte disk, and
several VMs on a node share one copy of it. The overlay is made once and
reused, so restarting a container keeps whatever the guest wrote;
removing the container discards it.

`-disk-mode=persistent` instead lets the guest write through to `-disk`
itself, which only makes sense for a VM that is the only one using that
image -- `konturctl guest build` is the case that wants it, since the
changes are the point. `-disk-mode=readonly` attaches the image
read-only.

Two things this used to require and no longer does: `-disk` had to live
under `-images-hostpath` (the overlay was created out here and needed a
host path to back onto), and `-disk-hostpath` named a directory to put
overlays in. Both are gone with the overlay moving inside the container;
`-disk-hostpath` is still accepted and ignored, and `-disk-readonly=false`
still means `-disk-mode=overlay`, which is what it always did.

Each VM's parameters (including which backend it uses) are saved as JSON
under `-state-dir` (default `/var/lib/kontur/vms`) so a later `update` or
`delete` only needs the flags that are actually changing, and doesn't need
`-backend` repeated. This is kept separate from the manifest directory
itself: kubelet's static pod source treats every file it finds under
`-static-pod-path` as a pod manifest, so nothing else can live there.

One VM maps to one pod (`-backend static-pod`) or one VM container plus
one small helper container (`-backend docker`, see below). This is a
deliberate simplification: the multi-VM
[`pod-example.yaml`](deploy/k8s/pod-example.yaml) shows several VMs
sharing one pod's IP through different external ports, which `konturctl
vm` doesn't support directly (VMs it manages are always independent, so
each can be created/updated/deleted on its own). Hand-write a manifest
after that example instead if VMs need to share a pod.

Build it with `go build -o konturctl ./cmd/konturctl`.

### Docker backend

`-backend docker` (see `internal/dockervm`) is for hosts that just want to
run kontur VMs with `docker run` -- no `konturctl setup`, no
containerd/CNI/kubelet at all, just a local docker daemon. It drives the
same `netshim`/`run` container pair a static pod manifest describes, via
the docker CLI directly:

```sh
konturctl vm create web -backend docker \
  -disk /images/disk.img -kernel /images/vmlinux \
  -ip 169.254.100.2 -port 30080
```

With `-net flat` (see "Flat mode" above) the VM instead takes over the
namespace's own identity, so there is no `-ip` or `-port` to give -- the
address comes from docker, and ports are published on the sandbox itself
the ordinary way:

```sh
konturctl vm create web -backend docker -net flat \
  -disk /images/disk.img -kernel /images/vmlinux \
  -docker-run-opt --network -docker-run-opt mynet \
  -docker-run-opt -p -docker-run-opt 8080:80
```

`-docker-run-opt` is repeatable and passes each value through verbatim to
the `docker run` that creates the namespace holder. It has to go there
rather than on the VM container: port publishing, network membership and
DNS all belong to the container that *creates* a network namespace, and a
container joining an existing one with `--network container:` cannot add
them afterwards. Passing the flag at all replaces whatever a previous
`vm create`/`vm update` saved, so it behaves like every other flag. It
applies in NAT mode too, which is what makes a NAT-mode VM's forwarded
port reachable from outside the host at all.

A Kubernetes pod's containers share a network namespace held open by the
pod sandbox for as long as the pod exists, which is what lets the
`netshim`-mode init container set up the tap/bridge/NAT that the VM
container then uses. Plain docker containers have no such sandbox, and a
container's network namespace disappears with it, so this backend starts
a third, otherwise-idle container per VM (`<name>-netns`, the same
`kontur` image with its entrypoint overridden to its own `sleep` mode --
see "How it works" above for why the image has no coreutils `sleep` of
its own to use instead) purely to hold that namespace open, and attaches
both `netshim` (`--network
container:<name>-netns`, run once to completion, same as an init
container) and the VM container to it the same way. `konturctl vm delete`
removes both; `konturctl vm update` tears both down and recreates them,
since there's no manifest for a kubelet to reconcile.

`netshim`'s container runs `--privileged` under this backend, same as the
static pod backend's own manifest (see "Pod-local networking" above and
`deploy/k8s/pod-example.yaml`): the `NET_ADMIN`/`NET_RAW` capabilities
alone are enough to create the tap/bridge/nftables rules, but not enough
for the `net.ipv4.ip_forward` write `netshim` needs for its
`MASQUERADE`/DNAT rules to actually forward traffic -- the container
runtime's default runc-style OCI spec mounts `/proc/sys/net` read-only
regardless of capabilities granted, and that turned out to be true of a
real standalone kubelet/containerd CRI pod just as much as plain `docker
run`, contradicting what an earlier version of this doc claimed (an
untested assumption, not something actually validated against a real
kubelet at the time). The VM container already ran `--privileged` under
both backends (for `/dev/kvm`), so this doesn't add a new class of
privilege escalation to the workflow, just extends it to one more
container.

Validated end-to-end against a real local docker daemon (`docker
version` 29.7.2): `konturctl vm create/update/delete` with `-backend
docker` correctly sequenced the netns-holder/`netshim`/VM containers,
`netshim` actually stood up a real bridge, tap device and `iptables`
DNAT/MASQUERADE rules inside the shared namespace (confirmed with `ip
addr`/`ip link`/`iptables -t nat -L` from inside the netns holder), and
`update`/`delete` correctly tore down and (for `update`) recreated both
containers. Booting an actual guest through this path was not
re-validated here (no kernel image was available in this environment)
but is exactly the same `kontur run` invocation the static pod backend
already makes, config-checked and smoke-tested against a real KVM guest
as described above.

That gap -- an actual guest boot through `-backend docker`, on a real
docker daemon and real KVM together -- was closed on a real GCE VM with
nested virtualization enabled (`--enable-nested-virtualization`, `vmx`
confirmed present) and Docker CE (`docker version` 29.7.2) installed.
Both net modes were exercised end to end, not just the container
sequencing: in NAT mode, `konturctl vm create -backend docker` booted the
bundled guest to `multi-user.target` with `sshd` up, and a real `ssh`
client on the host (not `kontur exec`, and not from inside the shared
network namespace) reached the guest's login prompt through the actual
DNAT rule `netshim` installed, on the external port `-port` published --
confirming the whole path from a real external client to the guest and
back, not just that the rule existed. The same guest also ran arbitrary
commands over `kontur exec` (`docker exec <vm> kontur exec -- <cmd>`) and
via the `sh`/`bash` shim (`docker exec <vm> sh -c '...'`), and shut down
cleanly on `docker stop` (SIGTERM). Flat mode was exercised the same way
-- `-net flat` with a published port (`-docker-run-opt -p -docker-run-opt
<port>:22`) -- and a real `ssh` client reached the guest through the
splice after it took over the namespace's own address, the same
external-client validation NAT mode got.

This found one real bug, now fixed: flat mode's guest-side control link
(`kontur-control-net`, see "Flat mode" below) never came up under this
guest image. `kontur exec` timed out dialing the control address, and
`kontur-mem-agent` never received its target either. The guest's second
NIC boots as `eth1`, which is what both `kontur-control-net`'s default
`KONTUR_CONTROL_IFACE` and (for the first NIC) the flat-mode `ip=`
kernel parameter assume -- but systemd-udevd's stock
`80-net-setup-link.rules` renames it to a slot-based name (`ens3`) before
`kontur-control-net.service` runs, so its `[ ! -e
/sys/class/net/eth1 ]` check silently found nothing and exited 0 without
configuring an address; `ip -brief addr` on the running guest showed
`ens3` present but `DOWN` with no address, and `/run/kontur-control-net.env`
was never written. The first NIC was unaffected only because the
kernel's own `ip=` autoconfiguration runs before udev renames it, and the
rename doesn't clear the address once applied. Fixed by masking
`80-net-setup-link.rules` for the Debian guest (a `/dev/null` symlink
under `/etc/udev/rules.d`, which overrides the same filename under
`/lib/udev/rules.d`) so every NIC keeps the kernel's own enumeration
order instead; confirmed the guest now reports `eth0`/`eth1` (not
`ens2`/`ens3`), the control link comes up at the expected address, `kontur
exec` reaches the guest immediately, and `kontur-mem-agent` picks up the
right target from `/run/kontur-control-net.env`. The full unprivileged
test suite and the privileged `internal/netshim` kernel tests
(`KONTUR_NETNS_TESTS=required`) were re-run on this same VM afterward and
all still pass.

## Benchmarks

See [`benchmarks/`](benchmarks/README.md) for startup-latency measurements
comparing `kontur run` invoked directly (no container) against the same VM
booted as a GKE pod, plus the scripts used to reproduce them.

## Development

```sh
go build ./...
go vet ./...
go test ./...

# The tests that actually touch the kernel (see below) skip without root.
# sudo resets PATH, so the toolchain is passed through explicitly.
sudo env "PATH=$PATH" KONTUR_NETNS_TESTS=required go test -count=1 ./internal/netshim/...
```

`.github/workflows/ci.yml` runs exactly that sequence -- `gofmt`, build,
vet, the unprivileged suite, then the kernel tests as root -- on every
pull request and on pushes to `main`, plus a second job building the OCI
image (see
"Building" above) for *both* guest variants, since the Debian and Alpine
rootfs stages carry separate overlays and building only one leaves the
other's changes uncompiled. The image build needs no privileges beyond an
ordinary `docker build`, which is a property the `Dockerfile` maintains
deliberately (see its `guest-customized` stage).

`internal/hypervisor`'s tests exercise the process/shutdown/suspend state
machine against a fake `cloud-hypervisor` stand-in (`testdata/fakechv`)
that speaks the same API, so they run without KVM; this covers that
`kontur run` drives the pause/snapshot/resume and restore-argument-
building sides of suspend/resume correctly, but not whether a real
`cloud-hypervisor` actually restores a snapshot the way fakechv's own
unconditional acks imply. `internal/netshim`'s integration tests go
further and exercise the real kernel through the same netlink/nftables Go
libraries `netshim` itself uses (no `ip`/`iptables` binaries needed): NAT
mode's bridge/tap/nftables setup, and flat mode's splice -- including a
genuine packet test that injects a frame on one end of a veth pair and
reads it back off the tap, and the reverse.

Those need `CAP_NET_ADMIN` and `/dev/net/tun`, and skip without them. That
default is necessary (they cannot run unprivileged) and it is a trap: a
package whose every kernel-touching test skipped still reports `ok`, and
skips are invisible without `-v`, so a green `go test ./...` says nothing
about whether the splice carries a packet. Setting
`KONTUR_NETNS_TESTS=required` turns each such skip into a failure naming
what was missing, which is how CI asserts they really ran rather than
passing quietly on a runner that could not exercise the kernel.
The Dockerfile and the resulting image have additionally been smoke-tested
against the real `cloud-hypervisor` binary under KVM (kernel + initramfs
boot, console streamed to stdout, and the SIGTERM → power-button →
forced-shutdown path, plus confirming the bundled guest disk image is
used automatically when `CHV_DISK_IMAGE` is unset). `netshim` mode has
been smoke-tested the same way: run against a real interface to set up
the bridge/taps/NAT for two VMs, then two `cloud-hypervisor` guests booted
on those taps and reached independently through their own external ports
on the single shared IP. The guest disk image has now also been
smoke-tested through an actual boot to a working SSH login over a real
network path: a purpose-built kernel (Linux 6.6 LTS, PVH entry plus
virtio-pci/virtio-blk/virtio-net, ext4, and everything `systemd` needs --
the benchmark suite's marker kernel is initramfs-only with no virtio-blk
driver, so it can't mount a real root filesystem) booted the bundled
`disk.img` under real KVM inside the actual `kontur` container image
(`CHV_DISK_IMAGE` left unset), reaching `multi-user.target` with
`kontur-ssh-host-keys.service` regenerating fresh host keys and
`ssh.service` up. With `netshim` mode wiring up the bridge/tap/NAT (two
VMs sharing one pod IP through their own forwarded ports, as above), SSH
with a key baked in via `GUEST_SSH_AUTHORIZED_KEY` reached each guest
independently, and both the `script`-recorded session transcript and the
command output showed up in that VM container's own `docker logs`,
confirming the console-mirroring wrapper works end-to-end and not just by
config inspection. Regenerated host keys differed between the two VMs, as
intended. Two side effects of this being a deliberately minimal
kernel/guest, not defects in the image itself: `systemd-sysctl.service`
fails to apply `kernel.pid_max=4194304` at the default `CHV_MEMORY_MB`
(the kernel caps the usable range based on available RAM), and graceful
ACPI power-button shutdown isn't handled without `logind`/`dbus` in the
guest, so `kontur run`'s SIGTERM path falls back to its forced-shutdown
step every time -- still exiting cleanly, just not via the graceful path.
The guest kernel used for that smoke test was, at the time, supplied
externally (`CHV_KERNEL` pointed at a hand-built kernel, since none was
baked into the image yet); with the guest kernel now baked in alongside
the disk image (see "Guest disk image and kernel"), the same boot -- to
`multi-user.target` with `ssh.service` up -- was re-run against the built
image with *both* `CHV_DISK_IMAGE` and `CHV_KERNEL` left unset (`docker
run --device=/dev/kvm kontur`), confirming `kontur run`'s own default now
resolves `CHV_KERNEL` to the image's baked-in
`/var/lib/kontur/guest/vmlinux` (visible in its own startup log line)
the same way `CHV_DISK_IMAGE` already defaulted to the bundled
`disk.img`, with no environment variables, hostPath, or init container
needed at all. Setting `CHV_KERNEL`/`CHV_FIRMWARE` together was also
re-confirmed to still fail fast with the existing "mutually exclusive"
error, and setting `CHV_FIRMWARE` alone was confirmed to skip the
`CHV_KERNEL` default and validate the firmware path instead -- both
`internal/config`'s default-resolution and its pre-existing validation
still compose correctly.
Memory hotplug (`internal/config`'s `CHV_MEMORY_HOTPLUG`/
`CHV_MEMORY_MAX_MB` and `hypervisor.BuildArgs`'s resulting `--memory`
argument) has been smoke-tested directly against the real
`cloud-hypervisor` v53.0 binary under KVM: booted with
`size=256M,shared=on,hotplug_method=virtio-mem,hotplug_size=1792M` (i.e.
`CHV_MEMORY_MB=256`, `CHV_MEMORY_MAX_MB=2048`), `vm.info` confirmed
cloud-hypervisor accepted that configuration as given, and a `vm.resize`
call with `desired_ram` for 1024 MiB (what `kontur resize
-memory-mb=1024` sends, see `hypervisor.APIClient.Resize`) returned `204`
and `vm.info` afterwards showed `hotplugged_size` grown to account for
the difference. This confirms the VMM-side wiring end to end; it does not
confirm a guest actually consuming the hotplugged memory, since that
needs a `CONFIG_VIRTIO_MEM` guest kernel and neither of this repo's own
kernels (the benchmark suite's marker kernel, or the purpose-built one
used for the guest disk image smoke test above) currently is one.
CPU hotplug (`internal/config`'s `CHV_CPUS`/`CHV_CPUS_MAX` and
`hypervisor.BuildArgs`'s resulting `--cpus` argument) has similarly been
smoke-tested directly against the real `cloud-hypervisor` v53.0 binary
under KVM, this time with a guest kernel (the benchmark suite's own
firecracker-ci `vmlinux-5.10.223`, booted with its initramfs, ACPI
enabled): booted with `boot=2,max=4`, a `vm.resize` call with
`desired_vcpus: 4` (what `kontur resize -cpus=4` sends, see
`hypervisor.APIClient.ResizeCPUs`) returned `204` and the guest's own
kernel log showed `CPU2 has been hot-added`/`CPU3 has been hot-added`,
confirming a real guest observes the new vCPUs, not just `vm.info`.
Shrinking back to 2 also returned `204`, and a second resize attempted
immediately after (before the guest had acknowledged the removal) came
back `429` with cloud-hypervisor's own `CpuManager(VcpuPendingRemovedVcpu)`
error, exactly as `ResizeCPUs`'s doc comment describes. Suspend was then
exercised on top of both cases -- pausing/snapshotting right after a
grow, and right after a shrink with the removal still pending -- and
restoring from each resulting snapshot came back `Running` at the
expected vCPU count with no leftover pending-removal state; see "CPU
hotplug" above for what this rules out.
`internal/setup` (which `konturctl setup` drives) is tested
against a fake `install.sh` stand-in (`fstest.MapFS`) that only records
what it was invoked with, so the wiring -- asset extraction, environment
variables, working directory -- is covered without needing root, apt, or
systemd; the real `install.sh` it runs in production is the one already
validated above. `internal/staticpod` and `internal/cli`'s tests cover
manifest rendering (including YAML-unsafe characters in flag values) and
the create/update/delete/list lifecycle end-to-end against a temp
directory, including that an auto-derived `CHV_CMDLINE` is recomputed
when `-ip` changes on `update` but an explicitly-given one is left alone.
`konturctl setup` itself (i.e. `install.sh` end-to-end via the embedded
copy) has not been re-smoke-tested beyond what's already validated in
`deploy/static-kubelet/README.md`, since running it for real reconfigures
system-wide containerd/kubelet state on whatever host runs it.
`internal/dockervm`'s tests exercise the same construction/error-handling
logic against a fake `docker` CLI stand-in (`testdata/fakedocker`, same
technique as `fakechv`) that records every invocation, so the exact
sequence and arguments of the `docker` commands `-backend docker` issues
are covered without a real daemon. That backend has additionally been
smoke-tested end-to-end against a real local docker daemon, described
under "Docker backend" above.

The `kontur` image's now-fixed Alpine base was originally built and
smoke-tested end-to-end against a real local docker daemon alongside a
Debian build (back when the base was still a `BASE_DISTRO` build-arg
choice rather than fixed): both `docker build` succeeded (Alpine's
compressed image came out about 120MB smaller), `file`/`cloud-hypervisor
--version` confirmed the statically-linked `kontur` and `cloud-hypervisor`
binaries run unmodified under Alpine's musl libc, and `kontur run` failed
fast with the expected error on a missing `CHV_KERNEL` path just as it
did on Debian. `netshim` mode was exercised the same way as the "Docker
backend" validation above -- a netns-holder container plus a
`--privileged` `netshim` container built from the alpine image -- and
produced the same bridge/tap/DNAT/MASQUERADE state confirmed with `ip
addr`/`iptables -t nat -S`, showing Alpine's `iptables` package
(`nf_tables` backend) is compatible with the exact rules `netshim`
installs.

The `GUEST_DISTRO=alpine` guest disk image was built with `docker build
--build-arg GUEST_DISTRO=alpine` alongside the default Debian build, and
the resulting `disk.img` (about 90MB vs. the Debian guest's roughly
390MB, both sized per the Dockerfile's 20%-headroom-plus-64MiB-floor
rule against a 22MB Alpine rootfs vs. a 264MB Debian one) was inspected
directly with `debugfs` rather than booted: the console-wrapper and
`powerbtn.sh` scripts carried their executable bit, `/etc/apk/repositories`
was populated for the pinned `GUEST_ALPINE_VERSION`, no SSH host keys
were present (Alpine's `openssh-server` package doesn't generate them at
install time the way Debian's does, and its own `sshd` OpenRC init
script regenerates whatever's missing on every start, so this guest
needs no equivalent of `kontur-ssh-host-keys.service` at all -- see
`deploy/guest-image/README.md`), and the `sysinit`/`boot`/`default`
runlevels contained exactly the hand-enabled services the Dockerfile
symlinks in (`devfs`/`dmesg`/`mdev`/`hwdrivers`,
`hwclock`/`modules`/`sysctl`/`hostname`/`bootmisc`/`syslog`, and
`local`/`sshd`/`acpid` respectively). A full guest boot under KVM was not
run for the Alpine guest specifically: the only kernel available in this
environment (the benchmark suite's PVH marker kernel, see `benchmarks/`)
has no virtio-blk driver and so can't mount *any* real root filesystem,
Alpine or Debian's -- the same limitation noted for it elsewhere in this
file. The `guest-image` stage's own logic (sizing and `mke2fs -d`
packing) and everything downstream of `disk.img` in `kontur`/
cloud-hypervisor (attaching, booting, console streaming) don't
distinguish between the two rootfs images beyond their size, and are
exactly what the existing Debian-based KVM boot smoke test above already
covers.

`netshim`'s move from exec'ing `ip`/`iptables` to the `vishvananda/netlink`
and `google/nftables` Go libraries (and the outer `kontur` image's
resulting move from `alpine:3.20` to `scratch`) was validated beyond
`TestSetup_Idempotent` (which only asserts real interfaces/rules get
created and that re-running doesn't duplicate them) with an actual
end-to-end packet test: a throwaway `kpod` network namespace connected to
the root namespace by a veth pair standing in for the pod's `eth0`, with
`netshim.Setup` run inside it exactly as it runs in production. `nft list
ruleset` inside `kpod` showed the expected `dnat`/`masquerade`/`accept`
rules; a real `curl` from the root namespace to the "pod IP" landed, after
DNAT, on a plain `nc` listener bound to the VM's bridge address,
delivering the actual HTTP request bytes; and a connection opened from
the VM's bridge address outward was observed by a listener in the root
namespace arriving from the pod IP, confirming MASQUERADE. Separately, the
full multi-stage `Dockerfile` was built end-to-end against `scratch` (no
`apk add` of anything in the final stage) and `docker run --privileged`
against the resulting image, with `NETSHIM_EXTERNAL_IFACE`/
`NETSHIM_BRIDGE_CIDR`/`NETSHIM_VMS` set, successfully created the bridge,
tap and nftables rules with no userland present beyond the `kontur` and
`cloud-hypervisor` binaries themselves.

### Validated on a real VM with nested virtualization

The rest of this section's smoke tests either ran without a bootable
kernel+virtio-blk/virtio-net combination on hand, or (per
`deploy/static-kubelet/README.md`) inside a container standing in for a
node rather than a real one. This pass instead ran on an actual VM with
nested virtualization enabled (real `/dev/kvm`, confirmed `vmx`/`svm` CPU
flags), using cloud-hypervisor's own published PVH-entry kernel
(`cloud-hypervisor/linux`'s `ch-release-v6.16.9-*` release, which has
virtio-pci/virtio-blk/virtio-net built in) instead of hand-building one --
the same release now pinned and baked into the image itself by the
Dockerfile's `fetch-kernel` stage (`KONTUR_KERNEL_VERSION`), so this run
is what gave confidence that release was a suitable default in the first
place -- so it could go further: a full Debian and Alpine guest boot to a working
SSH login, `netshim`'s bridge/tap/DNAT forwarding a real SSH session
end-to-end, and the static pod backend exercised against a real
standalone kubelet (`deploy/static-kubelet/`) rather than by hand. It
found two real bugs, now fixed, and one earlier doc claim that turned out
to be wrong:

- The Debian guest rootfs (`guest-rootfs-debian`) didn't include `udev`:
  `debootstrap --variant=minbase` skips Recommends, and without
  `systemd-udevd` running, `dev-ttyS0.device` never got marked ready,
  stalling every boot for `DefaultDeviceTimeoutSec` (90s) with the
  console showing nothing but a spinner, after which
  `serial-getty@ttyS0.service` gave up -- so the console (this guest's
  only other point of entry besides SSH) never showed a login prompt at
  all. Fixed by adding `udev` to the `debootstrap --include` list (see
  `deploy/guest-image/README.md`); confirmed `dev-ttyS0.device` now
  resolves in well under a second and the guest reaches
  `multi-user.target` with a working console login and SSH both, in
  roughly 18s to `serial-getty@ttyS0.service` on 1 vCPU / 1024MiB.
- `netshim`'s init container, both in `internal/staticpod`'s generated
  manifests and in `deploy/k8s/pod-example.yaml`, ran with only the
  `NET_ADMIN`/`NET_RAW` capabilities added -- documented (incorrectly, it
  turns out) as sufficient under a real kubelet's CRI runtime, unlike the
  `-backend docker` path which was already known to need
  `privileged: true` for the same reason. Running the static pod backend
  against a real standalone kubelet + containerd (not a container
  standing in for one) reproduced the exact same failure `-backend
  docker` has: `open /proc/sys/net/ipv4/ip_forward: read-only file
  system`, since the container runtime's default runc-style OCI spec
  mounts `/proc/sys/net` read-only regardless of capabilities granted,
  on both runtimes alike. `deploy/static-kubelet/README.md`'s own
  "Validated" section had actually hit this same error before, but
  dismissed it as an artifact of testing inside a container standing in
  for a node rather than a real one -- that dismissal was wrong; it
  reproduces identically on a real node. Fixed by giving `netshim`
  `privileged: true` in `internal/staticpod/manifest.go`,
  `deploy/k8s/pod-example.yaml`, and
  `deploy/static-kubelet/manifests/kontur-static-pod.yaml`, matching what
  `-backend docker` already did; confirmed the static pod backend now
  runs `netshim` to completion and boots the VM container successfully
  against a real kubelet.
- Not a bug, but worth being explicit about since it wasn't obvious going
  in: `konturctl vm create`'s `-disk` flag is mandatory, and both
  backends mount `-images-hostpath` read-only into the container (a
  shared node-local image cache several VMs may read at once -- see
  `staticpod.Defaults`'s doc comment). That means a VM created through
  `konturctl` can never get a writable root filesystem, regardless of
  `-disk-readonly`, which in turn means the bundled reference/demo guest
  image's first-boot SSH host key generation (`ssh-keygen -A` in
  `kontur-ssh-host-keys.service`) can never succeed there: it fails to
  write the keys, doesn't treat that as fatal, and `sshd` then refuses to
  start at all ("no hostkeys available"). SSH into the bundled guest only
  works through the direct `kontur run`/`docker run` path described under
  "Running locally" above (which is how the SSH validation in this
  section was actually done), not through `konturctl` -- `konturctl` is
  built around production disk images that don't need to write to
  themselves, and the demo guest doesn't fit that mold. `DiskReadOnly`
  defaulting to `true` in `staticpod.Defaults()` is correct as-is; it was
  double-checked (and briefly, incorrectly, "fixed" to `false` during
  this pass) before landing back on `true`, since a writable default
  would have made `-disk-readonly=false` fail outright with "Read-only
  file system" the moment `-images-hostpath` is a real read-only mount,
  which it always is.

Also found and fixed as a smaller usability issue while doing the above:
`konturctl vm create`/`vm update` accepted VM names that produce a
`tap-<name>` device name over Linux's 15-character interface name limit,
which previously only surfaced as a `netshim` crash loop after the pod
was already submitted (`internal/netshim` itself already checked this,
just not early enough to help). `staticpod.Validate()` now checks it
upfront and fails the CLI command immediately with a clear message
instead.

Separately, running `deploy/static-kubelet/install.sh` on a node that
already has Docker CE installed removes it: the Debian `containerd`
package `install.sh` installs and Docker's own `containerd.io` package
both provide `/usr/bin/containerd` and the same `containerd.service`
unit, so `apt` resolves the conflict by removing `docker-ce`. This
directly undermines the "on a machine with Docker (can be the same
node)" local registry workflow `deploy/static-kubelet/README.md`
describes, unless the two are reinstalled together afterward (`apt-get
install docker-ce docker-ce-cli containerd.io`, keeping the containerd
config `install.sh` wrote when prompted) so `containerd.io`'s newer
binary ends up serving both Docker and the CRI socket kubelet uses, under
one config. See `deploy/static-kubelet/README.md` for the same note
closer to where it matters.

### Validated on GKE

Everything above ran on a real, generic nested-virt VM or a container
standing in for a Kubernetes node, never on Kubernetes' most common
managed form. This pass built the image and ran it on an actual GKE
Standard cluster instead, to check for platform-specific gotchas in
either direction (a managed node image behaving differently from a
hand-rolled nested-virt VM, or GKE's own restrictions blocking something
that worked elsewhere). See [`deploy/k8s/gke.md`](deploy/k8s/gke.md) for
the full walkthrough and exact findings; in short: no kontur code changes
were needed. `--enable-nested-virtualization` is a real, documented GKE
node pool flag that Just Works, `netshim` needed the same
`privileged: true` already documented for a standalone kubelet (GKE's
containerd masks `/proc/sys/net` read-only the same way), and the VM
container itself also needs `privileged: true` when using a `/dev/kvm`
hostPath rather than a device plugin -- confirmed by first getting this
wrong (a `/dev/kvm` bind mount with capabilities dropped and no
`privileged` opens the device node fine but fails `KVM_CREATE_VM` with
`Operation not permitted`, since the non-privileged pod's device cgroup
doesn't allow it through regardless of the file's own permissions).
`deploy/k8s/gke.md` also adds a fully self-contained example
(`gke-pod-example.yaml`) that needs no node-local image cache and no
init container at all -- the guest disk image and kernel it boots both
come straight from the `kontur` image itself -- so a single `kubectl
apply` is enough to see a VM boot on a fresh cluster with no other setup
(see `deploy/k8s/gke.md`'s own "Validated" section for how that example
has evolved since this pass, now that the kernel it once had to fetch at
pod start is baked in instead). Both that and the existing
`netshim`-networked `pod-example.yaml` shape were confirmed working,
including a real `kontur exec` SSH session reaching the guest and a
clean sub-3-second shutdown on pod deletion.

### Flat mode

The splice at the heart of flat mode (see "Flat mode" above) was
validated against the real kernel rather than only through the netlink
calls being accepted. `TestSplice_MovesFramesBothWays` builds a veth pair
standing in for a container's own interface, splices it to a tap, opens
the tap the way cloud-hypervisor would, and confirms a frame injected on
the network side arrives on the tap and a frame written to the tap
arrives on the network side -- both directions, on real devices.
`TestSetupFlat` covers the rest of the setup the same way: identity
discovery off an addressed interface, the tap's MTU copied from it,
exactly one ingress filter per device after two runs (the same
convergence a retried init container needs), and the control link's
bridge addressed with its tap enslaved to it.

That found one real portability bug before it could ship. The filter
started out using the `matchall` classifier, which expresses "match
everything" directly and is what the code reads most clearly as; the
development kernel rejected it with a bare `ENOENT`, because `matchall`
needs `CONFIG_NET_CLS_MATCHALL` and plenty of minimal kernels leave it
out. Nothing in that error names the classifier as the missing piece, and
it would have surfaced as flat mode simply not working on some hosts. It
now uses `u32` with no selectors, which is effectively always present and
is what `kata-containers` uses for the same job.

Not covered here, for want of the environment: no real docker daemon and
no KVM-capable host were available, so the `-backend docker` sequencing
in flat mode is exercised only against the `fakedocker` stand-in (as the
NAT path already is), and no guest has actually been booted on a spliced
tap. The pieces that would differ from the NAT path there are the
container's own flags and the two `--net` values, both asserted in unit
tests; the guest-side control link (`kontur-control-net`) has not been
run in a booted guest at all.
