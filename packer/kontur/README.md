# The kontur guest image

`v2/pkg/kontur`'s own package doc comment, and `v2/README.md`'s
`grain mcpserver -kontur-vm` section, both state the same assumption: a
kontur-managed VM's guest image already carries the operator's SSH key and
a running sshd (and, per `v2/README.md`, git) before this repo ever tries
to reach it. Nothing in this repo built that image -- v1 has an equivalent
job for its own libvirt-managed sandboxes (`provision/sandbox.sh`, run as
cloud-init user-data against a shared base image on every VM's first boot);
v2 had no successor, kontur-backed or otherwise. This directory is that
successor: a [Packer](https://www.packer.io/) template that produces a
qcow2 disk image, plus the scripts around it.

bwsalmon/agents#267, which this closes out, is explicit that this is a
"decide, don't just wire" task: what the image needs beyond sshd+key,
how it gets built and published, and whether one image serves every
dispatch slot. The three sections below answer each in turn.

## What's in the image, and why

`provision.sh` mirrors `provision/sandbox.sh` package-for-package: git,
build tooling, Docker + kind (with the kind node image pre-pulled), and
`gcloud`/`terraform` for tasks whose deployment mints a per-task GCP key.
bwsalmon/agents#267's own text asks for exactly this -- "whatever v1's own
sandbox image already carries" -- rather than a leaner image scoped to only
what v2's four current MCP sandbox tools (`run_command`/`read_file`/
`edit_file`/`write_file`) exercise directly. `run_command` runs arbitrary
shell, so a dispatched task's own build/test step can reach for anything
in that list the same way a v1 sandbox lets it; trimming the list now would
just move the "actually I needed X" discovery from this decision to some
later task's failed dispatch, for a compiled/downloaded image that isn't
cheap to iterate on the way a Python provisioning script is (see
"One image, uniform" below).

Two things provision.sh does that `provision/sandbox.sh` doesn't, both
because a kontur VM has no per-VM provisioning hook analogous to
`LibvirtAdapter.render_domain_xml`/cloud-init NoCloud user-data (kontur
manages a VM's lifecycle as a static pod under a standalone kubelet --
`pkg/kontur`'s doc comment -- not through a cloud provider's metadata
service a NoCloud datasource would ride on):

- **sshd is enabled**, and **the operator's public key is baked into the
  `debian` user's `authorized_keys`** at build time, overwriting whatever
  the build's own temporary access used (see "How the build reaches the VM
  at all" below). This is the literal thing `pkg/kontur`'s doc comment and
  `v2/README.md` both say a kontur guest image already has to satisfy on
  its own -- there is nothing downstream of `konturctl vm create` positioned
  to inject it the way `LibvirtAdapter.create()` injects a sandbox's
  authorized key today.
- **cloud-init is disabled** (`systemctl disable cloud-init ...` plus
  `cloud-init clean`) once it has done the one job it has here -- see
  "How the build reaches the VM at all". Left enabled, it would either
  no-op forever against a datasource a kontur guest never presents, or (if
  some future deployment shape ever does present one) silently reconfigure
  networking a kontur/CNI-managed guest doesn't own the way a NoCloud guest
  normally would.

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
./build.sh
```

`provision.sh` runs it, as root, once the built-in provisioning above has
finished but before the operator-key/cloud-init finalization below --
see that script's own comment on the section for exactly where and why.
This is bwsalmon/kontur's own `GUEST_SETUP_SCRIPT` build arg's idiom
(`third_party/kontur/deploy/guest-image/README.md`, "Running a custom
setup script"), applied to this directory's own build instead: same
"an env var holds the script's contents, not a path" shape, so it needs
no extra `packer build`/`docker build` context-wrangling either way. The
mechanics differ where the two builds do -- that one's `chroot`s into an
as-yet-unbooted rootfs (no `/proc`, `/sys`, or running service manager);
this one runs over SSH against a live booted VM, the same as every other
step `provision.sh` already takes, so none of that chroot's caveats apply
here. Leave `SANDBOX_SETUP_SCRIPT` unset (the default) to build exactly
what `provision.sh` already bakes in on its own.

Like everything else in this image, the rule from "What's in the image,
and why" above still applies: no secret belongs in a script baked in at
build time, since it ends up in the shipped qcow2 for anyone with that
image to read back out.

## How the image gets built and published

`image.pkr.hcl` (Packer, `qemu` builder) boots the same stock Debian 12
generic-cloud qcow2 v1's own sandbox base already uses
(`terraform/gcp/variables.tf`'s `debian_image_url`,
`docs/runbook.md`'s first-time setup), runs `provision.sh` against it as
root, and writes out a new qcow2 -- a genuinely pre-baked image, not
v1's "shared base image plus first-boot provisioning" shape, since kontur
has no first-boot hook to run one against.

### How the build reaches the VM at all

Packer's `qemu` builder needs *some* way to SSH into the VM it just booted
from a stock cloud image before `provision.sh` can run at all, and the
stock image has no preset password or key. `build.sh` supplies one the
same way any Packer-against-a-cloud-image recipe does: it generates a
throwaway ed25519 keypair, writes a two-file NoCloud seed
(`user-data`/`meta-data`) authorizing only that key, and hands the seed to
Packer as a `cidata`-labelled CD-ROM -- cloud-init on the *stock* image
picks it up on its own first boot, same NoCloud mechanism
`grain/adapter/libvirt.py` already uses for v1's sandboxes, just scoped to
this one build rather than to a deployment's whole fleet. `provision.sh`'s
own last steps overwrite that throwaway key with the real operator key and
disable cloud-init, so none of this survives into the shipped image; `build.
sh` deletes the seed directory and the throwaway key itself once the build
finishes either way.

### Building and publishing

```sh
export OPERATOR_SSH_PUBLIC_KEY="$(cat /path/to/operator_key.pub)"
export KONTUR_IMAGE_BUCKET="<a GCS bucket this deployment's operator controls>"
./build.sh
```

`build.sh` runs `packer init`/`packer build`, names the output
`kontur-guest-<git-sha>-<UTC timestamp>.qcow2`, and (only if
`KONTUR_IMAGE_BUCKET` is set) `gsutil cp`s it to
`gs://$KONTUR_IMAGE_BUCKET/kontur-guest/<same name>.qcow2`. No bucket name
is hardcoded here -- this repo doesn't otherwise touch GCS for
artifacts, and `terraform/gcp/versions.tf`'s own comment on its Terraform
state bucket ("the bucket name lives in the repo as configuration, not
here") is the precedent to follow rather than inventing a project-specific
bucket name a deployment didn't choose. `OPERATOR_SSH_PUBLIC_KEY` is not a
secret (it's a public key), but is still left to the environment rather
than a repo file, so it's the deployment's own operator key and not one
hand-picked here.

**The flag `konturctl vm create` takes to point at a built image**, now
resolved by reading bwsalmon/kontur's own source directly (as of
bwsalmon/agents#351 it is readable locally at `../../third_party/kontur/`,
rather than only reachable from a sandbox that happens to hold proxy
access to the private repo the way bwsalmon/agents#267 found it didn't):
per `internal/cli/vm.go`'s `registerVMFlags` and
`internal/dockervm/docker.go`'s `Create`, `konturctl` never fetches an
image itself -- `-disk` (`internal/cli/vm.go`: "path to the VM's disk
image, as seen inside the kontur container, e.g. `/images/disk.img`") is
a path inside a host directory `-images-hostpath` (default
`/var/lib/vm-images`, `internal/staticpod/spec.go`'s own default) mounts
read-only at `/images` in the VM's container. So a deployment publishing
this directory's `build.sh` output has to land the built qcow2 on the
kontur host's own local disk under that directory (`gsutil cp`/`gcloud
storage cp` from wherever `KONTUR_IMAGE_BUCKET` published it, e.g. via a
startup script or provisioning step, since nothing downstream of
`konturctl vm create` fetches it there on its own) and then pass:

```sh
konturctl vm create <name> -backend docker \
  -images-hostpath /var/lib/vm-images \
  -disk /images/<same qcow2 filename as under -images-hostpath>
```

`orchestrator.KonturConfig.CreateArgs` (bwsalmon/agents#262) is the
passthrough a deployment sets this through -- `grain daemon` constructs a
real `KonturSandboxes`/`KonturConfig` from it (bwsalmon/agents#274) via
`-kontur-vm-name-prefix` and a repeatable `-kontur-create-arg` flag, e.g.
`-kontur-create-arg=-images-hostpath -kontur-create-arg=/var/lib/vm-images
-kontur-create-arg=-disk -kontur-create-arg=/images/kontur-guest-....qcow2`.
That vendored copy is a point-in-time snapshot with no automation keeping
it current (see its own `VENDORED.md`), so a deployment wiring
`-kontur-create-arg` for the first time should still treat bwsalmon/
kontur's own `konturctl vm create -h` as authoritative if the two ever
disagree.

## One image, uniform

v1 uses exactly one sandbox base image for every sandbox VM, with no
per-task or per-target-repo variant -- whatever a task needs beyond the
base image is either already on it (this directory's whole premise) or
fetched by the task itself at dispatch time via `run_command`. This
directory follows the same rule: `packer/kontur/` builds one image, and a
deployment's `KonturConfig.CreateArgs` (once wired) points every dispatch
slot at the same one. Varying it per task or repo would mean rebuilding
and republishing an image -- itself a multi-minute Packer build, unlike
v1's per-VM provisioning script -- on a per-dispatch cadence, for
toolchain differences `run_command` can already paper over inside a single
uniform image (installing an extra package or language runtime for one
run costs that run some seconds; it doesn't justify a second image to
maintain, test, and keep from drifting out of sync with the first).
