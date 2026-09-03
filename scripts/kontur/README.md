# The kontur guest image

`pkg/kontur`'s own package doc comment, and `README.md`'s
`grain mcpserver -kontur-vm` section, both state the same assumption: a
kontur-managed VM's guest image already carries the operator's SSH key and
a running sshd (and, per `README.md`, git) before this repo ever tries
to reach it. Nothing in this repo built that image -- v1 has an equivalent
job for its own libvirt-managed sandboxes (v1's own sandbox provisioning script, run as
cloud-init user-data against a shared base image on every VM's first boot);
v2 had no successor, kontur-backed or otherwise. This directory is that
successor: `build-guest.sh` derives a grain sandbox guest from a
published kontur image -- booting it, running `guest-setup.sh` inside,
scrubbing and committing -- and what comes out is itself a runnable
kontur image, carrying a guest cloud-hypervisor direct-kernel-boots with
no bootloader, firmware, or partition table anywhere in the picture.

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

## Converged on kontur's own guest build

This directory used to run its own pipeline -- `build.sh` driving
`provision.sh` through debootstrap and chroot, as root -- which duplicated
work `third_party/kontur`'s own Dockerfile already does: it debootstraps a
`--variant=minbase` rootfs, applies its overlays, and packs the result
with `mke2fs -d` -- *"no extra privileges beyond an ordinary docker
build"*. That pipeline is gone. Nothing here builds a guest rootfs any
more: `build-guest.sh` hands kontur's build-time guest setup hook
(`GUEST_SETUP_SCRIPT`) a script saying only what grain adds, and
`guest-setup.sh` is that script.

Beyond the one-build-instead-of-two win below, building *on* kontur's
rootfs rather than beside it means the guest carries kontur's own guest
overlays. That is what makes kontur's flat networking mode usable here:
its control link is configured guest-side by `kontur-control-net`, one of
those overlays, which a rootfs built from scratch in this directory never
had.

What that buys: one build instead of two, no root/debootstrap requirement
for a guest image build, and -- since bwsalmon/kontur#35 -- a guest image
with no key in it at all, which retired `-kontur-exec-key` and the
key-staging step `scripts/setup.sh` used to do for it.

### Status: both the kernel and the hook have landed upstream

The `third_party/kontur` resync (upstream `dd6e306`) bakes a guest kernel
into the kontur OCI image -- a new `fetch-kernel` stage pulling
cloud-hypervisor's own published PVH `vmlinux`, plus a `CHV_KERNEL`
default pointing at it. That closes kontur's "a kernel comes from
somewhere outside the image" gap **for kontur's own bundled guest**.

It does not close it for grain's, and grain should not adopt that kernel:
this guest runs docker and `kind`, which need a far richer kernel config
(overlayfs, cgroup v2, bridge netfilter, veth) than a release kernel built
for cloud-hypervisor's own CI is likely to carry, and "Why no custom
kernel" below records Debian's stock `linux-image-amd64` being verified by
hand under real KVM for exactly this guest. So `guest-setup.sh` installs
`linux-image-amd64` itself, and the hook's `guest-artifacts` target
publishes the resulting `vmlinuz`/`initrd.img` beside `disk.img`.

The build-time setup hook has now landed too, on kontur's `main`
(bwsalmon/kontur#28), and is in this repo's `third_party/kontur`
snapshot. kontur's build args are now `GO_VERSION`,
`CLOUD_HYPERVISOR_VERSION`, `KONTUR_KERNEL_VERSION`, `GUEST_DISTRO`,
`GUEST_SUITE`, `GUEST_ALPINE_VERSION`, `GUEST_SSH_AUTHORIZED_KEY` and
`GUEST_SETUP_SCRIPT` -- the last being the one this directory was waiting
on. It holds the script's own *text*, not a path.

### What the hook does, and the one problem it exists to solve

Customizing kontur's guest rootfs is not simply "run a script in it",
because the rootfs is a *directory* in a build stage. Running anything in
it means `chroot`, and a useful chroot needs `/proc` and `/dev`
bind-mounted -- which needs `CAP_SYS_ADMIN`, which an ordinary
`docker build` does not have. Without them, apt postinsts,
`systemctl enable` and `update-initramfs` variously misbehave or fail.
This is precisely why `build.sh` needed root while it existed, and why
keeping that build would have undone the "no extra privileges beyond an
ordinary docker build" property kontur's own `guest-image` stage calls out
for `mke2fs -d`.

It sidesteps that rather than paying it: a `guest-customized` stage
promotes the rootfs to an image of its own
(`FROM scratch` + `COPY --from=guest-rootfs /rootfs/ /`), so
`GUEST_SETUP_SCRIPT` runs as an ordinary `RUN` -- real `/proc`, real
`/dev`, working network, rootfs as `/` -- with no chroot and no
privileges. `guest-image` then packs that stage instead of the raw
rootfs. There is also a `guest-artifacts` target, so
`docker build --target guest-artifacts --output type=local,dest=<dir>`
yields `disk.img` plus the guest's own kernel and initramfs when it has
them, without bloating the runtime image with any of it. `final` stays
last, so the default build target is unchanged. See
`third_party/kontur/deploy/guest-image/README.md`'s "Running a custom
setup script" and "Exporting the built guest".

### Not yet verified

The hook was written and reviewed, and landed upstream, without ever
being built: no image registry is reachable from where it was written
(the egress policy denies Docker's blob CDN, so `docker build` cannot
resolve a single base image), and there is no `/dev/kvm` to boot the
result under. What needed checking on a machine that can:

1. **Device nodes survive the copy.** `COPY --from=guest-rootfs /rootfs/ /`
   has to carry debootstrap's `/dev` entries through BuildKit. If it
   chokes, empty `/dev` in the rootfs stages first -- both variants'
   kernels mount devtmpfs over it during early boot anyway.
2. **`RUN` works on the scratch-derived stage.** It needs the `/bin/sh`
   the rootfs provides; the patch sets `PATH` explicitly rather than
   relying on the runtime's default for an image config that sets none.
3. **The Alpine variant still builds.** The hook is written to be
   distro-agnostic (busybox `sh` satisfies it), but only the Debian path
   is what grain exercises.
4. **`update-initramfs` inside the RUN** produces an initramfs the guest
   actually boots from -- the step `guest-setup.sh` depends on for its
   `eth0`/`ip=` units to stick.

**Verified live (bwsalmon/agents#577), for the Debian path.** A fresh
n2-standard-4 GCE VM with nested virtualization, running nothing but
`scripts/setup.sh GRAIN_KONTUR_ENABLE=1`, built this image
(`docker build --target guest-artifacts`) end to end with no patching,
`konturctl vm create` booted it under real `cloud-hypervisor`/KVM, and a
real `grain daemon` dispatch reached it through the full production path
(`pkg/mcp/docker_exec_runner.go`'s `docker exec <container> kontur exec --
whoami`, over the guest's own sshd) and got back `debian`. All four items
above check out: the device nodes, the `RUN` stage, and the regenerated
initramfs all worked without local changes. Only item 3 (the Alpine
variant) remains unverified -- this run only ever exercised Debian, same
as every other deployment. `scripts/kontur-diag.sh` needed a real fix
first (it predated `-net flat` becoming the default and misdiagnosed a
healthy flat-mode guest as broken); see that script's own header for
what changed.

### Two things measured while porting, both load-bearing

Neither is a matter of taste; each produces a guest that boots cleanly and
then fails in a way that says nothing about its cause.

**kontur's guest has no `ip=` handling, and grain's kernel needs some.**
kontur's `deploy/guest-image/README.md` states its guest needs no
guest-side networking setup because *"it relies on the kernel's built-in
`ip=` boot-time autoconfiguration"*. That holds only for a kernel with
`CONFIG_IP_PNP`, which kontur ships none to have. Debian's stock kernel
does **not** enable it, so nothing acts on `ip=` without the klibc
`ipconfig` unit `guest-setup.sh` keeps -- and `konturctl` hard-codes
`eth0` in the `ip=` it derives, which systemd's predictable-naming policy
renames away from without the link file beside it. Neither has any
equivalent in kontur's overlays.

**kontur's ForceCommand console wrapper breaks grain's sandbox tools.**
`overlay-common/etc/ssh/sshd_config.d/10-console.conf` forces every SSH
session on the guest through `kontur-ssh-console-wrap`, which runs the
session's command under `script` so its output is mirrored to the serial
console. `script` runs the command under a **pty**, and a pty is not a
transparent pipe. Measured against the real wrapper:

| | through the wrapper |
|---|---|
| `cat` of a file with `\n` endings | comes back `\r\n` -- `read_file` corrupts every file it reads |
| stdout vs stderr | merged onto the one pty -- `run_command` loses the split, and a failed `cat`/`dd` reports an empty error |
| exit status | survives (`script --return`) |
| stdin | survives byte-for-byte |

So `guest-setup.sh` replaces that drop-in, keeping its two hardening lines
and dropping the `ForceCommand`. This is the one place where building on
kontur's guest actively breaks something rather than merely not helping.

### Wiring: done, then superseded

For a while `build-guest.sh` ran exactly one `docker build` against
`third_party/kontur`, handing `guest-setup.sh` to its
`GUEST_SETUP_SCRIPT` build arg and exporting `disk.img`/`vmlinuz`/
`initrd.img` through `--target guest-artifacts --output type=local`. That
is no longer what happens here, but it is still how the *base* image this
directory derives from gets built -- kontur's CI runs it, with
`GUEST_KERNEL_PACKAGE=linux-image-amd64` and `GUEST_CONSOLE_WRAP=0`, and
publishes the result. See "How the image gets built and published" below
for what this directory does on top of it.

Two variables that shaped this directory's history are worth recording
because both are gone. `SANDBOX_SETUP_SCRIPT` was a pass-through for a
caller wanting to add to `guest-setup.sh`, spliced into that script's own
text; deriving from a published image replaced it (see "Customizing this
guest"). `OPERATOR_SSH_PUBLIC_KEY` was what made the image
deployment-specific at all, and its removal is what made everything since
possible: bwsalmon/kontur#35 moved the `kontur exec` keypair to one
generated in the VM's own container on every boot, handed to the guest on
its kernel command line, with `konturctl vm create -guest-user` naming
the account to authorize it for -- which closed the last gap here, kontur
authorizing **root** while this guest's sandbox account is `debian`.
`-kontur-exec-key` and the key staging `scripts/setup.sh` did for it are
both gone.

What it does read is two variables `build-guest.sh` reads out of the tree
and splices into the script's own text, immediately after the shebang:
`GO_VERSION` (from `go.mod`) and `GRAIN_DEP_MANIFESTS` (the four
dependency manifests, gzipped and base64'd). See "The Go and Node
toolchains" below. konturctl runs the setup script with only the guest's
own environment, so a variable that is not in the script text does not
reach it.

See "Verified live (bwsalmon/agents#577)" above -- the build has now been
run and the result booted, for the Debian path.

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
`/boot/initrd.img-*` out of a normal `apt-get install linux-image-amd64`.
`guest-setup.sh` used to run that install itself; kontur's own
`GUEST_KERNEL_PACKAGE` does it now (bwsalmon/kontur#36), which is what
lets the base image carry the kernel, the initramfs and the boot-time
`ip=` handling that depends on it -- so nothing in this directory installs
a kernel or regenerates an initramfs any more.
There is no from-source kernel build anywhere in this pipeline, and no
firmware/GRUB/UEFI boot path either (cloud-hypervisor's other option,
`CHV_FIRMWARE` -- see below for why that path was ruled out, not just left
unexplored).

Two gaps still had to be closed by hand, both confirmed by booting the
actual built image under real KVM before this pipeline was considered
done. Both live in kontur's own Debian guest overlay now
(bwsalmon/kontur#36) rather than in `guest-setup.sh`, since both are
consequences of kontur's addressing rather than anything grain wants:

- **The guest's one NIC needs to be named `eth0`.** `konturctl` itself
  (`internal/staticpod/spec.go`) derives each VM's static-addressing
  kernel command line as `ip=<ip>::<gateway>:<netmask>::eth0:off`, with
  `eth0` hard-coded -- but systemd's predictable-naming policy renames a
  virtio-net device to something like `ens2` by default, and the rename
  happens inside the *initramfs* (its own bundled, and by default stale,
  copy of `/etc/udev`), not just the final root. `guest-setup.sh` turns
  predictable naming off wholesale -- masking the default `.link` file
  with `ln -sf /dev/null /etc/systemd/network/99-default.link` -- and
  re-runs `update-initramfs -u` afterwards, so the initramfs's own udev
  snapshot has it too; skipping that second step was confirmed by hand to
  leave the guest unreachable regardless. Masking beat the earlier
  `00-eth0.link`, which forced `Name=eth0` on `Type=ether`: that matches
  every NIC and can only win once, so kontur's flat mode -- which gives
  this guest two, the spliced NIC plus the control link -- left the second
  unnamed and the control link unconfigured. Masking leaves the kernel's
  own `eth0`/`eth1` in PCI probe order, and a single-NIC NAT-mode guest
  still gets exactly `eth0`. See `guest-setup.sh`'s own "Networking"
  comment.
- **Static addressing itself needs a small userspace helper.** Debian's
  kernel does not set `CONFIG_IP_PNP`, so nothing in the kernel itself
  acts on `ip=` the way a kernel built with in-kernel IP-config would.
  klibc's `ipconfig(8)` (pulled in transitively by `initramfs-tools`, via
  `klibc-utils`) implements the same static-addressing syntax, but --
  unlike the in-kernel code -- does not read `/proc/cmdline` itself; it
  only takes the spec as an explicit argument. `guest-setup.sh` installs
  a small wrapper (`kontur-configure-net`) plus a `systemd` oneshot unit
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
getting a static address into a firmware-booted guest, so a NAT-mode
deployment -- the one that still passes an address at all, through
`KonturConfig.IP`/`Port` (bwsalmon/agents#466, when those were still a
per-slot `BaseIP`/`BasePort`) -- would have no way to actually reach the
guest. Direct kernel boot is the only path that is already wired all the
way through.

## What's in the image, and why

`guest-setup.sh` mirrors v1's sandbox provisioning package-for-package: git,
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

Two things `guest-setup.sh` does that v1's sandbox script didn't, both
because a kontur VM has no per-VM provisioning hook analogous to
`LibvirtAdapter.render_domain_xml`/cloud-init NoCloud user-data (kontur
manages a VM's lifecycle as a static pod under a standalone kubelet --
`pkg/kontur`'s doc comment -- not through a cloud provider's metadata
service a NoCloud datasource would ride on):

- **sshd is enabled**, and **the operator's public key is baked into the
  `debian` user's `authorized_keys`** at build time. This is the literal
  thing `pkg/kontur`'s doc comment and `README.md` both say a kontur
  guest image already has to satisfy on its own -- there is nothing
  downstream of `konturctl vm create` positioned to inject it the way
  `LibvirtAdapter.create()` injects a sandbox's authorized key today.
- **The `debian` account itself is created here.** On v1's own sandbox
  base (a stock Debian cloud image), `debian` is cloud-init's
  `default_user`, already present before v1's sandbox script ever
  runs (that script's own comment: "The default cloud-init user"). This
  image has no cloud-init and no cloud image underneath it at all (see
  "Why no VM boot to build this"), so nothing creates that account except
  `guest-setup.sh` itself -- same name, so every downstream assumption (the
  authorized key above, v1's own libvirt-driver convention, the
  docker-group grant) keeps holding, with the same passwordless-sudo grant
  a cloud image's own `default_user` normally carries.

**Not** baked in, on purpose, matching v1's controller provisioning's own
rule ("no secret is ever baked into an image or a provisioning script",
`docs/design.md`, "Secrets on /data"): no GitHub token, no GCP key, no git
identity/credential helper. Per-dispatch git configuration (`credential.
helper = store`, the `grain agent` identity, the proxy token) is set at
runtime against a live sandbox the same way v1's `configure_git_credentials`
(v1's Python dispatch) and `mcp.ConfigureGitCredentials`
already do it -- "arrives with git already configured" (`README.md`)
turns out to mean only "the `git` binary is on `PATH`", confirmed against
both functions: neither one assumes any baked-in `.gitconfig`.

Claude Code itself stays off the guest, for the same reason
v1's sandbox script gave for itself: it runs against this
VM's SSH-exposed sandbox tools from the controller/orchestrator side, not
on the guest, so there is nothing here worth a credential leak protecting
in the first place.

### The Go and Node toolchains, and why their caches are the point

A sandbox is where the merge queue's own fix tasks run
(`orchestrator.fileFixTask`). Every one of them used to end its commit
message with some version of the same sentence -- *"not built or run:
this sandbox has no Go toolchain and no network to fetch one"* -- and a
fix agent that cannot run `go test ./...` is guessing. A merge fix that
is a guess costs another queue cycle when it turns out not to be the fix,
which is the whole thing the queue exists to avoid. So the image carries:

- **Go**, at the version `go.mod` asks for. `build-guest.sh` reads it out
  of `go.mod` with the same `sed` the `Makefile` uses for
  `Dockerfile.build`, so the image and the module cannot drift apart
  while only one of them is ever edited, and `GOTOOLCHAIN=local` (also
  `Dockerfile.build`'s pin, for the same reason) turns a stale image into
  an error naming both versions rather than a network fetch that cannot
  succeed here.
- **Node**, at the major `.github/workflows/tests.yml` pins for the `go`
  job. Debian's own `nodejs` is 18, and `vitest` -- what `npm test` runs
  -- needs 20.19 or newer. `tests/deploy` asserts the two files keep
  naming the same major.
- **A warm module cache and a warm npm cache**, holding what `go.sum` and
  `ui/package-lock.json` resolve to at build time. `build-guest.sh`
  carries the four manifests in (gzipped and base64'd -- see its own
  comment on why) and `guest-setup.sh` warms both caches into
  `/home/debian`'s own defaults, so nothing has to point either tool at
  them and both stay writable by the account that uses them.

These caches were written as the load-bearing half rather than an
optimization, on the premise that a dispatched sandbox reached nothing
but the git proxy and a cold cache therefore could not build anything at
all. That premise was a bug: flat mode derived the guest's `ip=`
parameter with an empty gateway field, so every sandbox booted without a
default route and lost the open egress `docs/design.md` says it has. The
fix is in the runtime image, not this one
(`third_party/kontur/VENDORED.md`, "Local patches") -- nothing in this
directory changed. With it, a cold cache is survivable, and the caches
earn their place the ordinary way: not re-fetching the same module graph
on every dispatch, and still working if a deployment narrows egress again
(`egress_policy(allowlist)`).

**Playwright's browsers are deliberately not here.** `npx playwright
install` is a ~300MB download per image, and the suite it feeds
(`make test-e2e`) is a separate CI job from the `go` job that merge fixes
keep tripping over. But `@playwright/test` is in `ui/`'s
devDependencies, and its install script fetches those browsers -- so a
plain `npm ci` in `ui/`, which is what `make frontend` and therefore
`make test` runs, would spend that download on a suite this guest does
not run (and, before egress worked at all, would fail outright on it).
The image wraps `npm` to default `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD` on
for exactly that, with `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD= npm ...` as the
way back to fetching them.

The cost is disk, and it is charged differently now that this guest is
provisioned by booting one rather than during an image build. Everything
`guest-setup.sh` installs has to fit in space `disk.img` was given when
kontur packed it -- rootfs plus 20% plus `GUEST_DISK_EXTRA_MB`, which
`build-guest.sh` asks for on the base build -- so the toolchains and
their caches are roughly a gigabyte of that budget rather than a
gigabyte the filesystem simply grew to hold. It is paid on every pull of
the published image. Weighed against a fix agent that can only read a
diff and reason about what CI would have done with it, that is still the
cheaper side.

## Running a custom setup script
## Customizing this guest

There is no environment variable for it any more, and that is a
simplification rather than a removal. `SANDBOX_SETUP_SCRIPT` existed
because the only way to add to this guest was to get a second script into
a `docker build` that could not otherwise reach one -- `build-guest.sh`
spliced its text into `guest-setup.sh` before handing that to kontur's
`GUEST_SETUP_SCRIPT` build arg, which is as awkward as it sounds.

Customizing is now the same operation that built this image in the first
place: derive from it.

```sh
konturctl guest build \
  -from ghcr.io/bwsalmon/grain/guest:latest \
  -setup ./my-setup.sh \
  -t my-guest:dev
```

What comes out is another runnable kontur image, which can itself be the
`-from` of the next one. So the extension point is the tool, the
customization is an ordinary script rather than a variable holding a
script's text, and nothing here has to know it happened.

The environment such a script gets is a *booted* guest: systemd is
running, so `systemctl start` works and dockerd can actually run --
unlike the container build the old hook ran in, where anything that
needed a running service manager was simply unavailable. It needs
`/dev/kvm` on the machine doing the derivation, which is the trade "It
boots a VM now" below spells out.

Like everything else in this image, the rule from "What's in the image,
and why" above still applies: no secret belongs in a script baked in at
build time, since it ends up in the shipped image for anyone with that
image to read back out.

## How the image gets built and published

### It boots a VM now, and that is the point

Two earlier answers to "how does this get built" both avoided booting
anything, and the reasoning that ruled a VM boot out is worth keeping
straight from the reasoning that brought one back.

The original pipeline used [Packer](https://www.packer.io/)'s `qemu`
builder: boot a stock Debian cloud image, reach it over SSH via a
throwaway NoCloud seed, provision the live VM. That was ruled out because
it could not produce the file shape a direct-kernel-booting guest needs --
a stock cloud image ships as one bootloader-dependent GRUB/UEFI qcow2,
not a kernel/initramfs/disk triple -- and because its provisioning step
was a shell script over SSH, no more capable than a chroot for the one
thing anyone wanted a boot for (pre-pulling the kind node image, which
needs a running dockerd).

A `docker build` against `third_party/kontur`'s own Dockerfile replaced
it and needed neither root nor a VM: `debootstrap` in one stage,
`guest-setup.sh` as an ordinary `RUN` in the next, `mke2fs -d` in the
third. That is still how the *base* image is built, and it is still
unprivileged.

What changed is what this directory builds on top of it. A guest is now
derived from a published kontur image by `konturctl guest build`: boot
it, run `guest-setup.sh` inside over `kontur exec`, scrub per-boot
identity, power it off, commit the container. So a build here does boot a
VM, and the trade is deliberate:

- **It costs `/dev/kvm` on whoever builds.** CI has it; a machine without
  it cannot build this image at all. That is the property the docker
  build had and this one spends.
- **It buys a setup script that runs against the real thing.** systemd is
  up, so `systemctl start` works and dockerd actually runs. The
  install-only restrictions a container build imposes -- and the ordering
  hazards that come with them -- are gone. It is also, finally, where the
  kind node image could be pre-pulled.
- **It is not something a deployment pays.** The point of deriving from a
  *published* image is that this happens once, in CI, and every host
  pulls the result. A deployment used to run this build itself, on every
  host, on every deploy generation.

The reason it could not work this way before was not the boot. It was the
key: `guest-setup.sh` baked the deployment's own SSH public key into
`authorized_keys`, so no generic published image could exist. kontur
generates that keypair per VM boot now, and nothing deployment-specific
reaches the disk.

### What comes out is another kontur image

Not a disk to hand somewhere. `konturctl guest build` commits the
container it provisioned, and that container was a kontur image -- so the
result carries `kontur`, `cloud-hypervisor` and the guest disk, and
`docker run` on it boots a VM exactly as the base does. Three
consequences worth naming:

- The sandbox container and the guest are one artifact. `scripts/setup.sh`
  pulls one image and passes `konturctl` one `-kontur-image`; there is no
  `-disk`/`-kernel`/`-initramfs` and no host directory to resolve them
  against.
- It can be the input to another build. Customizing this guest is
  `konturctl guest build --from` it, which is the same mechanism that
  produced it -- so the extension point is the tool rather than an
  environment variable spliced into a script.
- `docker commit` carries the base's config forward, including
  `org.opencontainers.image.source`. On a kontur base that names
  *kontur's* repository, and GHCR uses that label alone to decide which
  repository a package belongs to -- so `build-guest.sh` relabels when
  `GUEST_SOURCE_REPO` is set. Without it the build is green, the push is
  green, and the package lands in someone else's namespace.

### Building and publishing

```sh
IMAGE=ghcr.io/bwsalmon/grain/guest:dev ./build-guest.sh
```

`build-guest.sh` builds `konturctl` out of `third_party/kontur` -- not
from `PATH`, because a konturctl that happened to be installed here is
not the one this guest is built to match -- then builds the base out of
that same tree and runs `konturctl guest build` with `guest-setup.sh` as
the setup script. It needs docker, `/dev/kvm` and Go.

The disk that comes out is the rootfs plus 20% plus whatever
`GUEST_DISK_EXTRA_MB` asked for, and that is also the whole of a
sandbox's disk unless something says otherwise, since `konturctl` gives
each VM an overlay at exactly its backing image's size. `sandbox-disk-gb`
(Settings -> Sandbox, or a per-task override) is what says otherwise,
reaching `konturctl vm create` as `-disk-size-gb`; the guest's own half
of it is the `grain-growfs` unit `guest-setup.sh` installs, which grows
the root filesystem onto the larger device on each boot. Nothing about
the image build changes for it -- a bigger disk is a create-time
argument, not a different image.

No SSH key is involved anywhere in this. The image carries none
(bwsalmon/kontur#35 -- kontur generates one per VM boot instead), so
there is nothing here for `push-secrets.sh` to carry and no keypair for a
deployment and its guest image to keep in sync.

The base is built too, out of `third_party/kontur`, with the two args
kontur's CI publishes its `debian12` variant with plus the disk headroom
this guest installs into (`GUEST_DISK_EXTRA_MB`, above). It was pulled at
first, by the immutable per-commit tag kontur's CI writes -- which does
not work from grain's CI at all, for a reason worth recording rather than
rediscovering: a GitHub Actions `GITHUB_TOKEN` is scoped to its own
repository, so grain's can push grain's packages and cannot read
kontur's, and the pull is denied however the login went.

Building it is the better answer regardless of that. `konturctl` is built
from the vendored tree and drives a guest built from the vendored tree,
so "they are one commit" holds by construction -- rather than by a pinned
tag someone has to remember to move alongside the vendored SHA.
`KONTUR_GUEST_BASE` overrides it with an image of your own, published or
local, and skips the build.

`.github/workflows/build-artifacts.yml` runs exactly this on every
branch, publishes the result as `guest`, and stamps its per-commit tag
into the grain image built from the same commit
(`cmd/grain/sandboximage.go`). A deployment resolves it by asking its own
binary, so grain and its sandbox are always one commit's worth of each
other -- including after a rollback, which asks for its own older sandbox
rather than whatever is newest.

## Networking

Covered above under "Why no custom kernel" (the `eth0`-naming and
`ip=`-handling gaps and how `guest-setup.sh` closes them) since both are
consequences of the kernel decision, not independent choices.

## One image, uniform

v1 uses exactly one sandbox base image for every sandbox VM, with no
per-task or per-target-repo variant -- whatever a task needs beyond the
base image is either already on it (this directory's whole premise) or
fetched by the task itself at dispatch time via `run_command`. This
directory follows the same rule: `scripts/kontur/` builds one image, and a
deployment's `KonturConfig.CreateArgs` (once wired) points every dispatch
slot at the same one. Varying it per task or repo would mean rebuilding
and republishing an image on a per-dispatch cadence, for toolchain
differences `run_command` can already paper over inside a single uniform
image (installing an extra package or language runtime for one run costs
that run some seconds; it doesn't justify a second image to maintain,
test, and keep from drifting out of sync with the first).
