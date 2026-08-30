# kontur

`kontur` is a single OCI image and Go binary that boots a single
[cloud-hypervisor](https://www.cloudhypervisor.org/) VM as its entrypoint,
configured entirely from environment variables so it can be driven
directly from a Kubernetes pod spec. It's meant for workloads where the
VM's disk image (and kernel, if using direct kernel boot) are already
present on the node, or where the reference guest image baked into the
`kontur` image itself is good enough -- this runtime never fetches images
at boot time, so there is nothing on the startup path except launching
the VMM.

The same image also sets up the pod-local networking that lets several
such VMs share one pod's IP, as an init container. See "How it works"
below for how one binary/image ends up serving both roles.

A separate binary, `konturctl`, is the operator-facing CLI: it runs on
the node itself (not inside a container) to install a standalone kubelet
and manage the VM pods `kontur` runs. See "Operating a node" below.

## How it works

The image contains three things:

- `kontur` (this repo, Go): a single binary with four modes, selected by
  the first argument (`run`, the default if none is given; `netshim`;
  `exec`; or `sleep`):
  - **`run`** reads configuration from the environment, execs
    `cloud-hypervisor` with the resulting arguments, streams the guest's
    serial console to the container's stdout/stderr so it shows up under
    `kubectl logs`, and turns `SIGTERM`/`SIGINT` into a graceful VM
    shutdown.
  - **`netshim`** sets up the pod-local networking (bridge, taps, NAT)
    that lets several VM containers in the same pod share the pod's IP;
    see "Pod-local networking" below. It's meant to run once, to
    completion, as an init container.
  - **`exec`** runs a command inside the VM guest itself, over SSH to its
    already-running `sshd`, rather than in this otherwise-empty
    container -- meant to be `kubectl exec`'s own command, so that ends
    up in the guest too; see "Execing into a VM" below.
  - **`sleep`** just blocks until killed. It exists purely so
    `-backend docker` (see "Operating a node" below) can hold a network
    namespace open with the `kontur` image itself, without needing a
    coreutils `sleep` binary the image doesn't otherwise carry.

  `netshim` talks to the kernel directly via netlink/nftables (see
  "Pod-local networking" below) rather than shelling out to `ip`/
  `iptables`, so none of the four modes needs anything beyond the two
  statically linked binaries themselves -- the image's base is `scratch`
  (see "Building" below).
- `cloud-hypervisor`: the actual VMM, fetched from the upstream static
  release build and pinned by checksum in the `Dockerfile`.
- A reference guest disk image (see "Guest disk image" below): a minimal
  Debian system with `sshd`, baked into the image so a VM container works
  out of the box without a separately-managed disk image.

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

- No image fetching: `CHV_DISK_IMAGE` (and `CHV_KERNEL`/`CHV_FIRMWARE`)
  are expected to already be on the local filesystem, e.g. via a hostPath
  or a PVC backed by a node-local image cache, or baked into the `kontur`
  image itself (see "Guest disk image").
- Direct kernel boot (`CHV_KERNEL`) skips firmware/UEFI entirely when the
  guest kernel is known ahead of time; only fall back to `CHV_FIRMWARE`
  (e.g. an OVMF/CLOUDHV build) for guests that need to run their own
  bootloader.

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

## Configuration

`kontur run`'s configuration is entirely via environment variables:

| Variable              | Required | Default                          | Description |
|------------------------|:--------:|-----------------------------------|--------------|
| `CHV_DISK_IMAGE`       | no       | the guest image baked into this image (`/var/lib/kontur/guest/disk.img`) | Path to the primary disk image. |
| `CHV_DISK_READONLY`    | no       | `false`                           | Attach the primary disk read-only. |
| `CHV_EXTRA_DISKS`      | no       | —                                 | Comma-separated additional disks: `path[:ro\|rw]`. |
| `CHV_KERNEL`           | one of `CHV_KERNEL`/`CHV_FIRMWARE` | — | Path to a kernel for direct boot (PVH/`vmlinux`). |
| `CHV_INITRAMFS`        | no       | —                                 | Path to an initramfs, used with `CHV_KERNEL`. |
| `CHV_CMDLINE`          | no       | `console=ttyS0 root=/dev/vda rw` | Kernel command line, used with `CHV_KERNEL`. |
| `CHV_FIRMWARE`         | one of `CHV_KERNEL`/`CHV_FIRMWARE` | — | Path to firmware (e.g. `CLOUDHV.fd`) for firmware boot. |
| `CHV_CPUS`             | no       | `2`                               | Boot vCPU count. |
| `CHV_MEMORY_MB`        | no       | `2048`                            | Guest memory, in MiB. |
| `CHV_MEMORY_SHARED`    | no       | `true`                            | Shared guest memory (required for some device backends). |
| `CHV_NET`              | no       | —                                 | Passed through verbatim as `--net`, e.g. `tap=eth0,mac=...`. |
| `CHV_API_SOCKET`       | no       | `/run/cloud-hypervisor/api.sock` | Path to the cloud-hypervisor API socket. |
| `CHV_BINARY_PATH`      | no       | `/usr/local/bin/cloud-hypervisor` | Path to the cloud-hypervisor binary. |
| `CHV_EXTRA_ARGS`       | no       | —                                 | Extra CLI args appended verbatim, space-separated (e.g. `--watchdog`). |
| `CHV_SHUTDOWN_TIMEOUT` | no       | `20s` (Go duration syntax)        | How long to wait for a graceful guest shutdown before forcing it. |

Every configured path -- `CHV_DISK_IMAGE` (including its default) and, if
set, `CHV_KERNEL`/`CHV_INITRAMFS`/`CHV_FIRMWARE` -- must already exist on
disk: `kontur run` checks this at startup and fails fast with a clear
error rather than letting cloud-hypervisor fail deeper into boot.

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
Since the container ships no shell of its own (see "Building" below),
`kontur exec` -- not e.g. `/bin/sh` -- is always the command
`kubectl exec`/`docker exec` itself needs to run.

| Variable                      | Required | Default                          | Description |
|--------------------------------|:--------:|-----------------------------------|--------------|
| `KONTUR_EXEC_ADDR`             | yes      | —                                 | The guest's own address, e.g. `169.254.100.2:22` -- the same address `CHV_CMDLINE`'s `ip=` configures on the guest, reachable directly since this container shares netshim's network namespace (see "Pod-local networking" below). `konturctl vm` sets this automatically. |
| `KONTUR_EXEC_USER`             | no       | `root`                            | The guest account to log in as. |
| `KONTUR_EXEC_KEY`              | no       | `/etc/kontur/exec_id_ed25519`     | Private key authorized on the guest for `KONTUR_EXEC_USER`. The default is a keypair generated at image build time (see the Dockerfile's `exec-keypair` stage) and authorized on the reference guest image regardless of `GUEST_SSH_AUTHORIZED_KEY`; a custom `CHV_DISK_IMAGE` needs to authorize the same public key itself (or point this at a key it does authorize) for `kontur exec` to work against it. |
| `KONTUR_EXEC_CONNECT_TIMEOUT`  | no       | `30s` (Go duration syntax)        | How long to keep retrying the initial connection -- the guest's `sshd` may not be up yet immediately after the container starts. |

This depends on `CHV_NET` actually giving the guest a reachable address
(e.g. via `netshim`, as `konturctl vm` sets up automatically) -- a guest
booted with no network device at all has no path in for `kontur exec`
either.

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
`GUEST_SETUP_SCRIPT`).

## Running locally

```sh
docker run --rm \
  --device=/dev/kvm \
  -e CHV_KERNEL=/images/vmlinux \
  -v /path/to/images:/images:ro \
  kontur
```

`CHV_DISK_IMAGE` is left unset here, so this boots the reference guest
image baked into `kontur` itself -- see "Guest disk image" below for what
that gets you (mainly: SSH in, once `CHV_NET`/`netshim` gives it a
reachable address, and its session output shows up right here in the
container's own output). Point `CHV_DISK_IMAGE` at `/images/disk.img`
instead to boot a different guest.

## Guest disk image

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
at build time, alongside a keypair generated for `kontur exec`'s own use
-- see "Execing into a VM" above), and how host keys avoid being shared
across every VM booted from the image.

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
- `konturctl vm create <name> -disk ... -ip ... -port ... [-backend static-pod|docker] [flags]`:
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
it's never made writable, regardless of `-disk-readonly`. A VM that needs
a genuinely writable root filesystem (to persist installed packages or
other state across reboots) instead passes `-disk-readonly=false`: before
the VM starts, `konturctl` creates a small qcow2 overlay file in its own
private directory under `-disk-hostpath` (default
`/var/lib/kontur/vm-disks/<name>`), with `-disk` itself as the overlay's
qcow2 backing file, and mounts that overlay read-write instead. The
guest's writes land in the overlay as new qcow2 clusters; anything it
hasn't written yet still reads straight through to the shared,
read-only `-disk`. Creating the overlay costs a fixed few hundred KiB
regardless of `-disk`'s size, unlike copying it. The overlay is created
once: a later `vm update` or container restart leaves it alone rather
than overwriting whatever the guest has since written to it, and
`vm delete` removes it. `-disk` must be a path under `-images-hostpath`
for this to work, since that's the only place `konturctl` ever mounts a
source image from.

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

## Benchmarks

See [`benchmarks/`](benchmarks/README.md) for startup-latency measurements
comparing `kontur run` invoked directly (no container) against the same VM
booted as a GKE pod, plus the scripts used to reproduce them.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

`internal/hypervisor`'s tests exercise the process/shutdown state machine
against a fake `cloud-hypervisor` stand-in (`testdata/fakechv`) that speaks
the same API, so they run without KVM. `internal/netshim`'s integration
test creates a real bridge/tap/nftables setup via the same netlink/nftables
Go libraries `netshim` itself uses (no `ip`/`iptables` binaries needed) and
is skipped unless run as root (`sudo go test ./internal/netshim/...`).
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
virtio-pci/virtio-blk/virtio-net built in) instead of hand-building one,
so it could go further: a full Debian and Alpine guest boot to a working
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
