#!/bin/bash
# Baked into packer/kontur/image.pkr.hcl's output qcow2 by Packer's shell
# provisioner, which runs this once, as root, against a fresh boot of the
# stock Debian base image -- see README.md in this directory for the whole
# pipeline this script is one piece of.
#
# This is the kontur-guest counterpart to provision/sandbox.sh, which does
# the equivalent job for v1's libvirt-managed sandboxes: same package list,
# same reasoning (bwsalmon/agents#267's own text: "whatever v1's own sandbox
# image already carries"), same "no secret is ever baked into an image"
# rule provision/controller.sh's header states for the controller. The
# difference is delivery, not content -- v1 hands this script to cloud-init
# fresh on every VM's first boot, against a shared base image; a kontur VM
# has no equivalent per-VM provisioning hook (pkg/kontur's own doc comment:
# kontur has no apiserver, and nothing here found an analogous NoCloud-style
# seed kontur itself feeds a guest), so this script instead runs once, here,
# at image-build time, and every VM kontur creates from the resulting image
# boots with everything below already in place.
set -eux

KIND_VERSION="v0.32.0"
KIND_NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"

apt-get update
apt-get install -y --no-install-recommends \
  openssh-server git curl jq ripgrep fd-find build-essential python3 python3-venv \
  pipx tmux unzip ca-certificates bubblewrap gnupg

# Docker, from the official repo -- identical to provision/sandbox.sh's own
# block, same reasoning (the documented path on Debian, no kernel-config
# question at all).
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

# The sandbox SSH user (debian, matching v1's own sandbox convention --
# grain/adapter/libvirt.py embeds the operator key into this same account)
# needs to run docker without sudo.
usermod -aG docker debian

# kind itself.
curl -fsSL -o /usr/local/bin/kind \
  "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
chmod +x /usr/local/bin/kind

# kind's own guidance: common defaults (8192 watches, 128 instances) cannot
# bring up a cluster, and the failures look nothing like their cause.
cat > /etc/sysctl.d/99-grain-kind.conf <<'SYSCTL'
fs.inotify.max_user_watches   = 524288
fs.inotify.max_user_instances = 8192
SYSCTL
sysctl --system

# Pre-load the kind node image into the base: it is on the order of a
# gigabyte, and there is no per-task rebuild step here to reload it into
# (unlike v1's recreate, a kontur VM's own lifecycle is between this repo
# and bwsalmon/kontur, not grain -- see README.md).
systemctl start docker
docker pull "${KIND_NODE_IMAGE}"
systemctl stop docker

# gcloud and terraform -- identical reasoning to provision/sandbox.sh's own
# block (bwsalmon/agents#117): a task whose deployment mints a per-task GCP
# key needs the CLI to drive it with, and nothing here bakes any credential
# into the image itself -- a minted key only ever arrives per-dispatch.
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

# --- Optional operator-supplied customization, run once the built-in
# provisioning above has finished but before the security-critical
# finalization below (operator key, cloud-init disable) -- so a custom
# script can rely on everything above already being in place, and can't
# itself interfere with either finalization step by, say, leaving its own
# stray authorized_keys entry or re-enabling cloud-init. SANDBOX_SETUP_
# SCRIPT arrives as a build-time-only environment variable the same way
# OPERATOR_SSH_PUBLIC_KEY does below (Packer's shell provisioner,
# image.pkr.hcl's environment_vars) -- holding the script's own contents
# rather than a path, the same idiom bwsalmon/kontur's own GUEST_SETUP_
# SCRIPT build arg uses for its Dockerfile-based guest image build
# (third_party/kontur/deploy/guest-image/README.md, "Running a custom
# setup script"). Unlike that one, this runs against a live booted VM
# over SSH -- this whole script's own delivery mechanism -- rather than a
# chroot, so it has a running service manager, /proc, /sys, and network
# access all as themselves, no different from provision.sh's own
# apt-get/curl calls above; there is nothing analogous to that feature's
# "no /proc, /sys, or running service manager" caveat here.
if [ -n "${SANDBOX_SETUP_SCRIPT:-}" ]; then
  script="$(mktemp)"
  printf '%s\n' "${SANDBOX_SETUP_SCRIPT}" > "${script}"
  chmod 0755 "${script}"
  "${script}"
  rm -f "${script}"
fi

# --- sshd: enabled and running, matching the assumption pkg/kontur's own
# package doc comment and v2/README.md both state a kontur guest image
# has to satisfy on its own, since nothing analogous to
# LibvirtAdapter.render_domain_xml wires SSH access up per-VM the way v1
# does. -----------------------------------------------------------------
systemctl enable ssh

# --- The operator's SSH key, baked in rather than injected. OPERATOR_SSH_
# PUBLIC_KEY arrives as a build-time-only environment variable (Packer's
# shell provisioner, image.pkr.hcl's environment_vars) -- never written to
# this repo, never a secret (it is the public half; see the private half's
# own handling, e.g. provision/controller.sh's controller-ssh key, for what
# actually gates access). This overwrites, not appends: whatever
# provisioner-only key the build's own NoCloud seed (build.sh) used to reach
# this VM in the first place must not survive into the shipped image.
[ -n "${OPERATOR_SSH_PUBLIC_KEY:-}" ] || {
  echo "provision.sh: OPERATOR_SSH_PUBLIC_KEY is empty -- refusing to ship an image no one can reach" >&2
  exit 1
}
install -d -m0700 -o debian -g debian /home/debian/.ssh
printf '%s\n' "${OPERATOR_SSH_PUBLIC_KEY}" > /home/debian/.ssh/authorized_keys
chmod 0600 /home/debian/.ssh/authorized_keys
chown debian:debian /home/debian/.ssh/authorized_keys

# --- cloud-init: disabled, not left running. It did its one job -- getting
# the build-time seed's temporary key onto this VM so Packer could reach it
# at all (build.sh) -- and has no further job once the image ships: kontur
# manages a VM's lifecycle itself (static pod manifests under a standalone
# kubelet, per pkg/kontur's doc comment), not via a cloud provider's
# metadata service, and nothing found in this repo or in bwsalmon/kontur's
# own referenced docs (deploy/static-kubelet/README.md) describes a NoCloud
# datasource a kontur guest would see. Left enabled, cloud-init would at
# best no-op against a datasource that never appears and at worst try to
# reconfigure networking a kontur/CNI guest doesn't own the way a NoCloud
# guest normally would. `clean` also removes the build's own seed's cached
# instance-id, so if a datasource ever does appear in some future
# deployment shape, cloud-init treats it as a first boot rather than as a
# rerun of this one.
systemctl disable cloud-init cloud-init-local cloud-config cloud-final 2>/dev/null || true
cloud-init clean --logs --seed

mkdir -p /etc/kontur-guest
cat > /etc/kontur-guest/README <<'DOC'
This is a kontur guest image, built by packer/kontur/ in bwsalmon/grain.
See that directory's README.md for the full pipeline.

Baked in at image-build time (packer/kontur/provision.sh):
- openssh-server, enabled -- the operator's public key is the only
  authorized_keys entry for the debian user, and there is no password
  login
- git curl jq ripgrep fd-find build-essential python3 python3-venv pipx
  tmux unzip ca-certificates bubblewrap gnupg
- docker-ce (debian in the docker group) plus kind, with kindest/node's
  image pre-pulled
- google-cloud-cli and terraform, for a task whose deployment mints a
  per-task GCP key at dispatch time (nothing here bakes the key itself)

Not baked in, on purpose:
- any credential -- git config/credentials are set per-dispatch, the same
  way v1's configure_git_credentials and v2's mcp.ConfigureGitCredentials
  both do it against a real sandbox
- Claude Code -- it runs on the controller/orchestrator side against this
  VM's four MCP sandbox tools over SSH, not on the guest itself
  (docs/roadmap.md item 8's "Update" explains why, for v1; the same
  reasoning applies here)
DOC
