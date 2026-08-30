# The kontur guest image

`v2/pkg/kontur`'s own package doc comment, and `v2/README.md`'s
`grain mcpserver -kontur-vm` section, both state the same assumption: a
kontur-managed VM's guest image already carries the operator's SSH key and
a running sshd (and, per `v2/README.md`, git) before this repo ever tries
to reach it. Nothing in this repo built that image -- v1 has an equivalent
job for its own libvirt-managed sandboxes (`provision/sandbox.sh`, run as
cloud-init user-data against a shared base image on every VM's first boot);
v2 had no successor, kontur-backed or otherwise. This directory is that
successor: a plain shell pipeline (`build.sh`/`provision.sh`) that
produces a kernel, an initramfs and a disk image cloud-hypervisor
direct-kernel-boots, with no bootloader, firmware, or partition table
anywhere in the picture.

bwsalmon/agents#267 first answered "decide, don't just wire" for what the
image needs beyond sshd+key, how it gets built and published, and whether
one image serves every dispatch slot -- see the sections below for that.
bwsalmon/agents#478 (this directory's current shape) revisited *how* the
image and its kernel get built, because #267's own Packer/QEMU pipeline
had never actually been run for real, and the one kernel anyone had tried
booting it with (bwsalmon/kontur's own `benchmarks/kernel/build.sh`,
Firecracker's own firecracker-ci build) panics with "Cannot open root
device vda" against any cloud-hypervisor guest, direct-kernel-booted or
not: Firecracker uses virtio-mmio, cloud-hypervisor uses virtio-pci, and
that kernel only has a driver for the former. Building a purpose-built
PVH/virtio-pci kernel from source looked, going in, like the only way
past that. It turned out not to be needed at all -- see "Why no custom
kernel" below.

## Why no custom kernel

Debian's own `linux-image-amd64` package -- the same kernel package any
`apt-get install` on a stock Debian box would pull in, nothing special
selected or configured -- already has everything a cloud-hypervisor
direct-kernel-boot guest needs, confirmed by hand, under real KVM, against
a real cloud-hypervisor binary:

- **`CONFIG_PVH=y`** is already set in Debian's shipped kernel config
  (`/boot/config-<version>`) -- the entry point cloud-hypervisor's direct
  kernel boot uses, with nothing to enable or rebuild.
- **`virtio-pci`, `virtio_blk` and `virtio_net`** are all present as
  modules and bind automatically to cloud-hypervisor's virtio-pci
  transport -- this is precisely the driver the firecracker-ci kernel
  above lacks, and the reason that kernel panics on this VMM specifically.

So the "kernel" this image ships is just `/boot/vmlinuz-*` and
`/boot/initrd.img-*` out of a normal `apt-get install linux-image-amd64`,
copied out of the built rootfs as `vmlinuz`/`initrd.img` by `build.sh`.
There is no from-source kernel build anywhere in this pipeline, and no
firmware/GRUB/UEFI boot path either (cloud-hypervisor's other option,
`CHV_FIRMWARE` -- see below for why that path was ruled out, not just left
unexplored).

Two gaps still had to be closed by hand, both confirmed by booting the
actual built image under real KVM before this pipeline was considered
done:

- **The guest's one NIC needs to be named `eth0`.** `konturctl` itself
  (`internal/staticpod/spec.go`) derives each VM's static-addressing
  kernel command line as `ip=<ip>::<gateway>:<netmask>::eth0:off`, with
  `eth0` hard-coded -- but systemd's predictable-naming policy renames a
  virtio-net device to something like `ens2` by default, and the rename
  happens inside the *initramfs* (its own bundled, and by default stale,
  copy of `/etc/udev`), not just the final root. `provision.sh` pins the
  name with an explicit `.link` file (`00-eth0.link`) and re-runs
  `update-initramfs -u` after adding it, so the initramfs's own udev
  snapshot has it too -- skipping that second step was confirmed by hand
  to leave the guest unreachable even with the `.link` file in place.
- **Static addressing itself needs a small userspace helper.** Debian's
  kernel does not set `CONFIG_IP_PNP`, so nothing in the kernel itself
  acts on `ip=` the way a kernel built with in-kernel IP-config would.
  klibc's `ipconfig(8)` (pulled in transitively by `initramfs-tools`, via
  `klibc-utils`) implements the same static-addressing syntax, but --
  unlike the in-kernel code -- does not read `/proc/cmdline` itself; it
  only takes the spec as an explicit argument. `provision.sh` installs a
  small wrapper (`kontur-configure-net`) plus a `systemd` oneshot unit
  that extracts `ip=`'s value from `/proc/cmdline` and hands it to
  `ipconfig` directly. Also needed, and easy to miss: `virtio_net` has to
  be `modprobe`d explicitly (nothing auto-loads it early enough on its
  own) and the unit has to run after `systemd-udev-trigger`, or `ipconfig`
  finds no configurable device at all.

### Why `CHV_FIRMWARE` (a real alternative) was ruled out, not just skipped

cloud-hypervisor also supports booting through firmware (an EDK2/OVMF-like
build, `CLOUDHV.fd`) instead of a direct kernel -- which would let a
guest run its own bootloader/kernel of its choosing, sidestepping the
"does this specific kernel support direct boot" question entirely.
It was not used here because it does not compose with kontur's own
per-slot static addressing at all, not because of any missing
kernel/firmware feature: `internal/hypervisor/args.go`'s `BuildArgs` only
passes `--cmdline` (and so the `ip=` konturctl derived) when a *kernel* is
given; the `else` branch (`--firmware` alone) never passes a command line
to the guest at all. There is no other channel in kontur today for
getting a per-VM static address into a firmware-booted guest, so
`-max-concurrent` greater than 1 -- every slot needing its own address,
exactly what `KonturConfig.BaseIP`/`BasePort` (bwsalmon/agents#466) exist
for -- would have no way to actually reach the guest. Direct kernel boot
is the only path that is already wired all the way through.

## What's in the image, and why

`provision.sh` mirrors `provision/sandbox.sh` package-for-package: git,
build tooling, Docker + kind (the node image is not pre-pulled -- see
"Why no VM boot to build this" below for why), and `gcloud`/`terraform`
for tasks whose deployment mints a per-task GCP key. bwsalmon/agents#267's
own text asks for exactly this -- "whatever v1's own sandbox image
already carries" -- rather than a leaner image scoped to only what v2's
four current MCP sandbox tools (`run_command`/`read_file`/`edit_file`/
`write_file`) exercise directly. `run_command` runs arbitrary shell, so a
dispatched task's own build/test step can reach for anything in that list
the same way a v1 sandbox lets it; trimming the list now would just move
the "actually I needed X" discovery from this decision to some later
task's failed dispatch, for an image that is not cheap to iterate on the
way a Python provisioning script is (see "One image, uniform" below).

Two things `provision.sh` does that `provision/sandbox.sh` doesn't, both
because a kontur VM has no per-VM provisioning hook analogous to
`LibvirtAdapter.render_domain_xml`/cloud-init NoCloud user-data (kontur
manages a VM's lifecycle as a static pod under a standalone kubelet --
`pkg/kontur`'s doc comment -- not through a cloud provider's metadata
service a NoCloud datasource would ride on):

- **sshd is enabled**, and **the operator's public key is baked into the
  `debian` user's `authorized_keys`** at build time. This is the literal
  thing `pkg/kontur`'s doc comment and `v2/README.md` both say a kontur
  guest image already has to satisfy on its own -- there is nothing
  downstream of `konturctl vm create` positioned to inject it the way
  `LibvirtAdapter.create()` injects a sandbox's authorized key today.
- **The `debian` account itself is created here.** On v1's own sandbox
  base (a stock Debian cloud image), `debian` is cloud-init's
  `default_user`, already present before `provision/sandbox.sh` ever
  runs (that script's own comment: "The default cloud-init user"). This
  image has no cloud-init and no cloud image underneath it at all (see
  "Why no VM boot to build this"), so nothing creates that account except
  `provision.sh` itself -- same name, so every downstream assumption (the
  authorized key above, `grain/adapter/libvirt.py`'s v1 convention, the
  docker-group grant) keeps holding, with the same passwordless-sudo grant
  a cloud image's own `default_user` normally carries.

**Not** baked in, on purpose, matching `provision/controller.sh`'s own
rule ("no secret is ever baked into an image or a provisioning script",
`docs/design.md`, "Secrets on /data"): no GitHub token, no GCP key, no git
identity/credential helper. Per-dispatch git configuration (`credential.
helper = store`, the `grain agent` identity, the proxy token) is set at
runtime against a live sandbox the same way v1's `configure_git_credentials`
(`grain/automation/dispatch.py`) and v2's `mcp.ConfigureGitCredentials`
already do it -- "arrives with git already configured" (`v2/README.md`)
turns out to mean only "the `git` binary is on `PATH`", confirmed against
both functions: neither one assumes any baked-in `.gitconfig`.

Claude Code itself stays off the guest, for the same reason
`provision/sandbox.sh`'s own comment gives for v1: it runs against this
VM's SSH-exposed sandbox tools from the controller/orchestrator side, not
on the guest, so there is nothing here worth a credential leak protecting
in the first place.

## Running a custom setup script

To customize the image beyond `provision.sh`'s own fixed package list --
installing extra packages, dropping in config files, enabling services,
etc -- without forking this directory, set `SANDBOX_SETUP_SCRIPT` to a
script's contents (not a path) before running `build.sh`:

```sh
export SANDBOX_SETUP_SCRIPT="$(cat my-setup.sh)"
sudo -E ./build.sh
```

`provision.sh` runs it, as root, once the built-in provisioning above has
finished but before the operator-key finalization below -- see that
script's own comment on the section for exactly where and why. This is
bwsalmon/kontur's own `GUEST_SETUP_SCRIPT` build arg's idiom
(`third_party/kontur/deploy/guest-image/README.md`, "Running a custom
setup script"), applied to this directory's own build instead: same
"an env var holds the script's contents, not a path" shape, so it needs
no extra context-wrangling either way. The mechanics are actually simpler
here than that one's: this script's chroot has a real, bind-mounted
`/proc`/`/sys`/`/dev` and network access (build.sh sets all of that up for
`provision.sh` itself to use), so none of that Dockerfile build's "no
/proc/sys, no running service manager" caveats apply -- `apt-get install`,
`systemctl enable`, and the like all work normally. Leave
`SANDBOX_SETUP_SCRIPT` unset (the default) to build exactly what
`provision.sh` already bakes in on its own.

Like everything else in this image, the rule from "What's in the image,
and why" above still applies: no secret belongs in a script baked in at
build time, since it ends up in the shipped image for anyone with that
image to read back out.

## How the image gets built and published

### Why no VM boot to build this

This directory previously used [Packer](https://www.packer.io/)'s `qemu`
builder: boot a stock Debian cloud image under QEMU, reach it over SSH via
a throwaway NoCloud cloud-init seed, and run `provision.sh` against the
live, booted VM. That pipeline needed KVM and a full VM boot just to *build*
the image (on top of the KVM a deployment separately needs to *run* it),
had no way to pre-pull the kind node image without a running dockerd
anyway (booting a VM doesn't help there either, since Packer's build step
was still just a provisioner script over SSH -- no different in that
respect from a chroot), and could not produce anything with the file
shape a direct-kernel-booting guest actually needs (a stock Debian cloud
image only ever ships as one bootloader-dependent, GRUB/UEFI qcow2, not a
separate kernel/initramfs/disk triple).

`debootstrap` plus `chroot` needs neither: `debootstrap --variant=minbase`
builds a Debian rootfs directly on the build host's own filesystem, with
no VM involved, and `chroot` (unlike a fresh container, and exactly like
`third_party/kontur`'s own Dockerfile `guest-image` stage) shares the
build host's network namespace, so `apt-get`/`curl` inside the chroot just
work with no extra networking setup. The only things that still need
privilege are `debootstrap` itself, the bind-mounts (`/dev`, `/dev/pts`,
`/proc`, `/sys`) `provision.sh` needs for `update-initramfs`/`systemctl`
to behave normally, and `mke2fs -d`, which packs the finished rootfs
directly into an ext4 image with no loop-mount at all -- `build.sh` has to
run as root (`sudo -E ./build.sh`) for exactly these three things, nothing
more.

### Building and publishing

```sh
sudo apt-get install -y debootstrap e2fsprogs
export OPERATOR_SSH_PUBLIC_KEY="$(cat /path/to/operator_key.pub)"
export KONTUR_IMAGE_BUCKET="<a GCS bucket this deployment's operator controls>"
sudo -E ./build.sh
```

`build.sh` debootstraps a fresh rootfs, runs `provision.sh` against it via
chroot, copies out `/boot/vmlinuz-*`/`/boot/initrd.img-*` as `vmlinuz`/
`initrd.img`, packs the rootfs into `disk.img` (`mke2fs -d`, sized to the
rootfs plus 20% headroom and a 64MiB floor -- the same formula
`third_party/kontur`'s own guest-image Dockerfile stage uses, for the same
reason), and writes all three under
`output/kontur-guest-<git-sha>-<UTC timestamp>/`. If `KONTUR_IMAGE_BUCKET`
is set, `gsutil cp`s all three to both
`gs://$KONTUR_IMAGE_BUCKET/kontur-guest/<same name>/` (a permanent,
versioned copy) and `gs://$KONTUR_IMAGE_BUCKET/kontur-guest/latest/` (a
fixed alias overwritten by every build) -- the second is what
`v2/scripts/setup.sh`'s own `ensure_kontur_images` always fetches
(bwsalmon/agents#504), so a fresh build actually reaches a deployment on
its next `setup.sh` run without that script having to discover or
hardcode today's `<git-sha>-<timestamp>` version string itself. No bucket
name is hardcoded here -- this repo doesn't otherwise touch GCS for
artifacts, and `terraform/gcp/versions.tf`'s own comment on its Terraform
state bucket ("the bucket name lives in the repo as configuration, not
here") is the precedent to follow rather than inventing a
project-specific bucket name a deployment didn't choose; see
`terraform/gcp-v2/variables.tf`'s own `kontur_image_bucket` for where that
name is actually configured for that deployment shape.
`OPERATOR_SSH_PUBLIC_KEY` is not a secret (it's a public key), but is
still left to the environment rather than a repo file, so it's the
deployment's own operator key and not one hand-picked here -- see
`terraform/gcp-v2/README.md`, "Kontur sandboxing", for where that keypair
comes from on that deployment shape (the private half also has to reach
`grain daemon`'s own `-kontur-ssh-key`, via `push-secrets.sh`).

The other half of what a deployment needs published -- the `kontur`
binary and the cloud-hypervisor release bundled with it, not the guest
disk this directory builds -- is `third_party/kontur`'s own Dockerfile, a
plain `docker build`/`docker push` with no root and no debootstrap of its
own needed; see this directory's sibling `build-oci-image.sh` for exactly
that, and `v2/scripts/setup.sh`'s own `ensure_kontur_images` for how a
deployment pulls it back down and retags it to `konturctl`'s own default
image reference.

**The flags `konturctl vm create` takes to point at a built image**,
resolved by reading bwsalmon/kontur's own source directly
(`../../third_party/kontur/`, bwsalmon/agents#351): per
`internal/cli/vm.go`'s `registerVMFlags` and `internal/dockervm/docker.go`'s
`Create`, `konturctl` never fetches an image itself -- `-disk`, `-kernel`
and `-initramfs` (`internal/cli/vm.go`: paths "as seen inside the
container") are paths inside a host directory `-images-hostpath` (default
`/var/lib/vm-images`, `internal/staticpod/spec.go`'s own default) mounts
read-only at `/images` in the VM's container. So a deployment publishing
this directory's `build.sh` output has to land all three files on the
kontur host's own local disk under that directory -- `v2/scripts/
setup.sh`'s own `ensure_kontur_images` is that provisioning step
(bwsalmon/agents#504: nothing downstream of `konturctl vm create` fetches
them there on its own), always landing them at a fixed `<hostpath>/current/`
regardless of the version string `build.sh` gave them in GCS, so
`write_systemd_units`'s own `-kontur-create-arg` construction never has to
discover or hardcode one either:

```sh
konturctl vm create <name> -backend docker \
  -images-hostpath /var/lib/vm-images \
  -disk /images/current/disk.img \
  -kernel /images/current/vmlinuz \
  -initramfs /images/current/initrd.img \
  -guest-port 22
```

**`-guest-port 22` is required**, not optional, for this image specifically:
`konturctl`'s own default (`internal/netshim/config.go`'s `defaultGuestPort`)
is 80, on the assumption a guest is serving HTTP -- netshim's DNAT rule
forwards `-port`'s external port straight to `-guest-port` inside the
guest, so leaving it at 80 against this image (whose only listener is
sshd on 22) means every connection is refused, no matter how long a
caller waits for the guest to finish booting. Confirmed by hand: this was
the single largest time sink in validating this whole pipeline, since a
refused connection looks identical whether the guest hasn't finished
booting yet or is listening on a port nothing is forwarded to.

**One local patch to the vendored `third_party/kontur` copy is required
for the `-disk` path above to actually boot** -- see
`third_party/kontur/VENDORED.md` for what it is and why (`image_type=raw`
on every disk device); a deployment building its own `kontur`/`konturctl`
from a *fresh*, unpatched checkout of bwsalmon/kontur needs the same
one-line change until it lands upstream.

`orchestrator.KonturConfig.CreateArgs` (bwsalmon/agents#262) is the
passthrough a deployment sets this through -- `grain daemon` constructs a
real `KonturSandboxes`/`KonturConfig` from it (bwsalmon/agents#274) via
`-kontur-vm-name-prefix` and a repeatable `-kontur-create-arg` flag, e.g.
`-kontur-create-arg=-images-hostpath -kontur-create-arg=/var/lib/vm-images
-kontur-create-arg=-disk -kontur-create-arg=/images/current/disk.img
-kontur-create-arg=-kernel -kontur-create-arg=/images/current/vmlinuz
-kontur-create-arg=-initramfs -kontur-create-arg=/images/current/initrd.img
-kontur-create-arg=-guest-port -kontur-create-arg=22` (see "`-guest-port 22`
is required" above -- easy to leave out, since a refused connection gives
no hint that the guest is listening on the wrong port rather than still
booting). `v2/scripts/setup.sh`'s own `write_systemd_units` builds exactly
this list whenever `GRAIN_KONTUR_ENABLE=1` (bwsalmon/agents#504) -- see
`terraform/gcp-v2/README.md`, "Kontur sandboxing", rather than wiring it
by hand.
That vendored copy is a point-in-time snapshot with no automation keeping
it current (see its own `VENDORED.md`), so a deployment wiring
`-kontur-create-arg` for the first time should still treat bwsalmon/
kontur's own `konturctl vm create -h` as authoritative if the two ever
disagree.

## Networking

Covered above under "Why no custom kernel" (the `eth0`-naming and
`ip=`-handling gaps and how `provision.sh` closes them) since both are
consequences of the kernel decision, not independent choices.

## One image, uniform

v1 uses exactly one sandbox base image for every sandbox VM, with no
per-task or per-target-repo variant -- whatever a task needs beyond the
base image is either already on it (this directory's whole premise) or
fetched by the task itself at dispatch time via `run_command`. This
directory follows the same rule: `packer/kontur/` builds one image, and a
deployment's `KonturConfig.CreateArgs` (once wired) points every dispatch
slot at the same one. Varying it per task or repo would mean rebuilding
and republishing an image on a per-dispatch cadence, for toolchain
differences `run_command` can already paper over inside a single uniform
image (installing an extra package or language runtime for one run costs
that run some seconds; it doesn't justify a second image to maintain,
test, and keep from drifting out of sync with the first).
