#!/bin/sh
# Turns a stock kontur guest into a grain sandbox guest: the toolchain a
# dispatched task runs against, plus the accounts and daemon settings that
# toolchain needs.
#
# CONTRACT: runs as root, over `kontur exec`, inside a *booted* VM --
# scripts/kontur/build-guest.sh boots the base image, runs this, scrubs
# per-boot identity and commits the result (see `konturctl guest build`).
# So unlike its predecessor, which ran as a RUN in a container during an
# image build, this has a running kernel and a running systemd:
# `systemctl start` works, dockerd can actually run, and anything that
# needs to observe the machine it is configuring can.
#
# That is also the one hazard to keep in mind. A package whose postinst
# *starts* a service starts it here for real, with whatever configuration
# is on disk at that moment -- see the docker block below, which writes
# /etc/docker/daemon.json before installing docker precisely because
# dockerd would otherwise come up on the default bridge subnet and stay
# there.
#
# What this deliberately does NOT do, because the base image
# (ghcr.io/bwsalmon/kontur:debian12-*, built with
# GUEST_KERNEL_PACKAGE=linux-image-amd64 and GUEST_CONSOLE_WRAP=0) now
# carries all of it:
#
#   - install a kernel, or regenerate the initramfs. The base has Debian's
#     linux-image-amd64 and an initramfs generated with kontur's udev
#     mask already in place. Nothing here writes udev rules or
#     modules-load.d entries, which is what would make a regeneration
#     necessary.
#   - configure networking from the "ip=" kernel parameter, or keep the
#     guest's NICs named eth0/eth1. kontur-net-cmdline.service and the
#     udev mask do both.
#   - undo kontur's SSH console wrapper. The base is built without it, so
#     `kontur exec` output is byte-transparent rather than passing
#     through a pty that rewrites newlines and merges stderr into stdout.
#   - install any SSH key. kontur generates a keypair per VM boot and
#     passes the public half on the kernel command line.
#
# There is no operator hook here either. Customizing this guest is
# `konturctl guest build --from <this image>` with a script of your own,
# which is the same mechanism that built it -- so the extension point is
# the tool rather than an environment variable spliced into a script.
#
# Two inputs, both required, both environment variables rather than files
# (nothing here is ever written into this repo). konturctl runs this
# script with only the guest's own environment, so build-guest.sh splices
# each one into the text of the script immediately after the shebang --
# which has to stay on line 1:
#   GO_VERSION               the Go toolchain to install, read out of this
#                            repo's own go.mod by build-guest.sh -- see
#                            "The Go and Node toolchains" below.
#   GRAIN_DEP_MANIFESTS      base64 of a gzipped tar of go.mod, go.sum,
#                            ui/package.json and ui/package-lock.json,
#                            which is what the module and npm caches are
#                            warmed from.
set -eux

KIND_VERSION="v0.32.0"
# Node is pinned here rather than read out of a file, because no file in
# this repo names a full one: .github/workflows/tests.yml pins the major
# ("node-version: 20") and package.json pins none at all. The major has
# to keep matching that workflow -- a sandbox agent reproducing a red
# `npm test` on a different major is reproducing a different thing --
# which tests/deploy asserts across the two files.
NODE_VERSION="20.19.0"

# Both are spliced in by build-guest.sh; running this script by hand
# needs them set the same way. Checked here rather than left to fail
# further down, where an empty GO_VERSION would fetch a 404 and an empty
# manifest bundle would quietly produce an image whose toolchain works
# only on a machine with a network -- which is exactly the machine this
# image is never on.
if [ -z "${GO_VERSION:-}" ] || [ -z "${GRAIN_DEP_MANIFESTS:-}" ]; then
  echo "guest-setup.sh: GO_VERSION and GRAIN_DEP_MANIFESTS must both be set -- build-guest.sh reads them out of the tree and splices them in" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update

# The sandbox toolchain -- "whatever v1's own sandbox image already
# carries" (bwsalmon/agents#267), matching provision/sandbox.sh's list.
# sudo and libnss-myhostname are not toolchain: see the "debian" account
# and hostname blocks below for what needs each.
#
# xz-utils is here because `tar -J` shells out to the xz binary, and the
# Node tarball below is the only .tar.xz this script unpacks; minbase has
# liblzma5 (dpkg needs it) but not the command. e2fsprogs is named for
# the same kind of reason klibc-utils is in the base: resize2fs is what
# the grain-growfs unit below execs, and while debootstrap --variant=
# minbase does install this Priority: required package today, a guest
# that silently lost it would boot fine and simply never grow onto the
# disk its VM was created with -- a failure that looks like
# "-disk-size-gb did nothing", several layers from its cause.
apt-get install -y --no-install-recommends \
  sudo libnss-myhostname e2fsprogs \
  git curl jq ripgrep fd-find build-essential python3 python3-venv \
  pipx tmux unzip xz-utils ca-certificates bubblewrap gnupg

# --- Hostname resolution. libnss-myhostname's postinst does not itself
# edit this conffile (an existing /etc/nsswitch.conf predates it in every
# debootstrap run), so the module has to be wired into the hosts line by
# hand to apply. Without it sudo -- which resolves the local hostname for
# its own logging -- blocks for a long DNS timeout against a guest with no
# real nameserver. Confirmed by hand.
sed -i 's/^hosts:.*/hosts:          files myhostname dns/' /etc/nsswitch.conf

# --- The "debian" account. On v1's sandbox base (a stock Debian cloud
# image) this was cloud-init's default_user; this image has no cloud-init,
# so it is created here instead -- same name, so every assumption
# downstream of it keeps holding: grain/adapter/libvirt.py's v1
# convention, and the docker group grant below. Passwordless sudo matches
# what cloud-init's default_user grants on every cloud image v1 and this
# image's predecessor both used.
#
# kontur's own guest authorizes its exec keypair for root; this account is
# grain's, and -kontur-ssh-user names it. konturctl's -guest-user is what
# gets this boot's generated key authorized for it too.
useradd -m -s /bin/bash debian
echo 'debian ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-debian
chmod 0440 /etc/sudoers.d/90-debian

# The guest-side installer creates this too, but leaving it to that would
# mean an image whose only correct permissions came from a script that had
# not run yet.
install -d -m0700 -o debian -g debian /home/debian/.ssh

# --- Docker's bridge subnet, written BEFORE docker is installed.
#
# Docker's own default, 172.17.0.0/16, is not a guest-image choice -- it
# is dockerd's hardcoded first entry in its default address pool, so an
# operator host that also runs unconfigured Docker (the common case:
# scripts/setup.sh needs docker to run the deployment image and to attach
# a kontur VM's own container to it) ends up with its docker0 gateway at
# that exact address too. GRAIN_KONTUR_GIT_PROXY_HOST (scripts/setup.sh's
# ensure_kontur_git_proxy_host) defaults a VM's route to the host's git
# proxy to be that same host-side gateway address. Confirmed live: the
# moment this guest's own dockerd creates its identically-addressed local
# bridge, the guest's routing table gains a directly-connected
# 172.17.0.0/16 route that is *more specific* than its default route out
# through eth0 -- so a packet to the host's real 172.17.0.1 never leaves
# the guest at all, and lands on the guest's own unrelated docker0
# instead, refused with nothing listening there. Giving this guest's
# dockerd a different, deliberately unusual default bridge subnet up front
# removes the collision at its actual source.
#
# The ordering is the part that is new. This used to run after the docker
# install, which was harmless when nothing in the image was running: the
# file was simply on disk before anything read it. Here systemd is up, so
# docker-ce's postinst starts dockerd during the install -- with whatever
# is in /etc/docker at that moment. Written afterwards, this file would
# describe a bridge the running daemon did not have, and the collision it
# exists to prevent would be back until something restarted docker.
install -d -m0755 /etc/docker
cat > /etc/docker/daemon.json <<'DOCKERD'
{
  "bip": "172.30.255.1/24",
  "default-address-pools": [
    {"base": "172.31.0.0/16", "size": 24}
  ]
}
DOCKERD

# --- Docker, from the official repo -- identical to provision/sandbox.sh's
# own block, same reasoning (the documented path on Debian, no kernel
# config question at all).
install -m0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg \
  -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y --no-install-recommends \
  docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
usermod -aG docker debian

# Asserted rather than assumed: this is the whole reason the guest boots a
# distro kernel instead of kontur's own (overlayfs, cgroup v2, bridge
# netfilter, veth). A dockerd that cannot start is a guest that looks fine
# until the first dispatched task tries to run anything.
systemctl enable docker
systemctl start docker
docker info >/dev/null

# kind itself. The node image is deliberately not pre-pulled here, though
# for the first time it now could be -- there is a running dockerd a few
# lines up. It needs kind's default node image tag for this exact kind
# release pinned alongside KIND_VERSION, which is a version constant worth
# adding on purpose rather than in passing, and it would add most of a
# gigabyte to an image every deployment host pulls. A first
# `kind create cluster` inside a dispatched task pays that pull instead.
curl -fsSL -o /usr/local/bin/kind \
  "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
chmod +x /usr/local/bin/kind

# kind's own guidance: common defaults (8192 watches, 128 instances)
# cannot bring up a cluster, and the failures look nothing like their
# cause.
cat > /etc/sysctl.d/99-grain-kind.conf <<'SYSCTL'
fs.inotify.max_user_watches   = 524288
fs.inotify.max_user_instances = 8192
SYSCTL

# --- gcloud and terraform (bwsalmon/agents#117): a task whose deployment
# mints a per-task GCP key needs the CLI to drive it with. Nothing here
# bakes any credential into the image -- a minted key only ever arrives
# per-dispatch.
curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg | \
  gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg
echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
  > /etc/apt/sources.list.d/google-cloud-sdk.list
curl -fsSL https://apt.releases.hashicorp.com/gpg | \
  gpg --dearmor -o /usr/share/keyrings/hashicorp.gpg
echo \
  "deb [signed-by=/usr/share/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(. /etc/os-release && echo "$VERSION_CODENAME") main" \
  > /etc/apt/sources.list.d/hashicorp.list
apt-get update
apt-get install -y --no-install-recommends google-cloud-cli terraform

# --- The Go and Node toolchains, and warm caches for both.
#
# A sandbox is where the merge queue's own fix tasks run
# (orchestrator.fileFixTask), and until this block existed every one of
# them ended its commit message with some version of "not built or run:
# this sandbox has no Go toolchain and no network to fetch one". A fix
# agent that cannot run `go test ./...` is guessing, and a merge fix that
# is a guess costs another queue cycle when it turns out not to be the
# fix. What `make test` needs is exactly this: the Go the module asks
# for, the Node CI pins, and every dependency of both already on disk.
#
# The caches were written when they were the load-bearing half rather
# than an optimization: a dispatched sandbox reached nothing but the git
# proxy, so a toolchain with a cold cache could not build anything at
# all. That was a bug, not the design -- flat mode handed the guest an
# "ip=" parameter with an empty gateway field, so it booted with no
# default route, and docs/design.md's "Sandbox egress is open by default"
# had quietly stopped being true. The fix is in the runtime image rather
# than in this one (third_party/kontur/VENDORED.md, "Local patches"):
# nothing here changed, and a guest booted by a kontur image carrying
# that fix has real egress again, so a cold cache is survivable.
#
# They stay, on their own merits: an image that already holds every
# dependency does not re-fetch the same module graph on every dispatch,
# and it keeps working under an egress policy that narrows again
# (docs/design.md's egress_policy(allowlist)). They are warmed from this
# repo's own go.mod/go.sum and ui/package-lock.json, carried in as
# GRAIN_DEP_MANIFESTS, so what lands here is what the tree at build time
# actually resolves to rather than whatever is newest; a branch that adds
# a dependency the published image predates fetches that one dependency
# rather than the whole graph.
#
# Deliberately *not* here: Playwright's browsers. `npx playwright install`
# is a ~300MB download of Chromium/Firefox/WebKit per image, and the job
# it feeds (`make test-e2e`, tests.yml's ui-e2e) is a separate CI job
# from the one merge fixes actually keep tripping over. `go test ./...`
# and `npm test` are what the `go` job gates on, and they are what this
# block makes runnable.
#
# One image, uniform (README.md's own section) still holds: these caches
# are grain's because grain is what this image's sandboxes work on, the
# same way the package list above is v1's sandbox list. Nothing here is
# per-task or per-branch.

# Go, from the upstream tarball rather than Debian's golang-go: bookworm
# ships 1.19, and go.mod asks for a version years past that. GO_VERSION
# is read out of go.mod itself by build-guest.sh (the Makefile reads it
# the same way for Dockerfile.build), so the image and the module cannot
# drift apart while only one of them is ever edited.
go_tarball="go${GO_VERSION}.linux-amd64.tar.gz"
curl -fsSL -o "/tmp/${go_tarball}" "https://go.dev/dl/${go_tarball}"
rm -rf /usr/local/go
tar -C /usr/local -xzf "/tmp/${go_tarball}"
rm -f "/tmp/${go_tarball}"
# /usr/local/bin, not a profile script: sshd runs `bash -c` for a
# non-interactive command, which reads no profile at all, and its default
# PATH (/usr/local/bin:/usr/bin:/bin:/usr/games -- confirmed on a booted
# guest) already has this directory in it. Every environment default
# below is likewise written where the tool itself reads it rather than
# into a shell's environment, for the same reason.
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Node, likewise from upstream: bookworm ships 18, and vitest -- what
# `npm test` runs -- needs 20.19 or newer.
node_tarball="node-v${NODE_VERSION}-linux-x64.tar.xz"
curl -fsSL -o "/tmp/${node_tarball}" "https://nodejs.org/dist/v${NODE_VERSION}/${node_tarball}"
rm -rf /usr/local/lib/nodejs
install -d -m0755 /usr/local/lib/nodejs
tar -C /usr/local/lib/nodejs --strip-components=1 -xJf "/tmp/${node_tarball}"
rm -f "/tmp/${node_tarball}"
ln -sf /usr/local/lib/nodejs/bin/node /usr/local/bin/node
ln -sf /usr/local/lib/nodejs/bin/npx /usr/local/bin/npx

# npm is a wrapper rather than a symlink, for Playwright specifically.
# @playwright/test's own install script downloads three browsers unless
# PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD is set, and ui/'s devDependencies
# include it -- so a plain `npm ci` in ui/ (which is what `make frontend`
# runs, and so what `make test` runs) would fetch ~300MB of browsers on
# every install, for a suite (`make test-e2e`) a sandbox does not run.
# Not something a cache spares it: npm's cache holds packages, and those
# browsers are not a package. It used to fail the install outright rather
# than merely cost time, back when the guest could not reach that CDN at
# all -- see "The Go and Node toolchains" above for why it now can.
#
# The default is set with ${VAR-1} rather than ${VAR:-1} so that setting
# the variable to the empty string is a way back to the normal
# behaviour -- Playwright reads the variable's presence, not its value,
# so there is no "0" that means no.
cat > /usr/local/bin/npm <<'EOF'
#!/bin/sh
# grain's sandbox guest (scripts/kontur/guest-setup.sh) wraps npm to
# default PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD on: `make test-e2e` is not a
# suite a sandbox runs, and without this every `npm ci` in ui/ spends
# ~300MB fetching browsers for it instead of installing from npm's own
# warm cache. Run `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD= npm ...` to get the
# browsers anyway.
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD="${PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD-1}"
exec /usr/local/lib/nodejs/bin/npm "$@"
EOF
chmod 0755 /usr/local/bin/npm

# Defaults for the two tools, written where each one reads them rather
# than exported into a shell that a non-interactive SSH command never
# sources:
#
#   GOTOOLCHAIN=local -- the same pin Dockerfile.build sets, for the same
#   reason. A go.mod asking for a newer Go than this image carries would
#   otherwise have the go command fetch that toolchain at build time,
#   which here means failing on the network rather than saying so; local
#   makes it an error naming both versions, which is the signal to
#   republish this image.
#
#   prefer-offline -- npm serves from its cache without revalidating
#   against the registry, so a warm cache is enough on its own. It is
#   not `offline`, which would make a guest that *does* have a network
#   unable to install anything new.
#
#   audit/fund false -- both are registry round trips on every install
#   with nothing to say to an offline guest.
install -d -m0755 -o debian -g debian /home/debian/.config/go
GOENV=/home/debian/.config/go/env /usr/local/go/bin/go env -w GOTOOLCHAIN=local
HOME=/home/debian npm config set prefer-offline=true --location=global
HOME=/home/debian npm config set audit=false --location=global
HOME=/home/debian npm config set fund=false --location=global

# The caches themselves, warmed into the sandbox account's own default
# locations -- /home/debian/go/pkg/mod and /home/debian/.npm -- so that
# nothing has to point either tool at them, and both stay writable by the
# account that will use them (a read-only shared cache would turn "this
# module is missing" into a permission error).
#
# `go mod download` with no arguments is the whole build list for a
# go.mod at 1.17 or newer: the modules its own require directives name,
# resolved without loading a single package, which is what lets this run
# against a directory holding nothing but go.mod and go.sum. `npm ci`
# does need a node_modules to install into, so it gets one, in the same
# temporary directory that goes away afterwards -- the cache it fills on
# the way is the part that stays.
grain_manifests="$(mktemp -d)"
# set -x would otherwise echo the whole base64 blob into the build log.
set +x
printf '%s' "${GRAIN_DEP_MANIFESTS}" | base64 -d | tar -xz -C "${grain_manifests}"
set -x
(
  cd "${grain_manifests}"
  HOME=/home/debian GOMODCACHE=/home/debian/go/pkg/mod go mod download
  cd "${grain_manifests}/ui"
  HOME=/home/debian npm ci
)
rm -rf "${grain_manifests}"
# Everything above ran as root with HOME pointed at the account's home,
# so the files are in the right places under the wrong owner.
chown -R debian:debian /home/debian

# --- Grow the root filesystem onto whatever disk this VM was actually
# given (grain/task-41).
#
# `konturctl vm create -disk-size-gb` sizes the VM's own writable qcow2
# overlay, and that is all it can do: the filesystem packed into the
# backing guest image (kontur's `mke2fs -d`, sized to the rootfs plus 20%
# plus whatever GUEST_DISK_EXTRA_MB asked for) still ends where it ended,
# so a VM asked for 40 GiB boots
# with the image's few hundred megabytes and tens of gigabytes of
# unallocated space past the end of it. Nothing but the guest can close
# that gap -- the hypervisor has no idea what filesystem is on the device
# -- so this is grain's half of the disk-size setting.
#
# Online, on every boot, rather than once at build time: the size is not
# known when the image is built (it is a per-VM create-time argument, and
# a per-task one at that), and it is the same VM's first boot either way
# since a sandbox lives exactly as long as one run. On a VM whose disk was
# not enlarged -- every VM on a deployment that never sets a disk size --
# resize2fs finds nothing to do and exits 0, so this costs those a
# fraction of a second and changes nothing.
#
# Failure is deliberately not fatal: a guest that boots with its original
# filesystem is a run that may run out of disk, while a guest that fails
# to boot is a run that cannot start at all. The message lands on the
# console either way, which is where a VM's own boot is read from
# (`docker logs` on its container).
cat > /usr/local/sbin/grain-growfs <<'EOF'
#!/bin/sh
# Grows the root filesystem to fill its block device, if it does not
# already. See guest-setup.sh's own "Grow the root filesystem" block.
set -u
root_dev=$(findmnt -n -o SOURCE / 2>/dev/null || true)
case "${root_dev}" in
  /dev/*) ;;
  # An overlay or anything else that is not a plain block device has
  # nothing here to grow, and is not an error worth failing a boot over.
  *) echo "grain-growfs: root is ${root_dev:-unknown}, not a block device -- nothing to grow"; exit 0 ;;
esac
if ! resize2fs "${root_dev}"; then
  echo "grain-growfs: resize2fs ${root_dev} failed -- continuing with the filesystem as it is" >&2
fi
exit 0
EOF
chmod 0755 /usr/local/sbin/grain-growfs

cat > /etc/systemd/system/grain-growfs.service <<'EOF'
[Unit]
Description=Grow the root filesystem onto the whole virtual disk (grain sandbox)
DefaultDependencies=no
After=systemd-remount-fs.service
Before=sysinit.target
ConditionPathIsReadWrite=/

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/grain-growfs
RemainAfterExit=yes
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=sysinit.target
EOF
ln -sf /etc/systemd/system/grain-growfs.service \
  /etc/systemd/system/sysinit.target.wants/grain-growfs.service

install -d -m0755 /etc/kontur-guest
cat > /etc/kontur-guest/README <<'DOC'
This is a grain sandbox guest: a bwsalmon/kontur guest image that was
booted, provisioned by scripts/kontur/guest-setup.sh and committed. See that
directory's README.md for the pipeline.

Added on top of kontur's own guest image:
- the "debian" account (passwordless sudo, docker group) -- the account
  -kontur-ssh-user names. No key is baked in: kontur generates one per VM
  boot and authorizes it for the account konturctl's -guest-user names
- a systemd unit (grain-growfs) that grows the root filesystem onto the
  whole virtual disk on each boot, so a VM created with
  `konturctl vm create -disk-size-gb` actually has that space rather than
  just a larger empty device. A no-op on a VM whose disk was not enlarged
- git curl jq ripgrep fd-find build-essential python3 python3-venv pipx
  tmux unzip xz-utils ca-certificates bubblewrap gnupg
- docker-ce plus kind (its node image is not pre-pulled -- see
  guest-setup.sh's own comment on that)
- google-cloud-cli and terraform, for a task whose deployment mints a
  per-task GCP key at dispatch time (nothing here bakes the key itself)
- the Go toolchain go.mod asks for and the Node major CI pins, plus a
  module cache and an npm cache already holding every dependency
  go.sum/ui/package-lock.json name -- so `make test` runs without
  re-fetching the whole module graph, and still runs at all if this
  guest's egress is ever narrowed again.

Not baked in, on purpose:
- any credential -- git config/credentials are set per-dispatch
- Claude Code -- it runs on the controller side against this VM's four MCP
  sandbox tools, not on the guest itself
- Playwright's browsers (~300MB): `make test-e2e` does not run here. npm
  is wrapped to skip their download so that `npm ci` still works --
  guest-setup.sh's own "The Go and Node toolchains" explains it.
DOC

# Leaves the apt lists and the downloaded .debs out of the committed
# image. Not identity, so not something konturctl's own scrub touches --
# that only removes what would otherwise be shared between every VM
# cloned from this image -- but a few hundred MB that no dispatched task
# reads.
#
# The archives matter more than they look. apt keeps every .deb it
# installed under /var/cache/apt/archives, so without this they are
# committed into the image *and* still occupying the guest's disk when a
# task runs -- on the very filesystem an install needs free space in.
# This guest installs ~110MB of them.
apt-get clean
rm -rf /var/lib/apt/lists/*

# Best-effort: hand the blocks everything above freed back to the disk
# image, so they stay holes in it rather than zeros. It matters because
# of how this image travels: the headroom `GUEST_DISK_EXTRA_MB` asked for
# is a hole in disk.img until something writes to it, a hole costs
# nothing to push, and extracting the layer on the other side
# materializes whatever the file actually occupies. Every byte trimmed
# here is a byte off every pull.
#
# Guarded because it depends on the virtual disk supporting discard,
# which is cloud-hypervisor's decision rather than this guest's: an
# "operation not supported" is a missed optimization, not a broken image.
# The build log says which happened -- fstrim -v reports the bytes.
fstrim -v / || echo "guest-setup.sh: fstrim not supported on this disk -- the image keeps its unused blocks"
