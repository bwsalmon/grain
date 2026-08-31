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

`deploy/k8s/pod-example.yaml` -- the multi-VM, `netshim`-networked
example the main README points to -- also runs unmodified on a cluster
set up this way; `gke-pod-example.yaml` here is deliberately a simpler
single-VM version of the same thing, minus the node-local image cache
`pod-example.yaml` assumes is already populated.

## Validated

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
