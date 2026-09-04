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

The same image also sets up that VM's networking, as an init container:
one VM per pod, spliced onto the pod's own interface so the guest takes
over the address and MAC the container runtime assigned it. See "How it
works" below for how one binary/image ends up serving both roles.

A separate binary, `konturctl`, is the operator-facing CLI: it runs on
the node itself (not inside a container) to install a standalone kubelet
and manage the VM pods `kontur` runs. See "Operating a node" below.

**Start with "User flows" below** if what you want is the commands: it
walks through running a one-off command in a VM, customizing the guest,
exec'ing into a running VM and copying files in and out, under both
docker and GKE. Everything after it is the reference material those
walkthroughs draw on.

## User flows

Four things people actually do with a kontur VM, each written out twice:
once against a plain docker daemon on a KVM-capable host, and once on
GKE.

- [Before you start](#before-you-start)
- [Flow 1: run a one-off command in a stock VM](#flow-1-run-a-one-off-command-in-a-stock-vm)
- [Flow 2: customize the VM](#flow-2-customize-the-vm)
- [Flow 3: exec a command on a running VM](#flow-3-exec-a-command-on-a-running-vm)
- [Flow 4: copy files to and from a running VM](#flow-4-copy-files-to-and-from-a-running-vm)
- [Rough edges](#rough-edges)

These are deliberately literal -- every command, in order, including the
ones that exist only to work around something kontur doesn't do for you
yet. "Rough edges" at the end collects those in one place, because that
list, not the flows themselves, is the thing worth shortening.

Everything below boots the reference guest disk image and kernel baked
into the `kontur` image itself, so there is no separate disk image to
build, host or stage on a node first (see "Guest disk image and kernel").
The sections after this one are the reference material behind each step;
each flow points at the relevant one rather than repeating it.

### Before you start

**Docker.** A host with `/dev/kvm` -- a bare-metal machine, or a VM with
nested virtualization enabled (on GCE, `gcloud compute instances create
--enable-nested-virtualization`; on GKE, the node pool flag in
[`deploy/k8s/gke.md`](deploy/k8s/gke.md)) -- and a docker daemon:

```sh
docker build -t kontur:latest .            # see "Building"
go build -o konturctl ./cmd/konturctl      # see "Operating a node"
sudo modprobe tun                          # netshim creates the guest's tap via /dev/net/tun
mkdir -p ~/.kontur/vms
```

`konturctl`'s own state directory defaults to `/var/lib/kontur/vms`,
which an unprivileged user can't create, so every `konturctl vm` command
below passes `-state-dir ~/.kontur/vms`. Forgetting it costs nothing but
the retry: `vm create`/`vm update`/`vm run` check that directory before
they start anything, and refuse with a message naming both the directory
and the flag rather than failing on the save with a VM already running.
Nothing else here needs root:
membership of the `docker` group is enough, since the VM's containers get
`/dev/kvm` and `/dev/net/tun` from docker rather than from the caller.

**GKE.** A cluster whose nodes have nested virtualization enabled, and
the `kontur` image pushed somewhere the nodes can pull it -- both are
[`deploy/k8s/gke.md`](deploy/k8s/gke.md), which is a tested walkthrough
of exactly those two steps. Then:

```sh
export KONTUR_IMAGE=us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest
sed "s|IMAGE_PLACEHOLDER|$KONTUR_IMAGE|" deploy/k8s/gke-pod-exec-example.yaml \
  | kubectl apply -f -
kubectl wait --for=condition=Ready pod/kontur-gke-exec-example --timeout=180s
```

[`gke-pod-exec-example.yaml`](deploy/k8s/gke-pod-exec-example.yaml) is
the manifest every GKE flow below uses: one `netshim` init container plus
one VM container, KVM via `privileged: true` and a `/dev/kvm` hostPath,
and no node-local image cache or device plugin to set up first. Its
sibling [`gke-pod-example.yaml`](deploy/k8s/gke-pod-example.yaml) is the
same VM with no networking at all -- fine for watching a guest boot in
`kubectl logs`, and still a VM you can run commands in, since exec
reaches the guest over its own vsock device rather than over guest
networking (`kubectl exec kontur-gke-example -- kontur exec -- uname -a`
works on it, verified on a cluster). What it lacks is a network for the
*guest*, which is the distinction the next paragraph draws.

**Why the flows go through `konturctl`/`netshim` rather than a bare
`docker run kontur`.** A bare `docker run` boots a VM with no NIC: its
console is on the container's stdout and that is the only way to observe
it. Everything below that reaches *into* the guest -- `kontur exec`, the
setup script, copying a file -- works on a bare `docker run` too: it goes
over the VM's own vsock device rather than over guest networking (see
"Execing into a VM"). What a bare `docker run` still lacks is a network
*for the guest*, which is what `netshim` mode builds; `konturctl vm
create -backend docker` runs it for you, as does the init container in
the GKE manifest.

`kubectl exec`/`docker exec` on a kontur container lands in the container
itself, which is a `scratch` image holding two static binaries -- no
shell, no `tar`, no workload. `kontur exec` is what bridges that gap, so
every flow below that runs something "in the VM" is really `docker exec
<container> kontur exec -- <command>`.

### Flow 1: run a one-off command in a stock VM

Boot a VM, run one command inside the guest, throw the VM away.

**Docker.** That whole shape is one command:

```sh
konturctl vm run oneoff -state-dir ~/.kontur/vms -- uname -a
```

`vm run` creates the VM, waits for its guest to become reachable
(`-ready-timeout`, 3 minutes by default), runs the command, exits with
*that command's* exit status, and deletes the VM -- whatever the command
did, including exiting non-zero, which is a result rather than a failure.
Only the guest command's output goes to stdout; everything `konturctl`
has to say about creating and deleting the VM goes to stderr, so
`konturctl vm run oneoff ... -- cat /etc/os-release > os-release` writes
the file you meant. `-keep-on-failure` leaves behind a VM whose guest
never came up, so `docker logs kontur-vm-oneoff` can say why.

One setting still has to be passed, since `konturctl`'s own default
doesn't pick it: `-state-dir`, because the default is a directory an
unprivileged user can't create (see "Before you start"). Everything else
is defaulted for you here -- `-backend docker` (rather than `vm create`'s
`static-pod`), `-disk-mode overlay`, and `-kontur-image kontur:latest`,
the image `docker build -t kontur:latest .` made.

**The longhand**, which is what `vm run` does and what to reach for when
the VM has to outlive the command:

```sh
konturctl vm create oneoff -backend docker -state-dir ~/.kontur/vms -wait

konturctl vm exec oneoff -state-dir ~/.kontur/vms -- uname -a

konturctl vm delete oneoff -state-dir ~/.kontur/vms
```

`-state-dir` is the one setting `konturctl`'s defaults don't pick for
you. The two that used to need passing here no longer do, and both are
worth knowing about because they follow `-backend`:

- `-kontur-image` defaults to `kontur:latest` under `-backend docker` --
  the image `docker build -t kontur:latest .` made, which the daemon
  already holds. Under `-backend static-pod` it is
  `localhost:5000/kontur:latest`, the node-local registry the
  standalone-kubelet setup installs (see
  [`deploy/static-kubelet/`](deploy/static-kubelet/README.md)), because
  containerd resolves images by reference and cannot see a local docker
  daemon's. Name your own image with the flag either way.
- `-disk-mode` defaults to `overlay`, which gives the guest a private
  writable disk (a thin qcow2 in its own container, backed by the
  image's disk -- see "Disk size"). It used to default to `readonly`,
  which is not enough for the reference guest to finish booting.

`konturctl vm create` returns as soon as the containers are started,
which is *before* the guest has finished booting -- hence `-wait`, which
holds the create until the guest answers a command (`-ready-timeout`, 3
minutes by default) and gives up early if the VM's container exits
instead of waiting out the timeout. `konturctl vm wait oneoff -state-dir
~/.kontur/vms` is the same wait for a VM already created without it, or
one being restarted.

Neither is compulsory: `kontur exec` retries the connection for
`KONTUR_EXEC_CONNECT_TIMEOUT` (30s by default) rather than failing on the
first refused dial, so a `vm exec` on the line after a `vm create` often
just works, and `konturctl vm exec oneoff -connect-timeout 3m -- ...`
stretches that window for a slower guest. What `-wait` buys is a script
that fails at the create when a guest never comes up, rather than at
whatever command happened to run next.

The VM's container is named `kontur-vm-<name>` -- `vm exec` derives that
for you, but it is what to hand `docker logs -f kontur-vm-oneoff` for the
guest's console. `konturctl vm list -state-dir ~/.kontur/vms` shows what
exists.

If all you want is to watch a stock guest boot, with nothing to run
inside it, the whole flow is one command and no `konturctl`:

```sh
docker run --rm --device=/dev/kvm kontur:latest
```

**GKE.** With the pod from "Before you start" already applied:

```sh
kubectl exec kontur-gke-exec-example -c web -- kontur exec -- uname -a
kubectl delete pod kontur-gke-exec-example
```

`kubectl wait --for=condition=Ready` returns when the *container* is
running, which -- exactly as under docker -- is before the guest has
booted; `kontur exec`'s own retry window is again what covers the
difference, and `kubectl logs -f kontur-gke-exec-example -c web` is the
guest's console while it does.

There is a third way to run something once, which is neither of the
above: `CHV_SETUP_SCRIPT` on the VM container runs a shell script inside
the guest once, right after boot, before anyone execs anything (see
"Suspend and resume"). It fits a pod spec, where you can set arbitrary
environment:

```yaml
        - name: CHV_SETUP_SCRIPT
          value: |
            set -eux
            uname -a
```

Its output goes to the container's log, and a non-zero exit shuts the VM
down and fails the container -- but a *successful* one leaves the VM
running, so it provisions a VM rather than terminating like a Job. Pair
it with `CHV_SNAPSHOT_PATH` to make the next boot resume the finished
state instead of re-running the script.

It runs over the same path `kontur exec` uses, which is the VM's own
vsock device -- so unlike earlier versions of kontur it needs no
`netshim` and no control link, and a bare `docker run --device=/dev/kvm
-e CHV_SETUP_SCRIPT=... kontur` works. A script that itself needs the
network is of course still a script that needs the guest to have one.

### Flow 2: customize the VM

Three routes, and which one you want depends on when the customization
has to happen. All three produce a guest with your packages in it; only
the first can run anything while it does so.

| Route | Where it runs | Needs KVM | Can start services |
|-------|---------------|:---------:|:------------------:|
| `konturctl guest build` | in a booted guest, committed to a new image | yes | yes |
| `docker build --build-arg GUEST_SETUP_SCRIPT=...` | in the guest rootfs at image build time | no | no |
| `CHV_SETUP_SCRIPT` | in the guest at every fresh boot | yes | yes |

**Docker, route 1: `konturctl guest build`.** This boots the image's own
guest, runs your script inside it over the same `kontur exec` path as
everything else, scrubs the per-boot identity the boot created
(`/etc/machine-id`, any SSH host keys, seeds), and `docker commit`s the
result. What comes out is an ordinary OCI image of the same shape as its
base -- `docker run` on it boots your customized VM -- so a customized
guest is something you derive and push with the tools you already have,
rather than a disk image to build and host separately (see
`internal/guestbuild`).

```sh
# Headroom for whatever the script installs, since the base image's disk
# is sized to its own rootfs plus ~20%. Without this, a perfectly good
# boot fails with "You don't have enough free space in
# /var/cache/apt/archives/".
docker build -t kontur:base --build-arg GUEST_DISK_EXTRA_MB=2048 .

cat > setup.sh <<'EOF'
set -eux
export DEBIAN_FRONTEND=noninteractive
apt-get update
# The reference guest's kernel has no IPv6 (see the note below), and
# nginx's postinst starts the service, whose stock site listens on
# [::]:80 -- so hold the start back until that line is gone, or the
# install fails and takes the whole "guest build" with it.
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
chmod +x /usr/sbin/policy-rc.d
apt-get install -y --no-install-recommends nginx
rm -f /usr/sbin/policy-rc.d
sed -i '/listen \[::\]:80/d' /etc/nginx/sites-available/default
systemctl enable nginx
EOF

konturctl guest build -from kontur:base -setup setup.sh -t kontur:nginx

konturctl vm create web -backend docker \
  -kontur-image kontur:nginx -state-dir ~/.kontur/vms
konturctl vm exec web -state-dir ~/.kontur/vms -- systemctl is-active nginx
```

Notes on the ones that bite:

- **Disk headroom is a property of the base image**, not of the build:
  `guest build` provisions the guest in `CHV_DISK_MODE=persistent`
  (writing through to the image's own `disk.img`, which is the point --
  that is what gets committed), and `CHV_DISK_SIZE_MB`/`-disk-size-mb`
  only ever grows an overlay. So the room has to be in the image
  already, via `GUEST_DISK_EXTRA_MB`. Size it against what you install:
  every GB here is a GB on disk on every machine that pulls the image.
- **The reference guest's kernel has no IPv6 at all.** It is
  cloud-hypervisor's own published kernel (see "Guest disk image and
  kernel"), built without `CONFIG_IPV6`: there is no `/proc/net/if_inet6`
  in the guest and any `bind()` to an AF_INET6 socket fails with
  "Address family not supported by protocol". Debian packages whose stock
  config listens on `[::]` -- nginx being exactly the example above --
  therefore fail to *start*, and since a Debian postinst starts the
  service it just installed, the `apt-get install` itself exits non-zero
  and fails the whole `guest build`. The `policy-rc.d` dance above is what
  keeps that install from starting anything before its config is fixed;
  any setup script installing a network service on this guest needs the
  same treatment, or a guest kernel of your own with IPv6 in it
  (`GUEST_KERNEL_PACKAGE=linux-image-amd64` builds one).
- **The guest's DNS is whatever its rootfs shipped with.** kontur
  configures no resolver: the `ip=` parameter `netshim` derives carries
  an address, gateway and netmask and no DNS servers, and nothing writes
  `/etc/resolv.conf` in the guest. On docker's default bridge, where
  outbound traffic is NATed and the rootfs's inherited resolver address
  is reachable, `apt-get update` just works (CI's guest-build job
  installs `build-essential` this way on every commit). On a user-defined
  network, where docker's resolver is `127.0.0.11` inside the
  *namespace's* loopback, it cannot work -- see "Container networking"'s
  known gaps. Write `/etc/resolv.conf` at the top of your setup script if
  you are not sure which you have.
- `-docker-run-arg` passes an option through to the `docker run` behind
  the build -- a proxy's environment, a private registry's credentials, a
  different network -- since the guest inherits none of the builder's own
  network context. `-keep-on-failure` leaves the VM up so you can
  `docker logs`/`kontur exec` into what went wrong.

**Docker, route 2: `GUEST_SETUP_SCRIPT` at image build time.** No KVM
needed, and the disk is sized *after* your packages are in it, so there
is no headroom to guess at. The cost is that this runs in a container
with no running kernel and no service manager: it installs packages and
writes files, and it cannot `systemctl start` anything or observe the
guest running.

```sh
docker build -t kontur:nginx --build-arg GUEST_SETUP_SCRIPT="$(cat setup.sh)" .
```

(`systemctl enable` does work here -- it only writes symlinks. It is
`start` that doesn't.)

**Route 3: `CHV_SETUP_SCRIPT` at boot time.** Per-VM rather than
per-image, so it needs no image build at all; see Flow 1's last section
and "Suspend and resume". Under `-backend docker` there is currently no
`konturctl` flag that passes it to the VM container, so this route is
really a pod-spec (or hand-written `docker run`) one.

**GKE.** Do the customizing off-cluster -- `konturctl guest build` needs
`/dev/kvm` and a docker daemon on the machine running it, which is a
build host or a nested-virt GCE VM, not a pod -- then push the result and
point the manifest at it:

```sh
docker tag kontur:nginx us-central1-docker.pkg.dev/<project>/<repo>/kontur:nginx
docker push us-central1-docker.pkg.dev/<project>/<repo>/kontur:nginx

sed "s|IMAGE_PLACEHOLDER|us-central1-docker.pkg.dev/<project>/<repo>/kontur:nginx|" \
  deploy/k8s/gke-pod-exec-example.yaml | kubectl apply -f -
kubectl exec kontur-gke-exec-example -c web -- kontur exec -- systemctl is-active nginx
```

Nothing about the pod changes but the image reference: the customized
image carries its own guest disk, so the manifest still names no
`CHV_DISK_IMAGE`. `CHV_SETUP_SCRIPT` (route 3) is the one route that does
work in-cluster, since it is just another environment variable on the VM
container -- run on a cluster, its output lands in `kubectl logs <pod> -c
web` followed by `kontur: setup script complete`, the VM stays up
afterwards, and a script that exits non-zero instead fails the container
(`kontur: setup script exited with status 3`, pod `Failed`).

Give that `is-active` line a moment, or a retry: `kontur exec` starts
working as soon as the guest's agent is up, which is a little *before*
systemd has finished starting the guest's own units, so run immediately
after the pod is applied it can report `inactive` for a service that is
seconds away from running. Measured on a cluster, on a node with no other
load: 47s from `kubectl apply` to the first `kontur exec -- true` that
succeeds, and 50s to `systemctl is-active nginx` returning `active`.

### Flow 3: exec a command on a running VM

**Docker.**

```sh
# A single command, output and exit status straight back to you.
konturctl vm exec web -state-dir ~/.kontur/vms -- systemctl is-active nginx

# An interactive login shell in the guest.
konturctl vm shell web -state-dir ~/.kontur/vms
```

`vm exec` reads the VM's backend out of `-state-dir` and does the rest
itself: name the container, `docker exec` into it, run `kontur exec`
inside that. Its stdin, stdout and stderr are the guest command's, and
its exit status is the guest command's own -- so it drops into a script
where any other command would go, and the pipelines in Flow 4 work
through it too. `-user` overrides the account for one command
(`KONTUR_EXEC_USER`), `-connect-timeout` the dial retry window
(`KONTUR_EXEC_CONNECT_TIMEOUT`), and `-it` asks for a terminal, which
`vm shell` does by default when `konturctl` has one on stdin.

The same thing spelled out, which is what those two run and what to use
against a VM `konturctl` didn't create:

```sh
# A single command, and an interactive login shell (note -it, and no
# "-- <command>").
docker exec kontur-vm-web kontur exec -- systemctl is-active nginx
docker exec -it kontur-vm-web kontur exec

# The same two, via the sh/bash shim -- for tooling that doesn't know
# kontur exists and just runs a shell. See "Shimming sh and bash".
docker exec -it kontur-vm-web bash
docker exec kontur-vm-web sh -c 'tail -n100 /var/log/nginx/access.log'
```

`konturctl vm exec`/`vm shell` are `-backend docker` only. A VM running
as a static pod under the standalone kubelet is reached with `crictl`
instead, which `konturctl` doesn't install and can't assume is there --
so it says so, and prints the two `crictl` lines to run, rather than
shelling out to something that probably isn't installed.

**GKE.** Identical, with `kubectl exec <pod> -c web` in place of `docker
exec <container>`:

```sh
kubectl exec kontur-gke-exec-example -c web -- kontur exec -- systemctl is-active nginx
kubectl exec -it kontur-gke-exec-example -c web -- kontur exec
kubectl exec -it kontur-gke-exec-example -c web -- bash
```

This needs nothing set up on the guest or the container: exec goes over
the VM's own vsock device, so a VM with `NETSHIM_CONTROL_CIDR=""`, or
with no `netshim` in front of it at all, is still one you can exec into.
See "Execing into a VM" for the knobs there are (`KONTUR_EXEC_USER`,
`KONTUR_EXEC_CONNECT_TIMEOUT`, `CHV_VSOCK_SOCKET`).

A session is byte-transparent: stdout and stderr come back on separate
streams, newlines are not rewritten, and stdin is passed through
unchanged. That was not always true -- a stock guest used to mirror every
SSH session onto the serial console by running it under a pty, which
merged the two streams and turned every newline into CRLF -- and it is
worth knowing because the workaround for it (`GUEST_CONSOLE_WRAP=0`) is
gone along with the wrapper. A pty is now used only when you ask for one,
by running `kontur exec` with a terminal on stdin.

The same path is how you resize a running VM, since `kontur resize` also
has to run inside the VM's container to reach its API socket (see "Memory
hotplug" and "CPU hotplug"). Both ceilings are fixed at boot, so the VM
has to have been created with headroom to grow into:

```sh
konturctl vm create web -backend docker \
  -kontur-image kontur:nginx -state-dir ~/.kontur/vms \
  -memory-max-mb 4096 -cpus-max 4

docker exec kontur-vm-web kontur resize -memory-mb=1024 -cpus=4
kubectl exec kontur-gke-exec-example -c web -- kontur resize -memory-mb=2048 -cpus=4
```

`konturctl vm exec` is no shortcut for that one: it runs its command in
the *guest*, and `kontur resize` has to run in the container alongside
cloud-hypervisor's API socket.

On GKE the equivalent of those two `vm create` flags is a pair of
environment variables on the VM container, `CHV_CPUS_MAX` and
`CHV_MEMORY_MAX_MB`, and both example manifests now set them (booting at
2 vCPUs / 1024MB with ceilings of 4 / 2048, and pod `limits` sized for
the ceilings rather than the boot sizes, since a hotplugged vCPU competes
for the container's CPU quota and hotplugged guest memory counts against
its memory limit). They did not until this was run on a cluster, and the
`resize` line above was the thing that found it.

Without those ceilings a VM has no headroom at all and neither half of
that `resize` does anything useful: growing the vCPU count is refused by
cloud-hypervisor ("Requested vCPUs exceed maximum"), and a `-memory-mb`
*below* the boot size is accepted, logs `requested resize to 1024 MiB`,
exits 0 and changes nothing, since virtio-mem can only take back memory
it hotplugged in the first place. With them, the same call moved a
running guest's `MemTotal` from 1000828 kB to 2049404 kB and gave it
`cpu2`/`cpu3` -- which the guest still has to online itself before
`nproc` counts them, as "CPU hotplug" describes. Size the ceilings for
the VM's busiest case and its starting size for its typical one -- that
split is the whole point of the two settings (see "Memory hotplug").

### Flow 4: copy files to and from a running VM

The obvious commands do not do what their names suggest: `docker cp` and
`kubectl cp` copy into the *container*, which is a `scratch` image with
the two kontur binaries in it and none of the guest's filesystem -- and
`kubectl cp` fails outright besides, since it shells out to a `tar` the
container doesn't have. `kontur cp` is the one that lands in the guest,
and like `kontur exec` it is `docker exec`/`kubectl exec`'s own command
(see "Copying files in and out").

One side of the copy is always `-`: this container's own stdin/stdout,
which is what your shell's `<`/`>` redirection is connected to.

**Docker.**

```sh
# A file in, and the same file back out.
docker exec -i kontur-vm-web kontur cp - /srv/app.tar < app.tar
docker exec kontur-vm-web kontur cp /srv/app.tar - > app.tar

# A directory, as a tar stream on each side.
tar -cf - -C ./site . | docker exec -i kontur-vm-web kontur cp -tar - /srv/www
docker exec kontur-vm-web kontur cp -tar /srv/www - | tar -xf - -C ./site-copy
```

For a VM `konturctl` created, `konturctl vm exec` folds the container's
name and `kontur`'s own invocation away, and passes stdin and stdout
through byte for byte -- so the `cat`/`tar` pipelines `kontur cp`
replaced still work through it, for the cases a copy doesn't cover:

```sh
konturctl vm exec web -state-dir ~/.kontur/vms -- sh -c 'cat > /root/app.tar' < app.tar
konturctl vm exec web -state-dir ~/.kontur/vms -- cat /etc/os-release > os-release
```

There is no `konturctl vm cp`, though, so the copy itself is still
`docker exec ... kontur cp` as above (see "Rough edges").

**GKE.** Identical, with `kubectl exec <pod> -c web` in place of `docker
exec <container>`:

```sh
kubectl exec -i kontur-gke-exec-example -c web -- kontur cp - /srv/app.tar < app.tar
kubectl exec kontur-gke-exec-example -c web -- kontur cp /srv/app.tar - > app.tar
```

`-i` is what makes a copy *in* work: without it neither `docker exec` nor
`kubectl exec` attaches your stdin to the command, and the copy stores an
empty file rather than failing. Copies *out* need no flag -- but do not
add `-t`, which puts a terminal in the middle of the very stream being
copied.

Binaries are safe in both directions, on any guest, without the caller
doing anything about it: `kontur cp` encodes the payload on the wire by
default and works out for itself what the guest at the other end needs
(see "Copying files in and out" for the probe, and `-encode` for
overriding it). That is most of what this mode is for.

**The hand-built pipelines this replaced came with a long warning, and
the warning is worth keeping as history.** Until exec moved off SSH, a
stock guest wrapped every session in a pty so its output could be
mirrored to the serial console, and a pty is not a byte-transparent
pipe. Measured then, on the stock reference guest, with a 100KB random
file:

| Pipeline | Stock guest (pty) | without the wrapper |
|----------|-------------------|---------------------|
| text in (`cat > file`) | works, byte for byte | works |
| binary in (`cat > file`) | **failed**: exited 131, 0 bytes written | works |
| directory in (`tar -xf -`) | **failed**: "tar: Refusing to read archive contents from terminal" | works |
| binary in, base64-encoded | **hung** partway through | n/a |
| text out | CRLF line endings, stderr merged in | clean, stderr separate |
| binary out (`cat`) | **failed**: truncated and corrupted | works, byte for byte |

None of the left-hand column applies any more: there is no wrapper, no
pty unless a caller asks for one by running `kontur exec` with a terminal
on stdin, and stdout and stderr are separate frames on the wire rather
than two things sharing a terminal (see `internal/execwire`). The table
stays because it is the measurement that justified the change, and
because a guest of your own that puts a pty back in the middle -- by
installing sshd with a `ForceCommand` of its own, say -- gets the
left-hand column back for anything hand-built. `kontur cp` is the
exception: it detects that guest and encodes around it, so the same two
commands above copy a binary correctly either way.

### Where these come from

The docker half of Flows 1-3 is what CI exercises on every commit under
real KVM (see "Development" below): its `guest build` job builds a guest,
provisions it with an `apt-get install`, commits it, boots the result via
`konturctl vm create -backend docker`, and checks it with both `docker
exec ... kontur exec` and `konturctl vm exec` -- exit status and stdin
passthrough included. The same job then runs Flow 1's short form for
real: a `konturctl vm run` whose output it captures, a second one whose
guest command exits 42, and a check that neither left a VM behind.

Flow 4's docker half is in that CI job too, as its own step: a megabyte
of random binary copied into the booted guest and checksummed there,
copied back out and compared byte for byte, and a directory round-tripped
through `-tar` both ways.

The Kubernetes half has been run twice: once on a real GKE Standard
cluster, which is where the GKE-specific findings in
[`deploy/k8s/gke.md`](deploy/k8s/gke.md)'s own "Validated" section come
from (the node pool flag, the two `privileged` requirements, image
pulls), and once since -- every flow above, end to end, `kubectl exec ...
-- kontur exec`, `kontur resize` and Flow 4's copies included -- on a
local single-node Kubernetes cluster standing in for GKE, which is where
the corrections in Flows 1-4 above come from. "Validated on GKE" below
says which claims rest on which, and what is still GKE-specific and
assumed.

Flow 4 used to be the exception -- `kontur exec`'s documented
stdin/stdout proxying used for something it was not built for, never run
end to end against a booted guest. It has been now: every pipeline in it,
in both directions, against both a console-wrapped guest and one without
the wrapper, on a nested-virt VM with real KVM (see "Validated end to end
under docker" below). That is where the table in Flow 4 comes from. Those
runs predate the move off SSH, so they measure a transport this repo no
longer has -- see that table's own note.

### Rough edges

Everything above works, and most of the length above is workaround. In
rough order of how much friction each one costs:

- **The reference guest's kernel has no IPv6**, so a Debian package that
  listens on `[::]` by default fails to start -- and fails its own
  `apt-get install` with it, which is how it usually surfaces (see Flow
  2). Every stock guest is affected; the `debian12` variant, which brings
  Debian's own kernel, is not.
- **`konturctl vm create` still needs `-state-dir` passed** by anyone who
  isn't root, since the default `/var/lib/kontur/vms` isn't a directory
  an unprivileged user can create. It is at least a fast, clear failure
  now -- the directory is checked before any container starts, rather
  than on the save afterwards. (The other two defaults that used to
  need overriding no longer do: `-disk-mode` is `overlay`, the only mode
  the reference guest finishes booting from, and `-kontur-image` follows
  `-backend` -- `kontur:latest` under docker, the node-local registry
  under static-pod.)
- **`vm exec`/`vm shell`/`vm run`/`vm wait`/`vm status` are `-backend
  docker` only.** A VM the
  standalone kubelet runs is reached through `crictl` against
  containerd's socket, which `konturctl` neither installs nor drives; it
  prints the `crictl` lines to run instead of doing it for you.
- **No `konturctl vm cp`.** `vm exec`/`vm shell` fold the container name
  away for running a command, but a copy is still `docker exec ... kontur
  cp` against `kontur-vm-<name>` -- the one part of Flow 4 that still
  needs to know how the VM is run.
- **Readiness stops at "the guest answers".** `kontur ready`, the
  probe behind `vm wait`, `vm status` and the pod condition (see
  "Readiness"), reports that `kontur-agent` ran a command -- which lands
  ahead of the guest's own services being up (3s ahead, in the cluster
  run behind Flow 2's note). "I can exec" and "the workload is running"
  are two different instants and only the first is observable from
  outside, so a guest that boots straight into a service still needs a
  poll of its own after the wait returns. Letting the guest declare its
  own readiness would fix that and is not implemented.
- **`konturctl` can't pass `CHV_*` through to the VM container**, so
  `CHV_SETUP_SCRIPT`, `CHV_SNAPSHOT_PATH` and friends are reachable only
  from a hand-written pod spec or `docker run`.
- **`CHV_MEM_AGENT` does nothing on either reference guest.** Their
  kernel has no `CONFIG_PSI`, so the guest-side agent has no
  `/proc/pressure/memory` to poll and says so once before exiting;
  growing a stock guest is `kontur resize` from the host, or a guest
  whose kernel has PSI (see "Memory hotplug").
- **Guest DNS is inherited, not configured.** Nothing gives the guest a
  resolver, so whether `apt-get update` works inside it depends on what
  the rootfs shipped and which docker network it landed on.
- **Guest disk headroom is decided at image build time** for anything
  `konturctl guest build` installs, because provisioning writes through
  to the image's disk and `-disk-size-mb` only grows overlays.
- **Nothing `konturctl`-shaped exists for Kubernetes.** On GKE every flow
  is a hand-written manifest plus `kubectl exec`; `konturctl vm` targets
  a standalone kubelet or a docker daemon, not an apiserver.

## How it works

The image contains three things:

- `kontur` (this repo, Go): a single binary with seven modes, selected by
  the first argument (`run`, the default if none is given; `netshim`;
  `exec`; `cp`; `resize`; `ready`; or `sleep`):
  - **`run`** reads configuration from the environment, execs
    `cloud-hypervisor` with the resulting arguments, streams the guest's
    serial console to the container's stdout/stderr so it shows up under
    `kubectl logs`, turns `SIGTERM`/`SIGINT` into a graceful VM shutdown,
    and, if configured, runs a one-time setup script and suspends the VM
    once it completes so a later run can resume it instead of booting
    fresh; see "Suspend and resume" below.
  - **`netshim`** splices the pod's single guest straight onto the
    namespace's own segment, so it takes over the address and MAC the
    container runtime assigned; see "Container networking" below. It's
    meant to run once, to completion, as an init container.
  - **`exec`** runs a command inside the VM guest itself, over the VM's
    own vsock device, rather than in this otherwise-empty
    container -- meant to be `kubectl exec`'s own command, so that ends
    up in the guest too; see "Execing into a VM" below.
  - **`cp`** copies a file or a directory between the caller's own
    stdin/stdout and the guest's filesystem, over that same session --
    the copy `docker cp`/`kubectl cp` can't do, since those reach the
    container rather than the guest; see "Copying files in and out"
    below.
  - **`resize`** live-resizes an already-running VM's memory and/or
    vCPU count via cloud-hypervisor's own API, within the ranges `run`
    configured at boot -- also meant to be `kubectl exec`'s own command
    (it needs to reach the API socket inside this container); see
    "Memory hotplug" and "CPU hotplug" below.
  - **`ready`** reports whether the guest is up yet, as an exit status
    and nothing else -- the readiness signal callers used to write a
    poll loop around. Being a mode of the same binary in the same
    container, it doubles as that container's own readiness probe, which
    is what makes `kubectl wait --for=condition=Ready` report on the
    guest rather than on the container; see "Readiness" below.
  - **`sleep`** just blocks until killed. It exists purely so
    `-backend docker` (see "Operating a node" below) can hold a network
    namespace open with the `kontur` image itself, without needing a
    coreutils `sleep` binary the image doesn't otherwise carry.

  `netshim` talks to the kernel directly via netlink (see "Container
  networking" below) rather than shelling out to `ip`/`tc`, so none of
  the seven modes needs anything beyond the two statically linked binaries
  themselves -- the image's base is `scratch` (see "Building" below).
- `cloud-hypervisor`: the actual VMM, fetched from the upstream static
  release build and pinned by checksum in the `Dockerfile`.
- A reference guest disk image and a matching guest kernel (see "Guest
  disk image and kernel" below): a minimal Debian system carrying
  `kontur-agent`,
  plus a kernel that can boot it via cloud-hypervisor's direct-kernel-boot
  path, both baked into the image so a VM container works out of the box
  without a separately-managed disk image or kernel.

A VM pod is one `netshim`-mode init container plus one `run`-mode
container, both using the *same* `kontur` image -- just invoked with
different `args` (`["netshim"]` vs `["run"]`). One VM per pod is the
model: a network namespace has exactly one identity for a guest to take
over, so a second VM belongs in a second pod. See
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

`CHV_SETUP_SCRIPT`, if set, is run once inside the guest over its vsock
device (the same machinery `kontur exec` uses, see "Execing into a VM"
below) right
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

**A snapshot carries the guest's disk too**, not just its memory.
cloud-hypervisor's own snapshot is memory and device state, and in the
default `CHV_DISK_MODE=overlay` the disk behind it is a qcow2 inside the
VM container's writable layer -- which the *next* container, the one
resuming, doesn't have. So `kontur run` copies that overlay into the
snapshot directory (while the VM is paused, so it is not being written)
and puts it back before restoring, and a resumed guest lands on the disk
its memory was taken against. The other two disk modes have nothing to
carry: `persistent` writes to `CHV_DISK_IMAGE` itself and `readonly`
writes nowhere, so in both the disk on the next run is whatever that
image is then -- which has to be the same image, unchanged since the
snapshot, or the guest resumes onto a filesystem that has moved under it.
This costs a snapshot the size of whatever the guest has written so far,
on top of its memory. A snapshot written before kontur did this has no
overlay in it; resuming from one still works and says so in the log,
since what the guest gets is whatever disk the container it lands in
already had.

## Configuration

`kontur run`'s configuration is entirely via environment variables:

| Variable              | Required | Default                          | Description |
|------------------------|:--------:|-----------------------------------|--------------|
| `CHV_DISK_IMAGE`       | no       | the guest image baked into this image (`/var/lib/kontur/guest/disk.img`) | Path to the primary disk image. |
| `CHV_DISK_MODE`        | no       | `overlay`                         | How the primary disk is attached: `overlay` (the guest writes into a thin qcow2 of its own, created in this container, leaving the image untouched), `persistent` (the guest writes through to the image itself) or `readonly`. |
| `CHV_DISK_OVERLAY_PATH`| no       | `/var/lib/kontur/overlay/disk.qcow2` | Where `overlay` mode puts that qcow2. Inside the container's own writable layer, so a restart keeps it and only removing the container discards it. |
| `CHV_DISK_SIZE_MB`     | no       | the disk image's own size         | How large a disk the guest gets, in MiB: the overlay is created at (or grown to) this size before the VM starts. Growing only, and `CHV_DISK_MODE=overlay` only. See "Disk size" below. |
| `CHV_DISK_READONLY`    | no       | —                                 | Deprecated, replaced by `CHV_DISK_MODE`: `true` means `readonly`, `false` means `persistent`. Setting both is an error. |
| `CHV_EXTRA_DISKS`      | no       | —                                 | Comma-separated additional disks: `path[:ro\|rw]`. |
| `CHV_KERNEL`           | no       | the kernel baked into this image (`/var/lib/kontur/guest/vmlinux`), unless `CHV_FIRMWARE` is set | Path to a kernel for direct boot (PVH/`vmlinux`). Mutually exclusive with `CHV_FIRMWARE`. |
| `CHV_INITRAMFS`        | no       | —                                 | Path to an initramfs, used with `CHV_KERNEL`. |
| `CHV_CMDLINE`          | no       | `console=ttyS0 root=/dev/vda rw` | Kernel command line, used with `CHV_KERNEL`. When `NETSHIM_VM` is set, `kontur run` appends the `ip=` and `kontur.routes=` parameters it derives from the namespace -- unless this already carries an `ip=` of its own, which suppresses both (see "Container networking" below). |
| `CHV_FIRMWARE`         | no       | —                                 | Path to firmware (e.g. `CLOUDHV.fd`) for firmware boot, instead of `CHV_KERNEL`'s default. Mutually exclusive with `CHV_KERNEL`. |
| `CHV_CPUS`             | no       | `2`                               | Boot vCPU count. |
| `CHV_CPUS_MAX`         | no       | `CHV_CPUS`                        | Ceiling `CHV_CPUS` can grow to via hotplug. See "CPU hotplug" below. |
| `CHV_MEMORY_MB`        | no       | `256`                             | Guest memory at boot, in MiB. See "Memory hotplug" below. |
| `CHV_MEMORY_MAX_MB`    | no       | `2048`, or `CHV_MEMORY_MB` if larger | Ceiling `CHV_MEMORY_MB` can grow to via hotplug. See "Memory hotplug" below. |
| `CHV_MEMORY_HOTPLUG`   | no       | `true`                            | Attach a memory hotplug device sized for growth up to `CHV_MEMORY_MAX_MB`. See "Memory hotplug" below. |
| `CHV_MEMORY_SHARED`    | no       | `true`                            | Shared guest memory (required for some device backends, and for `CHV_MEMORY_HOTPLUG`). |
| `CHV_NET`              | no       | —                                 | Passed through verbatim as `--net`, e.g. `tap=eth0,mac=...`. Ignored when `NETSHIM_VM` is set, which is when `kontur run` derives the guest's NICs from the namespace itself (see "Container networking" below). |
| `CHV_API_SOCKET`       | no       | `/run/cloud-hypervisor/api.sock` | Path to the cloud-hypervisor API socket. |
| `CHV_BINARY_PATH`      | no       | `/usr/local/bin/cloud-hypervisor` | Path to the cloud-hypervisor binary. |
| `CHV_EXTRA_ARGS`       | no       | —                                 | Extra CLI args appended verbatim, space-separated (e.g. `--watchdog`). |
| `CHV_SHUTDOWN_TIMEOUT` | no       | `20s` (Go duration syntax)        | How long to wait for a graceful guest shutdown before forcing it. |
| `CHV_SETUP_SCRIPT`     | no       | —                                 | Shell script run once inside the guest after a fresh boot. If `CHV_SNAPSHOT_PATH` is also set, suspended to it on success; otherwise reruns on every fresh boot. See "Suspend and resume". |
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

## Disk size

Left alone, a guest's disk is exactly as big as the disk image it boots
-- which, for the reference guest baked into this image, is the rootfs
plus a little headroom (see the Dockerfile's `guest-image` stage) and
nothing more. `CHV_DISK_SIZE_MB` is how a VM gets a bigger one:

```
docker run --rm --privileged --device /dev/kvm \
  -e CHV_DISK_SIZE_MB=8192 kontur
```

`kontur run` applies it before cloud-hypervisor is started, to the qcow2
overlay the guest writes into (`CHV_DISK_MODE=overlay`, the default; see
"How it works" above): a fresh overlay is created at that size, and an
overlay that is already there -- a restarted container, or a volume
mounted at `CHV_DISK_OVERLAY_PATH` so it outlives one -- is grown to it
in place, so raising the value on such a VM enlarges the disk it comes
back with instead of being ignored. Growing a qcow2's virtual size
allocates no clusters and rewrites no data (it is a header field and an
L1 table sized to cover it), so this costs nothing at boot however large
the number is, and everything the guest had already written is still
there afterwards.

Three things it deliberately will not do:

- **Shrink.** A smaller value than the disk already has is an error, not
  a truncation: the clusters past the new end would become unreachable,
  and a guest filesystem spanning them would be corrupt.
- **Go below the disk image.** The overlay reads through to the image for
  everything the guest hasn't written yet, so a size below the image's
  own would cut the guest's root filesystem off partway.
- **Touch the image itself.** `CHV_DISK_SIZE_MB` requires
  `CHV_DISK_MODE=overlay` and is an error in the other two modes: the
  overlay is the only disk `kontur` creates, and `persistent`/`readonly`
  both put the guest on a disk image that is shared with every other VM
  booting it.

What it changes is the size of the block device the guest is offered, and
only that. Growing the partition table and filesystem inside it is the
guest's own job -- a guest that never does it simply sees a larger disk
with the same amount of free space in its filesystem.

The reference guest baked into this image does it for itself, so
`CHV_DISK_SIZE_MB` alone is enough there: it puts ext4 directly on
`/dev/vda` with no partition table, and `kontur-growfs` runs `resize2fs`
on it in early boot. That is an online resize of the
mounted root, so it needs no initramfs hook and no reboot, and it is a
no-op on every boot where the filesystem already fills the disk -- as
well as when the root is read-only (`CHV_DISK_MODE=readonly`). Both guest
variants have it; see `deploy/guest-image/README.md`'s "Growing the
filesystem". A guest of your own (`CHV_DISK_IMAGE`) still has to arrange
its own growth, with `growpart` first if it has a partition table.

`konturctl vm create -disk-size-mb` (see "Operating a node" below) is the
same setting for a VM managed by `konturctl`; `konturctl vm update
-disk-size-mb` raises it, and the VM's container resizes its overlay on
the boot that update triggers.

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
(`10.00` default), opens a plain TCP connection to `KONTUR_MEM_AGENT_HOST`
on `KONTUR_MEM_AGENT_PORT` (`30090` default, matching
`CHV_MEM_AGENT_ADDR`'s own default port) and writes a single `PRESSURE
<value>` line. That host is netshim's control link address, which the
reference guest image's `kontur-control-net` writes out at boot -- the
agent's own fallback, its default route's gateway, is wrong here, since
the guest's default route leads out to the container network rather than
to this VM's own `kontur run` container. See "Container networking"
below.

That guest-side half needs a kernel with `CONFIG_PSI`, and **neither
reference guest has one**: the bundled kernel is cloud-hypervisor's own
CI kernel (see "Guest disk image and kernel"), which is built without it,
so `/proc/pressure/memory` does not exist there and `psi=1` on
`CHV_CMDLINE` does not bring it back -- checked on a booted guest. The
host side still comes up and listens; the daemon in the guest says once
that this kernel cannot report pressure and exits, and `CHV_MEM_AGENT`
is a no-op on that guest. `kontur resize` from the host is unaffected and
is how a stock guest grows. A guest kernel with PSI compiled in is what
makes the automatic half work: Debian's own kernel, which the `debian12`
variant brings, has it behind that same `psi=1` boot parameter.

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
`CHV_MEM_AGENT_ADDR`'s *port* away from `30090` needs a matching
guest-side override of `KONTUR_MEM_AGENT_PORT` (e.g. via
`CHV_SETUP_SCRIPT` dropping in an env override and restarting the
service) to keep working -- there is currently no plumbing to pass that
through automatically. Moving `NETSHIM_CONTROL_CIDR` needs no such
override: `kontur-control-net` reads the address off the control link
itself.

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
the guest instead, over the VM's own virtio-vsock device, where the
guest's `kontur-agent` answers (see "Guest disk image" below) -- the same
way `run` already makes the guest's console show up under `kubectl
logs`:

```sh
kubectl exec -it <pod> -c <container> -- kontur exec -- <command> [args...]
```

See "Flow 3" under "User flows" above for the docker and GKE versions of
this end to end, and "Copying files in and out" below for `kontur cp`,
which copies a file or a directory over this same session.

For a VM `konturctl` created on a docker daemon, `konturctl vm exec
<name> -- <command>` (and `konturctl vm shell <name>`) is the same thing
with the container name and this binary's own invocation folded away; see
"Operating a node" below.

Leaving off `-- <command>` (i.e. just `kontur exec`) opens an interactive
login shell instead.

Two flags describe the command rather than the connection, spelled the
way `docker exec` spells them:

```sh
docker exec kontur-vm-web kontur exec -w /src -e GOFLAGS=-mod=vendor -- go build ./...
```

- `-w`/`--workdir <dir>`: an absolute guest directory to run in, instead
  of the account's home directory. The guest refuses a directory that
  isn't there, naming it, rather than letting the failure arrive as the
  shell's.
- `-e`/`--env KEY=value`, repeatable: added to the environment the
  session would have had, not a replacement for it -- `PATH`, `HOME`,
  `USER`, `LOGNAME`, `SHELL` and `TERM` are all still set (see
  `internal/agent` for why that environment is sshd's), and a key given
  here overrides the one it would have had. Unlike `docker exec`, a bare
  `-e NAME` is refused rather than read out of this container's own
  environment: nothing sets that environment up, so the variable would
  quietly not arrive.

Both used to have to be written into the command line itself -- `cd /src
&& GOFLAGS=-mod=vendor go build ./...`, quoted correctly by the caller --
and the fixed `PATH` is why a guest image tends to install its toolchain
into `/usr/local/bin`, which is on it. This is the alternative to that.

The two ends of this protocol ship from one commit, and a guest image
older than the `kontur` binary talking to it now says so instead of
ignoring what it doesn't understand: the agent reports which of these
optional fields it implements, and a `-w` that reached an agent without
them fails loudly rather than running in the home directory (see
`internal/execwire`'s package comment).
Explicitly running `kontur exec` this way handles any command; see
"Shimming sh and bash" below for a way to reach the same guest session
via plain `sh`/`bash`, without needing to know `kontur exec` exists at
all, for the common case of an interactive shell or a single `-c
<command>`.

| Variable                      | Required | Default                          | Description |
|--------------------------------|:--------:|-----------------------------------|--------------|
| `KONTUR_EXEC_USER`             | no       | `root`                            | The guest account to run the command as. `konturctl vm create -guest-user` sets this. The agent runs as root in the guest and drops to this account per request, so there is nothing to authorize on the guest side and nothing that has to agree with it. |
| `KONTUR_EXEC_CONNECT_TIMEOUT`  | no       | `30s` (Go duration syntax)        | How long to keep retrying the initial connection -- `kontur-agent` starts when the guest does, so how long that takes is guest boot time. |
| `CHV_VSOCK_SOCKET`             | no       | `/run/kontur/vsock.sock`          | The host end of the guest's vsock device, read by both `kontur run` (which tells cloud-hypervisor to create it) and `kontur exec` (which dials it). One setting for both halves, since they are two modes of the same binary in the same container. |

Nothing here depends on guest networking, and that is the point of it.
Exec used to be SSH over netshim's control link, which made every part of
bringing that link up load-bearing for being able to get in at all -- a
guest whose network had not come up was one kontur could not ask why. A
vsock connection is carried by the virtio device itself, so `kontur exec`
works on a guest with a broken network, no address, or no NIC at all.

## Readiness

A VM container is "up" the moment `cloud-hypervisor` is exec'd, which is
a long way before anything can be run inside the guest: `konturctl vm
create` returns there, and so does `kubectl wait
--for=condition=Ready` on a pod with no probe on it. `kontur ready` is
the signal for the difference:

```sh
docker exec kontur-vm-web kontur ready              # up to 30s, exit 0 when the guest answers
docker exec kontur-vm-web kontur ready -timeout 0   # ask once, answer now
kubectl exec <pod> -c <container> -- kontur ready -timeout 5m
```

It dials the guest over the same vsock device `kontur exec` uses, runs a
trivial command there, and exits `0` or non-zero -- printing nothing on
success, because the exit status is the whole answer. `-timeout` bounds
the retry and defaults to `KONTUR_EXEC_CONNECT_TIMEOUT` (30s); `-timeout
0` makes it a single attempt. Both of the ordinary "still booting"
failures are retried rather than reported: the vsock socket not existing
yet (the VMM has not started) and nothing listening on the port yet (the
guest has not started `kontur-agent`).

Being a mode of the same binary in the same image, it needs nothing added
to a container to work as a readiness probe, which is what makes the pod
condition mean something:

```yaml
      readinessProbe:
        exec:
          command: ["/usr/local/bin/kontur", "ready", "-timeout", "0"]
        periodSeconds: 5
        timeoutSeconds: 5
```

Every manifest here already carries that -- the one `konturctl vm create
-backend static-pod` renders, and the examples under
[`deploy/k8s/`](deploy/k8s) -- so `kubectl wait --for=condition=Ready`
waits for the guest rather than for the container wrapping it. `-timeout
0` because the kubelet is already retrying on `periodSeconds`, and the
absolute path because a probe is resolved against whatever `PATH` the
runtime defaults to for an image that sets none.

On a docker daemon it is `konturctl vm wait`/`vm status` that read this
signal (see "Operating a node" below), and `konturctl vm create -wait`
that folds the wait into the create.

**What "ready" means here, and what it does not.** Only that
`kontur-agent` answered and ran a command. That is the honest limit of
what is observable from outside the guest, and it is a smaller claim than
most callers eventually want: a guest becomes reachable a few seconds
*before* whatever it boots into is actually serving (3s ahead, in the
cluster run behind Flow 2's note), so "I can exec" and "the workload is
up" are two different instants and only the first one is visible here.
Letting a guest declare its own readiness -- a file its setup script
touches, a unit that has to be active -- is the thing that would close
that gap, and it is deliberately not what this does: it would need a
convention every guest image follows, and a guest that didn't follow it
would never be ready. A caller that needs the stronger statement still
polls for it itself, on top of this rather than instead of it.

## Copying files in and out

`kontur cp` (see `internal/guestcp`) copies a file or a directory between
the guest's filesystem and the caller's own stdin/stdout, over exactly
the session `kontur exec` above uses. It is meant to be invoked the same
way, and for the same reason: `docker cp`/`kubectl cp` reach the
container, which holds two binaries and none of the guest's filesystem
(and `kubectl cp` fails outright besides, since it shells out to a `tar`
the `scratch` image doesn't carry).

```sh
kubectl exec -i <pod> -c <container> -- kontur cp [-tar] [-encode auto|base64|raw] <src> <dst>
```

Exactly one of `<src>`/`<dst>` is `-`, which is this container's own
stdin/stdout -- the end `docker exec -i`/`kubectl exec -i` connects to
your shell's redirection. The other is a path inside the guest. There is
no third form: the container's own filesystem has nothing worth copying,
and a guest-to-guest copy is `kontur exec -- cp`.

| Flag | Default | Description |
|------|---------|-------------|
| `-tar` | off | The stream is a tar archive and the guest path is a directory: unpacked into it on the way in (created if it doesn't exist), packed from its contents on the way out. This is how a directory fits down one stream, and both ends are the caller's own `tar`, so a directory copied out and back in round-trips. |
| `-encode` | `auto` | How the payload is carried over the session: `base64`, `raw`, or `auto` to ask the guest. See below. |

Nothing is added to the guest for this: it is one exec session running
one shell pipeline (`base64 -d > file`, `tar -cf - -C dir .`, and so on),
so it works on any guest `kontur exec` already works on, needs no new
credential, and shares `kontur exec`'s own settings --
`KONTUR_EXEC_USER`, `KONTUR_EXEC_CONNECT_TIMEOUT` and
`CHV_VSOCK_SOCKET`, in the table above.

**Why the payload is encoded by default.** So that a copy doesn't depend
on which guest you have. The transport is byte-transparent (see "Execing
into a VM"), but the guest gets a say too: one that puts a pty back in
the middle of every session -- an sshd with a `ForceCommand` of its own,
a console wrapper like the reference guest image used to ship -- turns
every LF into CRLF, folds stderr into stdout, and corrupts binary in both
directions. That is what the old hand-built recipes had to work around
with a `base64 | tr -d '\r' | base64 -d` dance the caller had to know to
type. `-encode auto` probes the guest once instead: it encodes if the
guest has a `base64` binary (both reference guests do), falls back to a
raw stream if it doesn't and nothing is rewriting bytes, and refuses --
rather than writing a corrupt copy -- if the guest is wrapped *and* has
no `base64`. That probe is also what notices a wrapped session on the way
*in*, where a terminal reports end-of-input only when it is sent an EOT,
so `-encode base64`/`-encode raw` (which skip the probe, and the round
trip it costs) are for guests you already know about.

A copy streams, in both directions, with no staging file at either end:
a failure partway through can leave a partial file behind, and the error
says the copy did not complete rather than implying it did. A local
source that fails mid-read is reported the same way, instead of arriving
as a shorter file that exited zero.

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
from its six real modes by `argv[0]` (see `cmd/kontur`) and forwards
into the guest the same way `kontur exec` does.

This can't support arbitrary `sh`/`bash` invocations the way a real
shell binary would -- there's no way to plumb an arbitrary local argv
through a protocol that carries a single command line (see
`internal/guestexec`'s `ShellCommandLine`).
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
`netshim` mode sets up the tap, the splice and the control link via the
netlink Go library rather than exec'ing `ip`/`tc`, so nothing here needs a
real userland to run in -- there's no smaller-but-still-working option to
trade off against, unlike the guest disk image below. The cost is
`ip`/`tc`-style debuggability: there's no shell or package manager
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
(`GUEST_SUITE`/`GUEST_ALPINE_VERSION`, `GUEST_SETUP_SCRIPT`,
`GUEST_KERNEL_PACKAGE`, `GUEST_DISK_EXTRA_MB`).

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

"Flow 2" under "User flows" above compares this against the other two
ways to customize a guest (`konturctl guest build`, which boots the guest
and provisions it for real, and `CHV_SETUP_SCRIPT`, which does it per-VM
at boot) and walks each one through end to end.

## Running locally

```sh
docker run --rm --device=/dev/kvm kontur
```

This boots a VM with no network device, so its console is the only way to
watch it boot -- but not the only way to reach it: give the container a
name and `docker exec <name> kontur exec -- <command>` runs a command in
that guest over its vsock device, network or no network (checked on the
image this tree builds). "Flow 1" under "User flows" above is the same
thing with a guest that also has a network of its own.

`CHV_DISK_IMAGE` and `CHV_KERNEL` are both left unset here, so this boots
the reference guest disk image and kernel baked into `kontur` itself --
see "Guest disk image and kernel" below for what that gets you (mainly:
a guest `kontur exec` can reach, with no networking needed for it, whose
console shows up right here in the container's own output). Point
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
carrying `kontur-agent`, meant as a reference/demo guest usable without
managing a separate disk image -- not a production guest; bring your own
`CHV_DISK_IMAGE` for that.

It runs no SSH server. `kontur exec` reaches it over the VM's own
virtio-vsock device, where `kontur-agent` answers, so there is no
account to log into, no key to authorize and nothing on the guest's
network that has to be working first -- see "Execing into a VM" above.
See [`deploy/guest-image/README.md`](deploy/guest-image/README.md) for
what is in the image, how to add sshd to a guest of your own if you want
interactive access, and why the per-boot keypair, the host-key
regeneration and the console-mirroring session wrapper that used to be
here are all gone with the transport they served.

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
worked example: one VM per pod, whose guest takes over the pod's own IP
(see "Container networking" below), so the pod is addressed by Services
and everything else exactly as any other single-container pod is. Three
things any deployment needs to account for:

- **KVM access**: the node needs `/dev/kvm` (nested virtualization enabled,
  if the cluster itself runs on VMs) and the container needs access to it —
  either through a device plugin such as
  [`kubevirt/kubernetes-device-plugins`](https://github.com/kubevirt/kubernetes-device-plugins)'s
  `kvm` plugin (preferred: no elevated pod privileges needed), or by running
  the container `privileged: true` with `/dev/kvm` bind-mounted.
- **Graceful termination**: set `terminationGracePeriodSeconds` comfortably
  above `CHV_SHUTDOWN_TIMEOUT` so Kubernetes doesn't `SIGKILL` the container
  before `kontur run` finishes its own shutdown sequence.
- **The `netshim` init container**: it needs `privileged: true` in a pod,
  since a pod has no way to grant `/dev/net/tun` on its own. Leave it out
  altogether for a VM that needs no network at all, as
  [`gke-pod-example.yaml`](deploy/k8s/gke-pod-example.yaml) does.

See [`deploy/k8s/gke.md`](deploy/k8s/gke.md) for a GKE-specific,
tested walkthrough (enabling nested virtualization on the node pool,
pushing the image, and a self-contained
[`gke-pod-example.yaml`](deploy/k8s/gke-pod-example.yaml) that needs no
other cluster setup), and "User flows" above for what to do with the pod
once it is running --
[`gke-pod-exec-example.yaml`](deploy/k8s/gke-pod-exec-example.yaml) is
the same self-contained example plus the `netshim` init container those
flows need to reach the guest.

## Local testing with a static kubelet

See [`deploy/static-kubelet/`](deploy/static-kubelet/README.md) for
scripts that install a standalone kubelet (containerd + kubelet running
static pods from a manifest directory, no apiserver) on a single node,
plus a local image registry so `kontur` can be built and run there
straight from a working tree.

## Container networking (`netshim` mode)

A pod runs exactly one VM, and that VM *is* the pod's endpoint: `netshim`
mode splices the guest directly onto whatever segment the container
runtime already put the pod's network namespace on, where it takes over
the address *and* MAC the runtime assigned. From outside there is still
exactly one endpoint, so the VM behaves like an ordinary container --
Services, `-p`, `--network` membership, compose -- because all of those
are properties of the sandbox rather than of anything `netshim` installs.

There used to be a second, NAT-based mode here: a private bridge, one tap
per VM, and nftables DNAT rules sharing one pod IP between several VM
containers. It is gone. Kubernetes' own unit of address is the pod, and a
pod that runs one VM taking over that address needs no forwarding rules,
no `net.ipv4.ip_forward`, no per-VM external port and nothing on the data
path at all.

What `netshim` builds, run once to completion as an init container:

- One tap, `tap-<name>`, with its MTU copied from the external interface.
- A **splice**: on each of the external interface and the tap, an ingress
  qdisc plus a match-everything filter whose action is `mirred egress
  redirect` at the other one. Redirect steals the frame rather than
  cloning it, so nothing is copied and no frame leaves the kernel.
- Optionally a control link -- a bridge holding `NETSHIM_CONTROL_CIDR`
  plus a `ctl-<name>` tap -- see below.

What it deliberately does *not* build: no `net.ipv4.ip_forward` write, no
nftables table, no routing and no NAT. `netshim` is control plane only (it
programs kernel state and exits; nothing of it is ever in the data path),
and leaves the kernel one tc action per packet rather than a conntrack
lookup, a DNAT rewrite, a routing decision and a bridge FDB lookup.

Because init containers share the pod's network namespace with the
containers that follow them, everything `netshim` creates is already in
place by the time the VM container starts. `netshim` is idempotent: if
Kubernetes retries a failed init container, a second run converges on the
same bridge/taps/filters rather than erroring on things that already
exist.

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
container shares the namespace, so it reads the address, MAC, MTU and the
interface's own routing table back off the external interface itself and
synthesizes its own `--net` (with `mac=` and `mtu=`) and kernel `ip=`
and `kontur.routes=` parameters at boot -- which is what `NETSHIM_VM` on
the VM container selects, and why `CHV_NET` is ignored there. That behaves identically
whether the sandbox came from `docker run`, a kubelet or anything else,
and leaves no second copy of the identity to drift out of date. It works
because `netshim` leaves that interface addressed: the splice steals its
ingress, so the namespace's own stack can never receive a reply over it
anyway. An explicit `ip=` in `CHV_CMDLINE` still wins, for an operator
overriding the derived identity on purpose.

The default route is the piece with no second source: the `ip=`
parameter is the only thing that ever gives the guest one, so a guest
that doesn't get it here has no route off its own segment at all --
while its address, MAC and MTU are all correct and `kontur exec` reaches
it perfectly well over the control link.

**The CNI's routes.** An address, a netmask and a gateway are all `ip=`
can say, and they are not the whole of what a container's interface
carries. A guest configured from them alone believes its whole subnet is
on-link and ARPs for peers in it directly. On a bridge CNI -- docker's
own network, and
[`deploy/static-kubelet/cni/`](deploy/static-kubelet/cni/) -- that is
exactly right, because the subnet really is one L2 segment. On a
point-to-point one -- `kind`'s default `ptp`, and GKE's -- it is wrong
in a way that looks healthy: `ptp` gives the pod a `/24` address *plus*
an explicit `10.244.0.0/24 via <gateway>` route, precisely so that
nothing is treated as on-link, and the host end of the veth answers no
ARP for anyone else (`proxy_arp` is 0). A guest that believes its
netmask there reaches its gateway, the node and ClusterIPs perfectly
well, and cannot exchange packets with any other pod in the subnet in
either direction -- outbound "No route to host", inbound the request
arrives and the reply ARPs into the void. That was measured on a `ptp`
cluster before it was fixed; see "Validated on GKE".

So the guest is given the routes the runtime actually installed, rather
than a table inferred from a netmask. `kontur run` reads the external
interface's IPv4 routing table alongside the rest of the identity and
appends it to the kernel command line as a parameter of kontur's own,
next to the `ip=` it derives:

```
ip=10.244.0.5::10.244.0.1:255.255.255.0::eth0:off kontur.routes=10.244.0.1/32,10.244.0.0/24@10.244.0.1,0.0.0.0/0@10.244.0.1
```

Comma-separated `<destination>[@<gateway>]` entries, on-link ones first
-- a gatewayed route can only be installed once something makes its
gateway reachable, which on a `ptp` CNI is the host route in the same
list. The guest installs them with `ip route replace` at boot
(`kontur-configure-routes`, see [`deploy/guest-image/`](deploy/guest-image/README.md)),
and drops anything left on the interface that the runtime does not have.
The bridge case is unchanged by all of this in the only sense that
matters: there the CNI's routes *are* the routes `ip=` installs, so each
replaces itself and the resulting table is the same one as before.

A kernel parameter rather than more of `ip=` because the kernel has no
route parameter of any kind -- `ip=` is the whole of its network
configuration syntax -- and because the guest is the only place the
table can be applied. An explicit `ip=` in `CHV_CMDLINE` suppresses
`kontur.routes=` as well: the two describe one interface between them,
and a guest addressed by hand is not the identity these routes were read
off. A guest image with nothing reading the parameter simply ignores it,
which is the pre-fix behaviour rather than a worse one.

**The control link.** Because the guest answers to the namespace's own
address, anything *inside* the namespace dialing that address reaches
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
unchanged with the control link disabled or with no `netshim` in front of
it at all. A third-party guest image needs its own equivalent; without
one, the VM still works, but nothing inside the namespace can reach the
guest.

Setting `NETSHIM_CONTROL_CIDR=""` omits the control link entirely, which
costs the memory agent and nothing else. It used to cost `kontur exec`
too, which is most of why exec no longer goes over it.

The memory agent needs the same treatment for a different reason: it
signals whichever host address it is given, defaulting to its own default
route's gateway -- which here leads out to the container network rather
than to the `kontur run` container beside it. `kontur-control-net`
therefore also writes `KONTUR_MEM_AGENT_HOST` (see `cmd/kontur-mem-agent`)
pointing at the control link, which both guests' service definitions pick
up.

| Variable                 | Required | Default        | Description |
|---------------------------|:--------:|-----------------|--------------|
| `NETSHIM_VM`              | yes      | —               | The VM's name, which both the init container and the VM container derive `tap-<name>`/`ctl-<name>` from. Setting it on the VM container is also what tells `kontur run` to derive its own `--net` and `ip=` from the namespace. |
| `NETSHIM_CONTROL_CIDR`    | no       | `169.254.100.1/24` | The control link's address and subnet. Empty disables the control link. |
| `NETSHIM_BRIDGE`          | no       | `kontur0`       | Name of the control link's bridge. |
| `NETSHIM_EXTERNAL_IFACE`  | no       | `eth0`          | The interface whose identity the guest takes over. |
| `NETSHIM_DNS`             | no       | `8.8.8.8`       | Nameserver(s) the guest resolves through, comma separated, at most two: they travel on the guest's `ip=` boot parameter (its `dns0`/`dns1` fields) and the guest's `kontur-configure-dns` writes them into `/etc/resolv.conf`. Explicitly empty leaves whatever the guest image ships with. A public resolver is the default because neither the host's own resolver nor docker's embedded one is reachable from inside a guest -- see `deploy/guest-image/README.md`'s "The resolver". `konturctl vm create -dns` sets it. |

Under `-backend docker`, `netshim` needs no `--privileged`: with no
`/proc/sys/net` write to make it runs with `--cap-add NET_ADMIN --cap-add
NET_RAW --device /dev/net/tun`. The device grant is not optional -- the
netlink library creates a tap by opening `/dev/net/tun` rather than over
rtnetlink, and docker's device cgroup denies it otherwise. A pod has no
per-device grant to give, so the static pod manifest stays privileged.

Known gaps:

- **DNS.** Docker's embedded resolver listens on `127.0.0.11`, the
  *namespace's loopback*, which is not on the wire -- so other containers
  resolve the VM by name, but the guest cannot resolve them.
- **IPv4 only.**
- **Single queue.** The tap is created without `IFF_MULTI_QUEUE`, so
  `num_queues` cannot be raised without also handing cloud-hypervisor
  file descriptors instead of a tap name.
- **No re-addressing.** Setup is one-shot, so if the runtime ever
  re-addresses the container the guest's `ip=` goes stale and nothing
  notices.
- **The guest image has to install the carried routes** (see "The CNI's
  routes" below). The reference image does; a third-party one that
  ignores `kontur.routes=` gets the pre-fix behaviour on a
  point-to-point CNI, which is a guest that reaches its gateway and no
  other pod at all.


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
- `konturctl vm create <name> -disk ... [-backend static-pod|docker] [flags]`:
  starts one VM (a `netshim`-mode init container plus a single `run`-mode
  container, the same shape as
  [`manifests/kontur-static-pod.yaml`](deploy/static-kubelet/manifests/kontur-static-pod.yaml))
  under the chosen backend. Under `-backend static-pod` this writes a
  manifest to the kubelet's static pod directory, and the standalone
  kubelet notices the new file and starts the pod within a few seconds --
  no `kubectl apply`, since there's no apiserver to apply to. Under
  `-backend docker` it runs `docker run` directly instead; see "Docker
  backend" below. It returns as soon as the VM is started, which is
  before its guest has booted; `-wait` (with `-ready-timeout`, 3 minutes
  by default) keeps it from returning until the guest answers, and is
  `-backend docker` only for the reason the next few entries are.
- `konturctl vm wait <name> [-timeout]`: blocks until an existing VM's
  guest answers a command -- `vm create -wait`'s second half, for a VM
  created without it, restarted, or created by something else. It polls
  `kontur ready` in the VM's container (see "Readiness" above), so it
  waits for exactly what a pod's readiness probe reports. It gives up
  early if the VM's container has exited, since no amount of further
  waiting fixes that, and its error names the container to read logs
  from.
- `konturctl vm status <name> [-timeout]`: the same question, asked
  without waiting for the answer to be yes. It prints the VM's container
  (and whether it is running), whether the guest answered, and -- when it
  didn't -- the probe's own reason, which distinguishes "the VMM has not
  created its vsock socket yet" from "nothing is listening on it inside
  the guest yet". It exits `0` when the guest is ready and `1` when it
  isn't, printing nothing extra either way, so it sits in a script's own
  condition. A VM that can't be found, or a docker that can't be reached,
  is an error instead: the question couldn't be asked at all, which is a
  different thing from the answer being no.
- `konturctl vm exec <name> [flags] -- <command>`: runs one command
  inside that VM's guest. It reads the VM's backend and settings from
  `-state-dir`, derives its container name, and runs `kontur exec` in it
  -- so a caller needs the VM's name and nothing about how it is run.
  stdin, stdout and stderr are the guest command's, and `konturctl`'s
  exit status is the guest command's own, which is what lets it sit in a
  script where any other command would. `-it` asks for a terminal,
  `-user` overrides `KONTUR_EXEC_USER` for this one command, and
  `-connect-timeout` overrides `KONTUR_EXEC_CONNECT_TIMEOUT` (the retry
  window on a guest that may still be booting). `-w`/`-workdir` and
  `-e`/`-env` belong to the guest command rather than to the session, and
  are passed through to `kontur exec`'s own flags of the same name (see
  "Execing into a VM").
- `konturctl vm shell <name> [flags]`: the interactive case of the above
  -- no command, and a terminal by default when `konturctl` has one on
  stdin, so `konturctl vm shell web < script.sh` still works.
- `konturctl vm run <name> [flags] -- <command>`: create a VM, wait for
  its guest, run one command in it, exit with that command's status, and
  delete the VM. It takes every flag `vm create` does, plus
  `-ready-timeout` (how long to wait for the guest, 3 minutes by default)
  and `-keep-on-failure` (leave a VM whose guest never came up in place,
  so its console can be read). It takes `vm exec`'s flags too, `-w` and
  `-e` included, since the one command it exists to run wants them as
  much as any other. Progress goes to stderr and the guest
  command's output to stdout, so the command's output can be captured on
  its own. The VM is deleted however the command ends, a non-zero status
  included.
- Those five (`vm wait`, `vm status`, `vm exec`, `vm shell`, `vm run`),
  and `vm create -wait` with them, are `-backend docker` only, and say so:
  reaching into a VM the standalone kubelet runs means `crictl` against
  containerd's socket, which `konturctl` doesn't install and can't
  assume is present, so the error prints the `crictl` commands that do
  the same thing by hand.
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
- `konturctl guest build -from <image> -setup <script> -t <image>`: boots
  a kontur image's own guest, runs `<script>` as root inside it, scrubs
  the per-boot identity that boot created (machine-id, host keys, seeds)
  and commits the result as a new image of the same shape -- so
  `docker run` on the output boots the customized VM exactly as its base
  does. It is the counterpart to the Dockerfile's `GUEST_SETUP_SCRIPT`
  build arg for anything that has to observe the guest actually running
  (starting a service, warming a cache, running a test suite); see
  `internal/guestbuild` for what each costs, and "Flow 2" under "User
  flows" above for a worked example. Runs against a docker daemon on a
  machine with `/dev/kvm`, and is the one `konturctl` subcommand that has
  nothing to do with a node's own VMs -- it neither reads nor writes
  `-state-dir`.

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

`-disk-size-mb` sizes that overlay, for a VM that needs a bigger disk
than the image it boots. `konturctl` passes it through as
`CHV_DISK_SIZE_MB` and the VM's own container applies it before boot (see
"Disk size" above), so `konturctl vm create -disk-size-mb 8192` gives the
guest an 8GiB disk from the start. It is `-disk-mode=overlay` only and
never smaller than `-disk`'s own image. The bundled reference guest grows
its own filesystem onto the extra space at boot, so `-disk-size-mb` is
free space there and not just a bigger block device; a guest of your own
still has to do that growing itself.

`-cpus-max` and `-memory-max-mb` are the two hotplug ceilings, passed
through as `CHV_CPUS_MAX`/`CHV_MEMORY_MAX_MB` (see "CPU hotplug" and
"Memory hotplug"). Both are fixed at boot -- cloud-hypervisor sizes each
hotplug window once and cannot grow it later -- so a VM created without
them cannot be resized upward at all, whatever `kontur resize` is asked
for afterwards. Left at `0` they are omitted entirely and the VM
container's own defaults apply, which is what a spec saved before these
flags existed gets. `konturctl vm update` can raise them, at the cost of
the VM being recreated (and the overlay with it) like every other update.

`konturctl vm update -disk-size-mb 16384` raises it for an existing VM,
with the same caveat every other flag on `vm update` carries: the update
recreates the VM's container, and the overlay lives in that container's
writable layer, so the guest comes back from its image at the new size
rather than keeping what it had written. Growing an overlay *in place*,
writes intact, is what `kontur run` does for an overlay that outlived the
change (see "Disk size" above) -- not what an `update` does.

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
  -disk /images/disk.img -kernel /images/vmlinux
```

The VM takes over the namespace's own identity, so there is no address or
port to give: the address comes from docker, and ports are published on
the sandbox itself the ordinary way:

```sh
konturctl vm create web -backend docker \
  -disk /images/disk.img -kernel /images/vmlinux \
  -docker-run-opt --network -docker-run-opt mynet \
  -docker-run-opt -p -docker-run-opt 8080:80
```

`-ip`, `-port`, `-guest-port` and `-bridge-cidr` are still accepted and
now ignored: they configured the NAT mode that has been removed, and a
deployment's existing `vm create` line should not fail over flags that no
longer have anything to configure. `-net` is the exception -- `-net flat`
is accepted (it is what every VM is now), and `-net nat` is refused
rather than silently reinterpreted as something reachable elsewhere.

`-docker-run-opt` is repeatable and passes each value through verbatim to
the `docker run` that creates the namespace holder. It has to go there
rather than on the VM container: port publishing, network membership and
DNS all belong to the container that *creates* a network namespace, and a
container joining an existing one with `--network container:` cannot add
them afterwards. Passing the flag at all replaces whatever a previous
`vm create`/`vm update` saved, so it behaves like every other flag.

A Kubernetes pod's containers share a network namespace held open by the
pod sandbox for as long as the pod exists, which is what lets the
`netshim`-mode init container set up the tap and the splice that the VM
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

`netshim`'s container runs unprivileged under this backend, with
`--cap-add NET_ADMIN --cap-add NET_RAW --device /dev/net/tun` (see
"Container networking" above). It used to need `--privileged`, for one
specific reason: the NAT mode wrote `net.ipv4.ip_forward`, and a
container runtime's default runc-style OCI spec mounts `/proc/sys/net`
read-only regardless of capabilities granted -- true of a real standalone
kubelet/containerd CRI pod just as much as plain `docker run`. With that
mode gone there is no sysctl to write, and what remains needs only the
capabilities and the device. The static pod manifest stays privileged
anyway, since a pod has no per-device grant to hand `netshim`
`/dev/net/tun` with. The VM container runs `--privileged` under both
backends regardless, for `/dev/kvm`.

The two paragraphs below record passes made while the NAT mode still
existed; it has since been removed (see "Container networking" above),
and the flat-mode half of what they describe is what `netshim` does now.

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

This found one real bug, now fixed: the guest-side control link
(`kontur-control-net`, see "Container networking" above) never came up under this
guest image. `kontur exec` timed out dialing the control address, and
`kontur-mem-agent` never received its target either. The guest's second
NIC boots as `eth1`, which is what both `kontur-control-net`'s default
`KONTUR_CONTROL_IFACE` and (for the first NIC) the derived `ip=`
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

## Design notes

Everything above describes what kontur does today.
[`docs/`](docs/) holds proposals about what it could do instead, which
are decision documents rather than reference material -- read them for
the argument, not for how the thing currently works.

- [`docs/agent-substrate-uvm-runtime.md`](docs/agent-substrate-uvm-runtime.md):
  a candidate design for making kontur the micro-VM runtime behind
  [agent substrate](https://github.com/agent-substrate/substrate),
  checked against both codebases. It argues against the integration --
  substrate's micro-VM slot is already filled by a cloud-hypervisor
  runtime that does more -- and for contributing kontur's two genuinely
  distinctive pieces upstream instead. Along the way it names five things
  kontur is missing that have nothing to do with substrate: readiness,
  resource stats, a VM lifecycle independent of the process lifecycle,
  restore into a fresh host, and snapshot fan-out.

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
further and exercise the real kernel through the same netlink Go library
`netshim` itself uses (no `ip`/`tc` binaries needed): identity discovery,
the tap and control link, and the splice itself -- including a genuine
packet test that injects a frame on one end of a veth pair and reads it
back off the tap, and the reverse.

Those need `CAP_NET_ADMIN` and `/dev/net/tun`, and skip without them. That
default is necessary (they cannot run unprivileged) and it is a trap: a
package whose every kernel-touching test skipped still reports `ok`, and
skips are invisible without `-v`, so a green `go test ./...` says nothing
about whether the splice carries a packet. Setting
`KONTUR_NETNS_TESTS=required` turns each such skip into a failure naming
what was missing, which is how CI asserts they really ran rather than
passing quietly on a runner that could not exercise the kernel.

They do not run in *your* network namespace. Given root, the test binary
re-executes itself with `CLONE_NEWNET` before the first test runs
(`internal/netshim/netns_test.go`), so every veth, tap, bridge and `tc`
filter it creates lives in a namespace nobody else is in: nothing on the
machine -- `systemd-networkd`, `udev`, NetworkManager, docker -- sees a
new link appear and reacts to it, the interfaces can have fixed readable
names instead of names made unique from a pid, and the kernel destroys
the lot when the process exits, even if a test dies before its cleanup
runs. The namespace is taken by re-executing rather than by calling
`unshare(2)` in-process because a network namespace belongs to a
*thread*: the Go runtime moves goroutines across threads, so unsharing
would leave some tests in the new namespace and some in the old. A test
that finds itself in a shared namespace anyway (`CLONE_NEWNET` refused)
skips, or fails under `KONTUR_NETNS_TESTS=required`, rather than falling
back to creating fixed-name interfaces on the host.

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

> **These three sections predate `kontur exec` moving off SSH.** They are
> kept as the record of what was actually run and when, so where they
> describe an sshd in the guest, a per-boot keypair, host-key
> regeneration or a console-mirroring session wrapper, they are
> describing the transport this repo had at the time rather than the one
> it has now. What replaced it is covered under "Execing into a VM"; the
> `guest build` job in CI exercises the current path on every commit
> under real KVM.


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

### Validated end to end under docker

Every flow under "User flows" above, run in order on a GCE
`n2-standard-8` with `--enable-nested-virtualization` (real `/dev/kvm`,
`vmx` present) and Docker CE 29.8.0, against images built on that same
machine from this tree. What passed as written: Flow 1 whole (`vm create
-backend docker`, `docker exec ... kontur exec -- uname -a` returning in
about a second, a guest exit status of 42 arriving intact, `vm list`,
`vm delete`, and `docker run --rm --device=/dev/kvm kontur` booting a
stock guest to a login prompt and shutting down cleanly on SIGTERM);
Flow 2 route 2 (`--build-arg GUEST_SETUP_SCRIPT`, whose nginx came up
`active` on the next boot) and route 1's mechanics (`konturctl guest
build` provisioning a booted guest and committing it in about 80
seconds); Flow 3 entirely except the resize line (single command,
interactive login shell, and both `sh`/`bash` shims). It found four
things the docs had wrong, all fixed above:

- **Flow 2's own worked example failed**, and would have for anyone who
  ran it: the reference guest's kernel has no IPv6, nginx's stock site
  listens on `[::]:80`, and its postinst's `systemctl start` therefore
  fails the `apt-get install` outright ("Address family not supported by
  protocol"), which fails the whole `konturctl guest build`. Flow 2's
  script now holds the start back with `policy-rc.d` and drops the
  `[::]` line before enabling the service; with that, `guest build`
  commits an image whose booted guest reports `active`.
- **Flow 3's resize line could not work on a `konturctl`-created VM.**
  `konturctl` had no way to set `CHV_CPUS_MAX`/`CHV_MEMORY_MAX_MB`, so
  every VM it created booted with no hotplug headroom: `kontur resize
  -cpus=4` came back "Requested vCPUs exceed maximum" and
  `-memory-mb=1024` was accepted and did nothing (below the boot size,
  virtio-mem has nothing to take back). Fixed with `-cpus-max` and
  `-memory-max-mb` on `vm create`/`vm update`, plumbed into both
  backends; Flow 3 now shows the create that gives a VM room to grow.
  Confirmed on the same VM afterwards: created with `-cpus-max 4
  -memory-max-mb 4096`, a single `kontur resize -memory-mb=3072 -cpus=4`
  took the running guest from 1983MB to 3007MB of usable memory and from
  two `/sys/devices/system/cpu/cpuN` entries to four (still needing the
  guest to online the new ones, as "CPU hotplug" describes).
- **Flow 4 was wrong in both directions**, which is what "not run end to
  end against a booted guest" was hiding. Copying *in* is not
  unaffected by the console wrapper: the guest-side command's stdin is a
  pty, so a binary `cat > file` dies at the first control byte (exit
  131, nothing written), `tar -xf -` refuses to read an archive from a
  terminal, and a base64-encoded stream deadlocks against the pty's echo
  partway through. On a `GUEST_CONSOLE_WRAP=0` guest all four pipelines
  round-trip a 100KB random file byte for byte, in both directions, with
  stderr kept separate. Flow 4 now says so, with the measurements.
- **`CHV_SETUP_SCRIPT` under a bare `docker run` cannot work**, though
  Flow 1 offered it as a hand-written-`docker run` option: the script is
  delivered over the same SSH path `kontur exec` uses, so with no
  `netshim` in front of the VM it boots the guest, fails
  `KONTUR_EXEC_ADDR is required`, and shuts down again. (No longer true,
  and the clearest example of what the note at the top of these three
  sections is warning about: the script rides the same vsock session
  `kontur exec` does now, and a bare `docker run -e CHV_SETUP_SCRIPT=...`
  runs it and logs "setup script complete" -- re-checked on a booted
  guest in the pass below.)

Two smaller things seen and left alone at the time: `konturctl vm create
-h` (with no VM name) treated `-h` as the name and failed with "a disk
image is required" rather than printing usage, and `konturctl guest
build` inherits the host's DNS through docker's default bridge, which is
what makes `apt-get` work inside the guest -- the latter as documented in
Flow 2's notes. The first has since been fixed: a flag where the VM name
belongs is refused naming the argument (it used to be taken as the name,
so `vm create -state-dir /tmp/vms` made a VM called `-state-dir` in the
default state directory), and `-h` prints that subcommand's own flags and
exits 0 on every `konturctl` command. The GKE half of the flows was not
re-run in this pass; it remains covered by the section below.

### Validated beyond the flows: suspend/resume, disks, hotplug, cp and exec

The passes above walked the user flows. This one went after the parts of
the runtime *no* flow reaches -- suspend/resume, extra disks, overlay
growth, hotplug and the mem agent -- and the error paths around them, on
a GCE `n2-standard-8` with `--enable-nested-virtualization` (real
`/dev/kvm`, `vmx` present) and Docker CE 29.8.0, against an image built
on that machine from this tree. It found three bugs, all fixed here, and
one thing the runtime can't do at all on the guests it ships:

- **A suspended VM lost its disk.** `CHV_SNAPSHOT_PATH` promised a
  resumed guest "already reflects whatever the script did"; it didn't.
  cloud-hypervisor's snapshot is memory and device state only, and the
  disk it was taken against is (in the default `CHV_DISK_MODE=overlay`)
  a qcow2 inside the VM container's own writable layer -- which the next
  run, a new container, does not have. It made an empty one from the
  base image and resumed onto that: a 64MB file written by
  `CHV_SETUP_SCRIPT` and `sync`ed before the snapshot came back "No such
  file or directory" in the resumed guest, whose memory still believed
  in it, and which stayed mounted read-write over the rolled-back
  filesystem. `Runner.Suspend` now copies the overlay into the snapshot
  while the VM is paused and `kontur run` puts it back before booting, so
  a snapshot directory is self-contained; the same 64MB file now comes
  back `md5sum -c` clean in a fresh container. A snapshot written before
  this carries no overlay, which is not an error but does get a line
  saying what the guest is resuming onto.
- **A second `kontur run` bricked the VM already in the container.**
  Both sockets a boot has to clear -- cloud-hypervisor's API socket and
  the guest's vsock socket -- were removed before the run found out
  cloud-hypervisor would refuse to start anyway, unlinking them out from
  under the running VM. It kept running with no way in: `kontur exec`,
  `kontur cp`, `kontur resize` and the graceful-shutdown path all failed
  with ENOENT for the rest of that container's life, so the VM could only
  be killed. `docker exec <vm-container> kontur` was enough to do it, since
  `run` is the default mode. Each socket is now probed with a connect and
  a live one is refused rather than removed, before anything else in the
  boot writes anything.
- **`konturctl vm delete` claimed to remove a manifest it never had.**
  Run a second time on a docker-backend VM, with the saved state already
  gone, it printed "removed /etc/kubernetes/manifests/kontur-vm-<name>.yaml"
  -- a static-pod path for a VM that never was one, and a file `os.Remove`
  had just answered ENOENT for. It now says what happened.
- **`CHV_MEM_AGENT` cannot work on either reference guest**, and this is
  a guest kernel limitation rather than something to fix here. The
  bundled kernel (cloud-hypervisor's own CI kernel, see "Guest disk image
  and kernel") is built without `CONFIG_PSI`, so `/proc/pressure/memory`
  does not exist and `psi=1` on `CHV_CMDLINE` doesn't conjure it --
  verified on a booted guest. The host side comes up and listens; the
  guest-side `kontur-mem-agent` has nothing to read. It used to log the
  same ENOENT every 10 seconds for the life of the VM; it now says once
  that the kernel has no PSI and exits (`Restart=on-failure`, so it stays
  exited), leaving `kontur resize` from the host as the way this guest
  grows. Automatic growth needs a guest kernel with PSI compiled in --
  Debian's own kernel, which the `debian12` variant brings, has it behind
  the same `psi=1` boot parameter, untested here.

What was exercised and behaved as documented: `CHV_SETUP_SCRIPT` under a
bare `docker run` (which the SSH-era note below says cannot work -- it
can now, over vsock, and its "setup script complete" is in the log);
snapshot and resume across separate containers; `CHV_EXTRA_DISKS` in both
`rw` and `ro` (a second writer is refused by cloud-hypervisor's own image
locking, naming the file); `CHV_DISK_SIZE_MB` growing an overlay in place
across a restart, with the guest's writes intact (950MB to 2870MB of
usable root filesystem, `apt-get update` failing for want of space at the
image's own size and succeeding at 4GB); an overlay surviving `docker
restart`; memory hotplug up *and* back down (`kontur resize -memory-mb`
between 256 and 1024, virtio-mem returning it); `kontur cp` round-tripping
a 5MB binary file and a directory tree with a symlink in it, in both
directions, and reporting a missing guest path or an unwritable
destination as a failed copy rather than a silent empty one; eight
concurrent `kontur exec` sessions on one VM, a 10MB stdout and a 5MB
binary stdin arriving byte-for-byte, and guest exit statuses (0, 3, 42,
127) arriving intact; `konturctl vm run`'s whole create/wait/run/delete
cycle in 4.7s, its stdout/stderr kept separate from its own progress
output, and its refusal to reuse a name something else already has;
`vm create -wait`/`vm wait` failing in under a second, naming the
container to read logs from, on a VM whose container had exited; `vm
update` recreating a VM at its new size; and netshim giving the guest the
container's own address (`172.17.0.4`), reachable from the host, with
outbound TCP and inherited DNS working from inside the guest.

Smaller things seen and left alone: `kontur resize -memory-mb=N` for the
N the guest already has is rejected by cloud-hypervisor itself ("new size
and requested_size are identical"), so a script that resizes to a fixed
size twice fails the second time; and `kontur exec -h` runs `-h` in the
guest (`exec` has no flags of its own, and everything after it is the
command), unlike `kontur cp -h`/`kontur resize -h`, which answer with
their usage.

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

**The flows were re-run since, on a stand-in cluster rather than on
GKE.** That pass predates exec moving off SSH onto vsock, and the "User
flows" section's GKE column had never been walked end to end at all, so
all four flows were run again in full against the two `gke-*` manifests
exactly as checked in. Not on GKE, though: the machine that run happened
on had no GCP credentials, so no cluster of the shape
[`deploy/k8s/gke.md`](deploy/k8s/gke.md) describes could be created for
it. What it ran on instead was a single-node Kubernetes v1.34.0 cluster
created with `kind` v0.30.0 on a nested-virt host with real `/dev/kvm`
(Intel Xeon, `vmx`, Docker CE), with the host's `/dev/kvm` mounted into
the node container so the manifests' own `hostPath` finds it, and the
image built from this tree and side-loaded onto the node.

That distinction is worth keeping in view. A real apiserver, kubelet,
containerd and CNI ran the manifests as written, so everything in the
flows that is about *kontur* was exercised for real. Nothing that is
about *GKE* was: `--enable-nested-virtualization`, COS's node image,
GKE's own device cgroup, and an Artifact Registry pull are still covered
only by the earlier pass above and by `gke.md`.

What that re-run found, all fixed above and in the manifests:

- **Neither example manifest had any hotplug headroom**, so Flow 3's
  `kubectl exec ... -- kontur resize` line could not work as written --
  the same finding the docker pass made about `konturctl`-created VMs,
  in its pod-spec form. Both manifests now set `CHV_CPUS_MAX` and
  `CHV_MEMORY_MAX_MB` and boot below them; see Flow 3 for the before and
  after measurements.
- **Three manifests still set `KONTUR_EXEC_ADDR`**, which no longer
  exists in the code, and explained exec as SSH over the `netshim`
  control link. It is vsock now, which the cluster confirmed the useful
  way: `kubectl exec kontur-gke-example -- kontur exec -- uname -a`
  returns from a guest whose only interface is `lo`, on the manifest with
  no `netshim` in it at all. The docs said there was "no way to run a
  command in it"; there is.
- **The guest takes over the pod's IP, and on a `ptp` CNI that is not
  enough to talk to other pods.** `netshim` gave the guest the pod's
  address, MAC and gateway, and from inside it the node, the gateway and
  a ClusterIP (`10.96.0.1:443`) were all reachable -- but another pod
  could not fetch the guest's nginx over the pod IP (the node itself
  could, with a `200`), and the guest could not open a connection to
  another pod's IP either ("No route to host"). The cause is not
  Kubernetes-specific: `ip=` carries a netmask, the `ptp` plugin's whole
  design is that the subnet is *not* on-link, and the route that
  reconciles the two was the one part of the identity that did not
  survive the splice. Since fixed, by carrying the interface's routing
  table into the guest rather than inferring one from the netmask -- see
  "Container networking"'s "The CNI's routes". The fix has been
  exercised against the kernel (a `ptp`-shaped pod pair, in which the
  guest's side goes from unable to open a connection to another pod, in
  either direction, to able) and in CI's own bridge case, but not yet
  re-measured on a cluster; and whether this bit on GKE specifically was
  never established either way, since GKE's own node CNI is `ptp`-based
  but was not measured. Both are worth a pass.
- **`kontur exec` becomes available before the guest's services do** --
  47s from `kubectl apply` to the first successful exec, 50s to nginx
  reporting `active` -- which is enough to make Flow 2's own
  `systemctl is-active nginx` answer `inactive` if you run it the moment
  exec starts working. Noted in Flow 2 and under "Rough edges".

What passed as written, on that cluster:

- Flow 1: `gke-pod-example.yaml` booted to a login prompt in `kubectl
  logs`; on the exec manifest, `kubectl exec ... -c web -- kontur exec --
  uname -a` returned about 8s after `kubectl apply` (`kontur exec`'s own
  retry window covering the boot), and a guest exit status of 42 came
  back through `kubectl exec` intact.
- Flow 1's third route: `CHV_SETUP_SCRIPT` in a pod spec ran in the guest
  at boot, logged to `kubectl logs` and left the VM running, and a script
  exiting 3 failed the container instead (pod `Failed`) -- the one route
  the docs said works in-cluster, now booted rather than assumed.
- Flow 2's GKE half: `konturctl guest build` off-cluster (route 1, the
  README's own nginx script), the result side-loaded and named in the
  manifest with nothing else changed, `systemctl is-active nginx`
  answering `active` in the pod.
- Flow 3 entirely: single command, interactive login shell over a pty
  (`kubectl exec -it ... -- kontur exec`), both `sh`/`bash` shims, and --
  after the headroom fix -- `kontur resize -memory-mb=2048 -cpus=4`
  moving a running guest from 1000828 kB to 2049404 kB of `MemTotal` and
  from two `cpuN` entries to four.
- Flow 4, in the shape it had at the time -- hand-built pipelines through
  `kubectl exec -i ... -- kontur exec`, which is what that flow now keeps
  only as the cases a copy doesn't cover -- and with none of the caveats
  its table records for the SSH/pty era: a 100KB random binary in through
  `sh -c 'cat > file'` and back out again, both byte for byte by md5; a
  directory in through `tar -xf -`; text out with LF endings and stderr
  arriving separately from stdout. `kontur cp` landed on `main` after
  that run and has not been exercised through `kubectl exec` on a
  cluster; its docker half is in CI.
- Pod deletion: `kubectl delete pod --wait` returned in about 6s, well
  inside `terminationGracePeriodSeconds: 40`.

### Flat mode

The splice at the heart of `netshim` mode (see "Container networking"
above; it was one of two modes when this pass was made, and is the only
one now) was
validated against the real kernel rather than only through the netlink
calls being accepted. `TestSplice_MovesFramesBothWays` builds a veth pair
standing in for a container's own interface, splices it to a tap, opens
the tap the way cloud-hypervisor would, and confirms a frame injected on
the network side arrives on the tap and a frame written to the tap
arrives on the network side -- both directions, on real devices.
`TestSetup` covers the rest of the setup the same way: identity
discovery off an addressed interface, the tap's MTU copied from it,
exactly one ingress filter per device after two runs (the same
convergence a retried init container needs), and the control link's
bridge addressed with its tap enslaved to it.

`TestDiscoverIdentity_Gateway` covers the one part of the identity those
left out, against the real kernel for the same reason: the default route
is only what the kernel hands back, and the bug it now guards against was
exactly the difference between a route read off a dump and one built in a
test. `DiscoverIdentity` looked for a route with a nil `Dst`, which no
dump ever contains -- the kernel omits `RTA_DST` at prefix length zero
and `vishvananda/netlink` synthesizes `0.0.0.0/0` back in, the way
iproute2 does -- so every flat-mode guest booted with an empty gateway
field in `ip=` and no egress off its own segment, while every other part
of the identity it took over was correct. Found in a downstream
deployment; the test fails on the old condition and passes on the new
one.

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

### One VM per pod

Removing the NAT mode (see "Container networking" above) was verified
against the real kernel rather than only through the unit tests that no
longer mention it. `internal/netshim`'s whole suite was re-run as root
with `KONTUR_NETNS_TESTS=required`, so nothing skipped quietly: identity
discovery off an addressed interface, the tap's MTU copied from it, the
control link's bridge with its tap enslaved, exactly one ingress filter
per device after two runs, and `TestSplice_MovesFramesBothWays` injecting
a real frame on each side of a veth/tap pair and reading it back off the
other -- all passing on the single remaining code path.

A guest was not booted in that same sandbox, which has no room to build
the multi-gigabyte guest image -- but CI's "guest build under real KVM"
job boots one on every commit, and passed on this change: it builds the
`debian12` guest, provisions and commits it, then runs it through
`konturctl vm create -backend docker`, so `netshim` really did splice
that guest onto the container's interface and `kontur exec` really did
reach it over the control link, on a runner with `/dev/kvm`. That is the
same path a `-net flat` VM already took before this change, now the only
one.

`konturctl`'s own flags are the piece that could have broken a caller
silently, so `-ip`/`-port`/`-guest-port`/`-bridge-cidr` are accepted and
ignored and `-net nat` is refused outright, both asserted in
`internal/cli`'s tests.
