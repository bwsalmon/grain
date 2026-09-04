# Static kubelet

Scripts to turn a single Debian node into a minimal place to run kontur's
pods — a `netshim`-mode init container plus one `run`-mode VM
container, both from the same `kontur` image — without standing up a
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
| `cni/10-kontur.conflist` | Basic bridge CNI config so pods get a normal network namespace/IP; a prerequisite for `netshim` mode, which splices the guest onto exactly that interface. |
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

If Docker is already installed on the node you're about to run
`install.sh` on: `apt-get install containerd` (what `install.sh` does)
and Docker's own `containerd.io` package both provide `/usr/bin/containerd`
and the same `containerd.service` unit, so `apt` resolves the conflict by
silently removing `docker-ce` -- which breaks the "can be the same node"
option above the moment it happens. If that's already bitten you,
`apt-get install docker-ce docker-ce-cli containerd.io` afterward brings
Docker back; when apt prompts about `/etc/containerd/config.toml` having
changed, keep the version `install.sh` wrote (it's the one with the CRI
plugin and the registry mirror this page depends on) -- `containerd.io`'s
newer binary then ends up serving both Docker and kubelet's CRI socket
under that one config, which is a fine end state, just one apt won't
reach on its own without that prompt answered.

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

`crictl` isn't installed by `install.sh`; grab it from
https://github.com/kubernetes-sigs/cri-tools/releases if it's not already
on the node. Container logs are also readable without it, straight from
`/var/log/pods/<namespace>_<pod>_<uid>/<container>/*.log` (what kubelet
itself writes them to), which is enough for a one-off look without
installing anything extra.

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
`CHV_DISK_IMAGE`/`CHV_KERNEL` instead falls back to the reference guest
disk image and kernel already baked into the `kontur` image itself, see
the main README's "Guest disk image and kernel".

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
/proc/sys/net/ipv4/ip_forward: read-only file system`, which was assumed
at the time to be an artifact of nested containerization (the stand-in
"node" was itself a container) rather than something expected on a real
node.

That assumption was wrong. This was re-run against a real VM node with
nested virtualization enabled and a persistent systemd (see the top-level
README's "Validated on a real VM with nested virtualization"), running
`install.sh` for real -- not the equivalent commands by hand -- against a
real standalone kubelet + containerd. The exact same `/proc/sys/net`
read-only failure reproduced identically: it's not a nested-containerization
artifact, it's that a real kubelet's CRI runtime masks `/proc/sys/net`
read-only for any non-privileged container regardless of capabilities
granted, same as plain `docker run` does. `internal/staticpod`'s generated
manifest (and `manifests/kontur-static-pod.yaml` in this directory) now
give `netshim` `privileged: true` instead of just those two capabilities,
and with that fix the static pod backend runs end-to-end against this
real standalone kubelet: `install.sh`, `local-registry.sh`,
`build-and-push.sh`, and `konturctl vm create/update/delete/list` (all
with `-backend static-pod`, the default) all worked as documented, the
netshim init container now completes successfully, and the VM container
boots a real guest under KVM (console output visible via
`/var/log/pods/.../*.log`, since `crictl` isn't installed by `install.sh`
itself -- see "Usage" above for that caveat).

`netshim` no longer makes that `/proc/sys/net` write at all: the NAT mode
that needed it is gone, and what is left splices the guest onto the pod's
own interface without routing anything (see the top-level README's
"Container networking"). It stays `privileged: true` in a pod all the
same, for a different reason -- the netlink library creates a tap by
opening `/dev/net/tun`, and a pod has no per-device grant to give it the
way `docker run --device` does.
