# Startup benchmarks

Answers the question in bwsalmon/agents#247: how long does it take
cloud-hypervisor to boot with no container wrapper at all, and how much
does running the same thing as a GKE pod (kontur container + Kubernetes
scheduling) add on top?

## TL;DR

| Scenario                                            |   n | min   | median | mean  | max   |
|------------------------------------------------------|----:|------:|-------:|------:|------:|
| `kontur` binary, direct exec, no container/K8s        |  10 | 1.22s | 1.30s  | 1.29s | 1.35s |
| Same VM, as a GKE pod (image already cached on node)  |   8 | 2.24s | 2.28s  | 2.28s | 2.31s |

Both rows measure the same thing: wall-clock time from "go" to the guest
printing a ready marker on its console, for the same 1 vCPU / 256MiB VM
booting the same kernel+initramfs. Both ran on the *same* GKE node (a
freshly created `n2-standard-4`, see "Environment" below) -- the
standalone number came from SSHing onto the node and running the binary
directly (then named `chv-runtime`; see the top-level README for the
rename to `kontur`), specifically so the two numbers are comparable on
identical hardware.

The ~1s difference is almost entirely pod scheduling and container
creation, not cloud-hypervisor itself: on one representative pod, the
first runtime log line appeared 0.87s after `kubectl create` returned,
and the guest's BOOT_COMPLETE marker followed 1.29s after that --
essentially identical to the standalone VM-boot time above. See
"Breakdown" below.

A pod that has to pull the image for the first time on a node (not
covered by the table above, since it's a one-time cost per node rather
than a per-boot one) took ~3.6s end to end, of which the kubelet's own
`Pulling`/`Pulled` events accounted for ~1.9s (the image at the time was
two static binaries on `distroless/static`, about 6MB compressed). See
"Caveats / follow-ups" below: the image has changed substantially since
this run (it now bundles a full guest disk image and a non-distroless
base), so this particular number is stale and worth re-measuring.

## Environment

- GKE Standard, zonal, single node, created for this benchmark:
  `n2-standard-4`, `UBUNTU_CONTAINERD` image, `--enable-nested-virtualization`,
  us-central1-a. CPU: `Intel(R) Xeon(R) CPU @ 2.80GHz`. Node kernel
  6.8.0-1055-gke, Ubuntu 24.04.4 LTS.
- cloud-hypervisor v53.0 (the version currently pinned in the Dockerfile).
- Guest payload (see `kernel/build.sh`): the firecracker-ci project's
  published `vmlinux-5.10.223` (a generic PVH-entry kernel, not
  Firecracker-specific -- cloud-hypervisor's `--kernel` just needs a PVH
  entry point) plus a busybox initramfs whose `/init` mounts `/proc`,
  prints `BOOT_COMPLETE`, and powers off. This deliberately does no guest
  application work, so the number measured is VMM + kernel boot latency
  only, not e.g. sshd or cloud-init startup.
- 1 vCPU, 256MiB memory, no network device, no root disk (a 1MB
  placeholder disk is attached because kontur's "run" mode always attaches
  a boot disk; the guest never touches it since it boots from the
  initramfs instead).

## Why not benchmark "no container" on a random dev machine?

Early runs did exactly that, in a shared sandbox VM with nested
virtualization enabled, and got 10-16 *seconds* to the same marker, with
much higher run-to-run variance than either number above. Nested-KVM
boot latency turned out to be extremely sensitive to how contended and
how deeply nested the underlying host already is, which makes a number
from an arbitrary machine useless for deciding how much overhead the
container/Kubernetes layer itself adds. Once the standalone case was
re-run by SSHing onto the actual GKE node instead, it dropped to a
consistent ~1.3s and lined up with the boot-time component visible
inside the GKE pod numbers. Moral: always benchmark the "no wrapper"
baseline on the same hardware as the "wrapped" case, especially with
nested virtualization involved.

## Breakdown

From one representative pod's timestamps (`kubectl get pod -o json`,
`kubectl get events`, and `kubectl logs --timestamps`, all real UTC
timestamps recorded by the API server / containerd on the node):

| Step                                                        | Elapsed since `kubectl create` |
|---------------------------------------------------------------|------:|
| Pod scheduled                                                  | <1s (same second) |
| Container created and started, runtime execs cloud-hypervisor | 0.87s |
| Guest prints `BOOT_COMPLETE`                                    | 2.16s |

So: ~0.87s of Kubernetes-side overhead (API admission, scheduling,
kubelet pod sync, containerd container create/start) before
cloud-hypervisor even starts, then ~1.29s of VM boot -- matching the
standalone number almost exactly. kontur's "run" mode itself adds
negligible overhead on top of the raw `cloud-hypervisor` invocation
either way, since it just execs the VMM and streams its console.

## Reproducing

```sh
# 1. Build the guest payload (fetches a small kernel, builds an initramfs
#    with busybox already on PATH; requires cpio, gzip, busybox).
./kernel/build.sh

# 2. Standalone: build kontur and the pinned cloud-hypervisor binary,
#    then time N direct boots (needs /dev/kvm and sudo).
go build -o kontur ../cmd/kontur   # or extract from `docker build --target build`
curl -fsSL -o cloud-hypervisor <the pinned release asset from ../Dockerfile>
./standalone/run.sh 10 ./kontur ./cloud-hypervisor

# 3. GKE: build+push the kontur image, create a cluster with
#    --enable-nested-virtualization, stage kernel/build.sh's output onto a
#    node via stage-assets-job.template.yaml (substitute IMAGE_PLACEHOLDER
#    and HOSTPATH_PLACEHOLDER), render pod.template.yaml the same way into
#    pod.rendered.yaml, then:
NAME_PREFIX=kontur-bench ./gke/run.sh 8
```

Numbers above came from a cluster and Artifact Registry repo created
solely for this benchmark, both torn down afterward.

## Caveats / follow-ups

- This only measures a single VM with no network device and no
  netshim-mode init container. A pod running netshim mode plus one or
  more networked VMs would add init-container time on top (a
  bridge/tap/nftables setup) and is not covered here -- worth a follow-up
  if per-pod networking setup time turns out to matter in practice.
- The image built by this benchmark's Dockerfile has grown substantially
  since the ~3.6s cold-pull number above was measured: it now bundles a
  full guest disk image (see `../deploy/guest-image/README.md`) and a
  guest kernel (see the main README's "Guest disk image and kernel").
  Re-measure the cold-pull case if that number matters.
- Only tested with `/dev/kvm` via `privileged: true` + hostPath, not the
  `kubevirt/kubernetes-device-plugins` route the main README recommends
  for production; the device plugin shouldn't change VM boot time itself
  but could change pod admission latency.
- Single data point for the cold (image-not-cached) case; re-run if that
  number matters for capacity planning.
