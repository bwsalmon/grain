# Static kubelet

Scripts to turn a single Debian node into a minimal place to run kontur's
pods — a `netshim`-mode init container plus one or more `run`-mode VM
containers, both from the same `kontur` image — without standing up a
full Kubernetes cluster. There's no apiserver, no scheduler, no node
registration: just `containerd` and a `kubelet` running in **standalone
mode**, watching a directory of "static pod" manifests and running
whatever it finds there directly. This is the same mechanism real
clusters use to bootstrap `kube-apiserver`/`etcd`/etc. before an apiserver
exists to talk to; here it's repurposed as a lightweight way to exercise
the real kubelet/CRI/CNI machinery kontur's pods depend on (init
container ordering, capability grants, hostPath and device mounts,
`terminationGracePeriodSeconds`, ...) on a real node, without needing
cluster credentials or CI access to a cloud project.

## What's here

| File | Purpose |
|---|---|
| `install.sh` | Installs containerd + CNI plugins + kubelet and starts kubelet standalone. |
| `containerd-config.toml` | containerd config: CRI enabled, `SystemdCgroup`, and a mirror for the local registry below. |
| `kubelet-config.yaml` | `KubeletConfiguration`: static pod path, no apiserver auth, matching cgroup driver. |
| `kubelet.service` | systemd unit for kubelet (no `--kubeconfig`, which is what makes it standalone). |
| `cni/10-kontur.conflist` | Basic bridge CNI config so pods get a normal network namespace/IP; separate from and a prerequisite for `netshim`-mode's in-pod bridge. |
| `local-registry.sh` | Runs a local, unauthenticated image registry on `localhost:5000`. |
| `build-and-push.sh` | Builds `kontur` from this repo and pushes it to that registry. |
| `manifests/kontur-static-pod.yaml` | A static-pod version of `../k8s/pod-example.yaml`, pointed at the local registry. |

## Usage

```sh
# On the node, as root:
sudo ./install.sh

# On a machine with Docker (can be the same node):
./local-registry.sh
./build-and-push.sh

# Back on the node:
sudo cp manifests/kontur-static-pod.yaml /etc/kubernetes/manifests/
```

`install.sh` and hand-copying manifests is the from-source path. Once
`konturctl` (built from `cmd/konturctl` at the repo root) is on the node,
`sudo konturctl setup` does the same thing as `install.sh` -- it's the
same script, embedded in the binary -- and `konturctl vm
create/update/delete` manages VM manifests in the static pod directory
instead of hand-editing `manifests/kontur-static-pod.yaml`. See the
top-level README's "Operating a node" section.

Within a few seconds kubelet notices the new file, runs the `netshim`-mode
init container to completion, then starts the VM container. Watch it
with:

```sh
journalctl -u kubelet -f
crictl --runtime-endpoint unix:///run/containerd/containerd.sock ps -a
crictl --runtime-endpoint unix:///run/containerd/containerd.sock logs <container-id>
```

`kubectl` doesn't work here — there's no apiserver for it to talk to.
`crictl` (https://github.com/kubernetes-sigs/cri-tools) talks to
containerd's CRI socket directly and is the standalone equivalent for
inspecting pods/containers and reading logs.

To stop the pod, remove its manifest from the static pod directory;
kubelet tears it down the same way it would any other pod deletion.

## Why a local registry

containerd (via kubelet) pulls images the same way in standalone mode as
in a real cluster: it needs somewhere to pull `kontur` from. Pointing it
at `ghcr.io/bwsalmon/kontur` works if that tag is already published, but
for iterating on a local change before it's pushed anywhere,
`local-registry.sh` plus `build-and-push.sh` gives you a `localhost:5000`
registry that never leaves the node — `docker build` straight from a
working tree, `docker push`, and kubelet's copy of containerd pulls it
right back via the mirror configured in `containerd-config.toml`. This is
the only piece of "artifact repo" infrastructure this setup needs:
node-local disk images/kernels for `kontur`'s "run" mode itself still work
exactly as described in `../k8s/pod-example.yaml` (pre-populated under
`/var/lib/vm-images`, nothing fetched at runtime) -- omitting
`CHV_DISK_IMAGE` instead falls back to the reference guest image already
baked into the `kontur` image, see `../guest-image/README.md`.

For a fully offline node, skip the registry: `docker save` the built image
and `ctr -n k8s.io images import` it directly into containerd's store
instead of pulling from anywhere; then a `registry.k8s.io/pause` image and
the CNI plugin binaries need to have been vendored onto the node ahead of
time too, since those are still fetched/used normally.

## What differs from a real cluster deployment

- **No device plugin.** `deploy/k8s/pod-example.yaml` requests
  `devices.kubevirt.io/kvm` as an allocatable resource, which only exists
  because a device plugin registered it with the apiserver — there's no
  apiserver here. `manifests/kontur-static-pod.yaml` instead bind-mounts
  `/dev/kvm` via a hostPath and runs the VM container `privileged: true`,
  same as the fallback `pod-example.yaml` documents for clusters without
  that device plugin.
- **`restartPolicy: Always`.** Static pods require it. The ordinary
  example uses `Never` for a one-shot VM; here the VM restarts if it
  exits. Delete the manifest file to actually stop it.
- **No node conditions/eviction wired to anything real** (disk pressure,
  memory pressure, etc. are still computed, they just have no scheduler
  on the other end to act on them) and no RBAC/admission — this is a
  single-node debugging tool, not a substitute for testing against a real
  cluster before shipping.

## Validated

`install.sh`'s steps (containerd with the CRI plugin enabled, the CNI
config, kubelet fetch + checksum verification, standalone kubelet startup)
and the local-registry/build-and-push/static-pod flow above were run
end-to-end in a container-based stand-in for a node: `chv-runtime` and
`netshim` (this repo's binaries before they were merged into a single
`kontur` image with "run"/"netshim" modes -- see the top-level README)
were built from this repo, pushed to a local registry, and pulled back by
containerd through the mirror config; the standalone kubelet picked up
`kontur-static-pod.yaml` from its static pod directory with no apiserver
involved, created the pod sandbox with a real CNI-assigned IP, mounted the
hostPath volumes (including `/dev/kvm`), and started the netshim-mode init
container with the requested `NET_ADMIN`/`NET_RAW` capabilities. It ran
but failed inside that harness on `open
/proc/sys/net/ipv4/ip_forward: read-only file system` — an artifact of
nested containerization (the stand-in "node" was itself a container), not
something expected on a real node; netshim-mode establishing a real
bridge/NAT setup is already covered by the smoke test described in the
top-level README. This hasn't been re-run against real hardware/VM node
with a persistent systemd, which would be the next thing to confirm
`install.sh`'s systemd units specifically (rather than the equivalent
commands run by hand, as here), nor re-run since the merge into a single
`kontur` image.
