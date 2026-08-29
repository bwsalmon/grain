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

- `kontur` (this repo, Go): a single binary with two modes, selected by
  the first argument (`run`, the default if none is given, or `netshim`):
  - **`run`** reads configuration from the environment, execs
    `cloud-hypervisor` with the resulting arguments, streams the guest's
    serial console to the container's stdout/stderr so it shows up under
    `kubectl logs`, and turns `SIGTERM`/`SIGINT` into a graceful VM
    shutdown.
  - **`netshim`** sets up the pod-local networking (bridge, taps, NAT)
    that lets several VM containers in the same pod share the pod's IP;
    see "Pod-local networking" below. It's meant to run once, to
    completion, as an init container.

  Both modes need `ip`/`iptables` on `PATH` (only `netshim` uses them,
  but they're part of the same binary and image now), so unlike the
  distroless image this used to ship as when `run` and `netshim` were
  separate binaries, the base here is `debian:bookworm-slim`.
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

## Building

```sh
docker build -t kontur .
```

The `cloud-hypervisor` release version and checksums are pinned via
`CLOUD_HYPERVISOR_VERSION` and the `sha256` values in the `Dockerfile`'s
`fetch-chv` stage; bump both together when upgrading. See
[`deploy/guest-image/README.md`](deploy/guest-image/README.md) for the
guest disk image build (`guest-image` stage) and its own build args
(`GUEST_SUITE`, `GUEST_SSH_AUTHORIZED_KEY`).

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
(built by the `Dockerfile`'s `guest-image` stage) is a minimal Debian
system with `sshd`, meant as a reference/demo guest usable without
managing a separate disk image -- not a production guest; bring your own
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
at build time), and how host keys avoid being shared across every VM
booted from the image.

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
- `iptables` DNAT rules forwarding each VM's own external port on the pod
  IP to a single fixed port inside that VM (`NETSHIM_GUEST_PORT`), plus
  `MASQUERADE` so VM-initiated outbound traffic leaves via the pod's own
  IP.

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
`kontur` image with its entrypoint overridden to `sleep infinity`) purely
to hold that namespace open, and attaches both `netshim` (`--network
container:<name>-netns`, run once to completion, same as an init
container) and the VM container to it the same way. `konturctl vm delete`
removes both; `konturctl vm update` tears both down and recreates them,
since there's no manifest for a kubelet to reconcile.

One difference from the static pod backend worth knowing: `netshim`'s
container there only needs the `NET_ADMIN`/`NET_RAW` capabilities a real
kubelet's CRI runtime grants it (see "Pod-local networking" above). Plain
`docker run` is stricter -- it masks `/proc/sys/net` read-only regardless
of capabilities added, which blocks the `net.ipv4.ip_forward` write
`netshim` needs for its `MASQUERADE`/DNAT rules to actually forward
traffic -- so under this backend `netshim`'s container runs
`--privileged` instead. The VM container already ran `--privileged` under
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
test creates a real bridge/tap/iptables setup and is skipped unless run as
root with `ip`/`iptables` on `PATH` (`sudo go test ./internal/netshim/...`).
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
