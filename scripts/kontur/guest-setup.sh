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
set -eux

KIND_VERSION="v0.32.0"

export DEBIAN_FRONTEND=noninteractive
apt-get update

# The sandbox toolchain -- "whatever v1's own sandbox image already
# carries" (bwsalmon/agents#267), matching provision/sandbox.sh's list.
# sudo and libnss-myhostname are not toolchain: see the "debian" account
# and hostname blocks below for what needs each.
apt-get install -y --no-install-recommends \
  sudo libnss-myhostname \
  git curl jq ripgrep fd-find build-essential python3 python3-venv \
  pipx tmux unzip ca-certificates bubblewrap gnupg

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

# Leaves the apt lists out of the committed image. Not identity, so not
# something konturctl's own scrub touches -- it only removes what would
# otherwise be shared between every VM cloned from this image -- but a few
# hundred MB that no dispatched task reads.
rm -rf /var/lib/apt/lists/*
