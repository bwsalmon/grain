# Running on GKE

kontur needs nested virtualization (a real `/dev/kvm` inside the pod) to
run at all; everything else it needs to actually boot a VM (a guest disk
image and kernel) is already baked into the image itself (see the main
README's "Guest disk image and kernel"), so GKE only has to supply the
first. This is a concrete, tested walkthrough of that, ending in
[`gke-pod-example.yaml`](gke-pod-example.yaml) actually booting a VM.

## 1. Create a cluster with nested virtualization

```sh
gcloud container clusters create kontur-demo \
  --zone us-central1-a \
  --num-nodes 1 \
  --machine-type n2-standard-4 \
  --image-type COS_CONTAINERD \
  --enable-nested-virtualization
```

`--enable-nested-virtualization` is a real, first-class GKE node pool
flag (`gcloud container node-pools create --help`) -- no custom node
image or VMX license juggling needed, unlike plain GCE nested
virtualization. It requires either `COS_CONTAINERD` on GKE 1.28.4-gke.
1083000+ or `UBUNTU_CONTAINERD` (any version); every release channel
available at the time of writing satisfies that easily. Any nested-virt-
capable machine type works (N1/N2/N2D/C2 families, Haswell or later) --
`n2-standard-4` above is just comfortably enough for the 2 vCPU / 2GiB
example VM plus the container/kubelet overhead around it.

Confirm it worked: `/dev/kvm` should exist on the node with `crw-rw-rw-`
permissions (world-accessible -- no special group/capability needed to
open it, only to actually create a VM through it, which is what
`privileged: true` below is for).

## 2. Build and push the image

```sh
docker build -t kontur .
docker tag kontur us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest
docker push us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest
```

Whatever service account the node pool runs as (the default Compute
Engine service account, unless overridden) needs the Artifact Registry
Reader role (`roles/artifactregistry.reader`) on that repo to pull the
image -- already true by default for a repo in the same project as the
cluster in most setups.

## 3. Apply the pod

Substitute the pushed image into
[`gke-pod-example.yaml`](gke-pod-example.yaml) and apply it:

```sh
sed 's|IMAGE_PLACEHOLDER|us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest|' \
  gke-pod-example.yaml | kubectl apply -f -
kubectl logs -f kontur-gke-example
```

No other cluster setup is needed: that one manifest boots the guest disk
image and kernel already baked into the `kontur` image, and gets KVM
access via `privileged: true` + a `/dev/kvm` hostPath. The console log
should reach a login prompt within a few seconds of the container
starting.

`deploy/k8s/pod-example.yaml` -- the `netshim`-networked example the main
README points to, where the guest takes over the pod's own IP -- is the
same shape with two prerequisites this one deliberately drops: a KVM
device plugin providing `devices.kubevirt.io/kvm`, and a node-local
`/var/lib/vm-images` holding a disk image and kernel. Install those and
it runs here too; without them it stays `Pending` on the missing
resource. `gke-pod-exec-example.yaml` in the next section is the closer
equivalent -- the same `netshim` shape, with neither prerequisite.

## 4. Run something in the guest

The pod above boots a VM with no network of its own, which is not the
same thing as one you cannot reach: exec goes over the VM's own vsock
device, so `kubectl exec kontur-gke-example -- kontur exec -- uname -a`
already runs a command inside that guest, with no `netshim` and no
control link involved (verified on a cluster -- see "Validated" below).

What the pod above has no answer for is a guest that needs a *network*.
Apply [`gke-pod-exec-example.yaml`](gke-pod-exec-example.yaml) instead --
identical, plus the `netshim` init container, which gives the guest the
pod's own IP and MAC -- and the same `kubectl exec` reaches a guest that
can also talk to the rest of the cluster:

```sh
sed 's|IMAGE_PLACEHOLDER|us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest|' \
  gke-pod-exec-example.yaml | kubectl apply -f -
kubectl wait --for=condition=Ready pod/kontur-gke-exec-example --timeout=180s
kubectl exec kontur-gke-exec-example -c web -- kontur exec -- uname -a
```

The main README's "User flows" section walks that, guest customization
and copying files in and out through to the end, alongside the docker
equivalents.

Both manifests, exactly as they are checked in here, have since been
applied to a live single-node cluster and walked through all four of
those flows -- though not a GKE one; see "Validated" below for what that
does and does not settle.

## Validated

Two passes, on two different clusters, covering different halves of what
this file claims. The GKE-specific mechanics come from the first; the
manifests and the flows in their current form come from the second, which
did not run on GKE.

### On a real GKE Standard cluster

Both of the above were run end-to-end against a real GKE Standard
cluster (not Autopilot -- its restricted `securityContext` would reject
`privileged: true`) created exactly as described here. That run predates
the guest kernel being baked into the `kontur` image (`gke-pod-example.yaml`
then had its own `fetch-kernel` init container staging one into an
`emptyDir` first); the pod shape below reflects the now-simpler manifest,
but the underlying GKE mechanics it exercises -- nested-virt `/dev/kvm`
access, `privileged: true` requirements, image pull permissions -- are
unchanged by baking the kernel in and were not re-run against a live GKE
cluster for this change. What *was* re-verified for this change is the
kernel/disk defaulting itself (see the main README's "Testing" section):
a local `docker run --privileged --device=/dev/kvm` smoke test against
the built image, with neither `CHV_DISK_IMAGE` nor `CHV_KERNEL` set --
the same environment `gke-pod-example.yaml` now provides, minus GKE's own
scheduling -- reached the guest's login prompt.

- `gke-pod-example.yaml` as it was then: guest booted to a console login
  prompt under real nested-virt KVM, using the fetched-at-pod-start
  kernel and the bundled Debian guest disk, entirely from one `kubectl
  apply`.
- The full `pod-example.yaml` shape (one `netshim` init container plus
  one networked VM container, adapted to this cluster's staged kernel):
  guest reachable end-to-end, confirmed with
  `kubectl exec ... -- kontur exec -- <command>` running a real command
  inside the guest over the `netshim`-provided network path, and a clean
  pod deletion (SIGTERM) exiting in about 2s, well inside
  `terminationGracePeriodSeconds`.
- The `netshim` init container needed `privileged: true` (not just
  `NET_ADMIN`/`NET_RAW`) on GKE's `containerd`, matching the
  already-documented finding for a plain standalone kubelet in the main
  README's "Validated on a real VM with nested virtualization" --
  GKE's runtime masks `/proc/sys/net` read-only the same way.
- The VM container itself needs `privileged: true` too when using the
  `/dev/kvm`-hostPath approach (as opposed to a KVM device plugin): a
  `/dev/kvm` bind mount alone, without `privileged`, opens fine but fails
  `KVM_CREATE_VM` with `Operation not permitted` -- the device cgroup
  GKE's runtime sets up for a non-privileged pod doesn't allow it through
  even though the file's own permissions would.

No kontur code changes were needed -- everything here worked exactly as
already documented once the one GKE-specific piece (the node pool flag)
was in place.

### On a stand-in cluster, not GKE

The pass above predates exec moving off SSH onto vsock, and the main
README's "User flows" had never had its GKE column walked end to end at
all. Both were put right by running all four flows against the two
manifests in this directory exactly as they are checked in -- but on a
single-node Kubernetes v1.34.0 cluster created with `kind` v0.30.0 on a
nested-virt host with real `/dev/kvm` (Intel Xeon, `vmx`, Docker CE),
with the host's `/dev/kvm` mounted into the node container so the
manifests' own `hostPath` finds it, and the image built from this tree
and side-loaded onto the node rather than pulled. The machine that run
happened on had no GCP credentials, so a cluster of the shape section 1
describes could not be created for it.

So: a real apiserver, kubelet, containerd and CNI ran these manifests
unmodified, and everything in them that is about kontur was exercised for
real. Nothing GKE-specific was -- `--enable-nested-virtualization`, the
COS node image, GKE's device cgroup and an Artifact Registry pull all
still rest on the pass above, which is why that pass's findings are kept
above in full rather than folded into this one.

- Both manifests boot: `gke-pod-example.yaml` reached a guest login
  prompt in `kubectl logs`, and `gke-pod-exec-example.yaml` ran its
  `netshim` init container to completion and then its VM container, with
  `kubectl exec ... -c web -- kontur exec -- uname -a` answering about 8s
  after `kubectl apply`.
- **`kubectl exec ... -- kontur exec` works on the `netshim`-less pod
  too**, whose guest has no interface but `lo`. Exec is vsock now, not
  SSH over the control link, so section 4 above no longer says otherwise
  -- and `KONTUR_EXEC_ADDR`, which the code stopped reading when the SSH
  transport went, is gone from `gke-pod-exec-example.yaml`,
  `pod-example.yaml` and the static-kubelet manifest.
- **Neither manifest had hotplug headroom**, which made the main README's
  Flow 3 `kontur resize` line a no-op on either of them: `-memory-mb=1024`
  exited 0 and moved nothing (it was below the boot size), `-cpus=4` came
  back 500 "Requested vCPUs exceed maximum". Both now set `CHV_CPUS_MAX`
  and `CHV_MEMORY_MAX_MB` above their boot sizes, and pod `limits` sized
  for the ceilings; `kontur resize -memory-mb=2048 -cpus=4` then took the
  running guest from 1000828 kB to 2049404 kB of `MemTotal` and added
  `cpu2`/`cpu3`.
- **The guest holds the pod's IP, but pod-to-pod traffic did not work**
  on this cluster's `ptp` CNI: another pod could not reach the guest's
  nginx on the pod IP (the node could, `200`), and the guest could not
  reach another pod's IP either, while its gateway, the node and a
  ClusterIP were all fine. The main README's "Container networking"
  known gaps explains why -- `ip=` carries a netmask, `ptp` deliberately
  makes nothing on-link, and the CNI's own route is what the splice
  drops. GKE's node CNI is `ptp`-based too, so this is the first thing
  to measure on a real GKE cluster; nothing here shows whether it bites
  there.
- **`CHV_SETUP_SCRIPT` in a pod spec, booted rather than assumed**: its
  output appears in `kubectl logs <pod> -c web`, the file it wrote was
  there afterwards, the VM stayed up, and a script exiting non-zero
  failed the container (pod `Failed`) instead.
- Flow 4's copy pipelines through `kubectl exec -i`, in the `kontur
  exec` shape they had at the time (`kontur cp` landed after that run and
  has not been through `kubectl exec` on a cluster), round-trip a 100KB
  random binary byte for byte in both directions, extract a `tar` of a
  directory in the guest, and keep stderr separate from stdout with no
  CRLF rewriting -- none of the pty-era caveats in that flow's table
  apply to a stock guest any more.
- Flow 2's GKE half end to end: a `konturctl guest build` image built
  off-cluster, named in the manifest with nothing else changed,
  `systemctl is-active nginx` answering `active` in the pod. `kontur
  exec` starts working about 3s before the guest's own units finish
  starting (47s to first exec, 50s to `active` from `kubectl apply`), so
  that check wants a retry rather than a single shot.
- `kubectl delete pod --wait` returned in about 6s, well inside
  `terminationGracePeriodSeconds: 40`.
