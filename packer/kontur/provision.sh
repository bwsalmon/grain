#!/bin/sh
# Populates a Debian rootfs with everything a kontur guest needs, run by
# build.sh via chroot against a fresh debootstrap tree -- see README.md in
# this directory for the whole pipeline this script is one piece of, and
# for why chroot (not a booted VM, unlike this directory's previous
# Packer-based build) is enough to do all of it.
#
# This is the kontur-guest counterpart to provision/sandbox.sh, which does
# the equivalent job for v1's libvirt-managed sandboxes: same package list,
# same reasoning (bwsalmon/agents#267's own text: "whatever v1's own
# sandbox image already carries"), same "no secret is ever baked into an
# image" rule provision/controller.sh's header states for the controller.
set -eux

KIND_VERSION="v0.32.0"

export DEBIAN_FRONTEND=noninteractive

apt-get update
# linux-image-amd64 is the guest kernel: cloud-hypervisor direct-kernel-
# boots it (CHV_KERNEL/CHV_INITRAMFS) rather than going through a
# bootloader/firmware -- see README.md, "Why no custom kernel" for why
# Debian's own stock kernel already has everything that needs (PVH entry,
# virtio-pci/virtio-blk/virtio-net) with nothing built from source.
apt-get install -y --no-install-recommends \
  linux-image-amd64 openssh-server systemd-sysv iproute2 acpid sudo \
  libnss-myhostname \
  git curl jq ripgrep fd-find build-essential python3 python3-venv \
  pipx tmux unzip ca-certificates bubblewrap gnupg

# Chroot shares this build host's own UTS namespace, so anything run
# above that queries "the current hostname" (some package's postinst,
# confirmed by hand: /etc/hostname ended up holding this build host's own
# name, not anything meaningful to a booted guest) sees this build host's
# name, not the guest's own. Every guest getting the exact same fixed
# name is not a real problem here -- nothing in this image needs a unique
# hostname, and konturctl's own addressing (see the "ip=" handling below)
# never relies on it -- but inheriting an arbitrary build host's name
# would be actively misleading. libnss-myhostname above (whatever the
# name ends up being) is what actually keeps a local lookup -- notably
# sudo's, which otherwise blocks for a long timeout trying to resolve a
# name no DNS server has ever heard of -- from ever needing a DNS
# round-trip in the first place.
echo kontur-guest > /etc/hostname

# libnss-myhostname's postinst does not itself edit this conffile (an
# existing /etc/nsswitch.conf, from base-files, predates it in every
# debootstrap run), so the "myhostname" module it just installed has to
# be wired into the hosts line by hand to actually apply -- confirmed by
# hand: without this, sudo (which resolves the local hostname as part of
# its own logging) blocks for a long DNS timeout against a guest with no
# real nameserver, since nothing here otherwise resolves it locally.
sed -i 's/^hosts:.*/hosts:          files myhostname dns/' /etc/nsswitch.conf

# The "debian" account itself: on v1's own sandbox base (a stock Debian
# cloud image), this is cloud-init's default_user, created before
# provision/sandbox.sh ever runs against it (that script's own comment:
# "The default cloud-init user"). This image has no cloud-init and no
# cloud image underneath it, so the account has to be created here
# instead -- same name, for every assumption downstream of it (the
# operator's authorized_keys below, grain/adapter/libvirt.py's v1
# convention, this script's own docker group grant) to keep holding.
# Passwordless sudo matches what cloud-init's own default_user grants on
# every cloud image v1 and this image's predecessor both used.
useradd -m -s /bin/bash debian
echo 'debian ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-debian
chmod 0440 /etc/sudoers.d/90-debian

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

# Docker's own default bridge subnet, 172.17.0.0/16, is not a guest-image
# choice -- it is dockerd's hardcoded first entry in its default address
# pool, so an operator host that also runs unconfigured Docker (the common
# case: v2/scripts/setup.sh needs docker to build/run the OCI image and
# to attach a kontur VM's own container to it) ends up with its docker0
# gateway at that exact address too. GRAIN_KONTUR_GIT_PROXY_HOST
# (v2/scripts/setup.sh's ensure_kontur_git_proxy_host) defaults a VM's
# route to the host's git proxy to be that same host-side gateway
# address. Confirmed live: the moment this guest's own dockerd creates
# its identically-addressed local bridge, the guest's routing table gains
# a directly-connected 172.17.0.0/16 route that is *more specific* than
# its default route out through eth0 -- so a packet to the host's real
# 172.17.0.1 never leaves the guest at all, and lands on the guest's own
# unrelated docker0 instead, refused with nothing listening there. Giving
# this guest's dockerd a different, deliberately unusual default bridge
# subnet up front removes the collision at its actual source, without
# touching the host-side networking or the address-selection logic at
# all.
install -d -m0755 /etc/docker
cat > /etc/docker/daemon.json <<'DOCKERD'
{
  "bip": "172.30.255.1/24",
  "default-address-pools": [
    {"base": "172.31.0.0/16", "size": 24}
  ]
}
DOCKERD

# kind itself. Unlike this directory's previous Packer-based build, the
# node image is not pre-pulled here: that needs a running docker daemon,
# which a chroot -- deliberately not a booted VM, see README.md -- cannot
# provide. A first `kind create cluster` inside a dispatched task now pays
# that pull once itself instead of paying it here on every image build.
curl -fsSL -o /usr/local/bin/kind \
  "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
chmod +x /usr/local/bin/kind

# kind's own guidance: common defaults (8192 watches, 128 instances) cannot
# bring up a cluster, and the failures look nothing like their cause.
cat > /etc/sysctl.d/99-grain-kind.conf <<'SYSCTL'
fs.inotify.max_user_watches   = 524288
fs.inotify.max_user_instances = 8192
SYSCTL

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

# --- Static "eth0" naming, and static addressing from the kernel's own
# "ip=" boot parameter -- see README.md, "Networking", for why both of
# these are needed at all (systemd's predictable-naming policy renames the
# guest's one NIC away from "eth0" otherwise, and Debian's stock kernel
# does not enable CONFIG_IP_PNP, so nothing acts on "ip=" without this).
# konturctl derives that "ip=" value itself (kontur's own
# internal/staticpod/spec.go) hard-coding "eth0" as the device name, so
# the guest has to guarantee that name rather than the other way around.
cat > /etc/systemd/network/00-eth0.link <<'EOF'
[Match]
Type=ether

[Link]
NamePolicy=
Name=eth0
EOF

cat > /usr/local/sbin/kontur-configure-net <<'EOF'
#!/bin/sh
# Configures the guest's network interface from the kernel's own "ip="
# boot parameter. klibc's ipconfig(8) (from klibc-utils, pulled in by
# initramfs-tools) implements the same static-addressing syntax the
# kernel's own in-kernel IP-config code would if CONFIG_IP_PNP were
# enabled, but (unlike that in-kernel code) does not read /proc/cmdline
# itself -- it only accepts the spec as an explicit argument.
set -e
ipparam=$(sed -n 's/.*\bip=\([^ ]*\).*/\1/p' /proc/cmdline)
[ -n "$ipparam" ] || exit 0
exec /usr/lib/klibc/bin/ipconfig "$ipparam"
EOF
chmod 0755 /usr/local/sbin/kontur-configure-net

cat > /etc/systemd/system/kontur-net-cmdline.service <<'EOF'
[Unit]
Description=Configure networking from the ip= kernel command line (kontur static addressing)
DefaultDependencies=no
After=systemd-udevd.service systemd-udev-trigger.service
Before=network-pre.target sshd.service ssh.service
Wants=systemd-udev-trigger.service network-pre.target

[Service]
Type=oneshot
ExecStartPre=/sbin/modprobe -v virtio_net
ExecStartPre=/bin/udevadm settle --timeout=10
ExecStart=/usr/local/sbin/kontur-configure-net
RemainAfterExit=yes

[Install]
WantedBy=sysinit.target
EOF
mkdir -p /etc/systemd/system/sysinit.target.wants
ln -sf /etc/systemd/system/kontur-net-cmdline.service \
  /etc/systemd/system/sysinit.target.wants/kontur-net-cmdline.service

# --- Optional operator-supplied customization, run once the built-in
# provisioning above has finished but before the security-critical
# finalization below (operator key) -- so a custom script can rely on
# everything above already being in place, and can't itself interfere
# with that finalization by, say, leaving its own stray authorized_keys
# entry. SANDBOX_SETUP_SCRIPT arrives as an environment variable (set by
# build.sh from its own caller's environment) holding the script's own
# contents rather than a path -- the same idiom bwsalmon/kontur's own
# GUEST_SETUP_SCRIPT build arg uses for its Dockerfile-based guest image
# build (third_party/kontur/deploy/guest-image/README.md, "Running a
# custom setup script"). This runs in the same chroot -- with a real
# /proc, /sys and /dev bind-mounted by build.sh, and network access, since
# chroot does not create a new mount or network namespace -- as every
# other step in this script: enabling a unit the normal way (systemctl
# enable) works, apt-get works, and so on. That is not a point of
# difference from the Dockerfile build any more: kontur's guest-customized
# stage runs its script as an ordinary RUN rather than under chroot, so it
# has the same real /proc, /dev and network. Neither has a *running*
# service manager, so systemctl start works in neither.
if [ -n "${SANDBOX_SETUP_SCRIPT:-}" ]; then
  script="$(mktemp)"
  printf '%s\n' "${SANDBOX_SETUP_SCRIPT}" > "${script}"
  chmod 0755 "${script}"
  "${script}"
  rm -f "${script}"
fi

# --- sshd: enabled, matching the assumption pkg/kontur's own package doc
# comment and v2/README.md both state a kontur guest image has to satisfy
# on its own, since nothing analogous to LibvirtAdapter.render_domain_xml
# wires SSH access up per-VM the way v1 does. --------------------------
systemctl enable ssh

# --- The operator's SSH key, baked in rather than injected.
# OPERATOR_SSH_PUBLIC_KEY arrives as an environment variable (set by
# build.sh) -- never written to this repo, never a secret (it is the
# public half; see the private half's own handling, e.g.
# provision/controller.sh's controller-ssh key, for what actually gates
# access).
[ -n "${OPERATOR_SSH_PUBLIC_KEY:-}" ] || {
  echo "provision.sh: OPERATOR_SSH_PUBLIC_KEY is empty -- refusing to ship an image no one can reach" >&2
  exit 1
}
install -d -m0700 -o debian -g debian /home/debian/.ssh
printf '%s\n' "${OPERATOR_SSH_PUBLIC_KEY}" > /home/debian/.ssh/authorized_keys
chmod 0600 /home/debian/.ssh/authorized_keys
chown debian:debian /home/debian/.ssh/authorized_keys

# initramfs-tools' own hooks bake a snapshot of /etc/udev's rules and
# /etc/modules-load.d into the initramfs at the time update-initramfs
# runs -- everything above (the kernel package's own postinst included)
# already triggered at least one such run, from before the eth0/ip=
# units above existed, so it has to be regenerated now that they do:
# confirmed by hand while writing this script, the guest's NIC still got
# renamed away from "eth0" by the initramfs' own stale udev snapshot
# until this final regeneration was added.
update-initramfs -u -k all

mkdir -p /etc/kontur-guest
cat > /etc/kontur-guest/README <<'DOC'
This is a kontur guest image, built by packer/kontur/ in bwsalmon/grain.
See that directory's README.md for the full pipeline.

Baked in at image-build time (packer/kontur/provision.sh):
- linux-image-amd64 (direct-kernel-booted by cloud-hypervisor; see
  README.md, "Why no custom kernel"), openssh-server (enabled), the
  operator's public key as the only authorized_keys entry for the debian
  user (no password login), and a systemd unit that statically addresses
  eth0 from the kernel's own "ip=" boot parameter (see README.md,
  "Networking")
- git curl jq ripgrep fd-find build-essential python3 python3-venv pipx
  tmux unzip ca-certificates bubblewrap gnupg
- docker-ce (debian in the docker group) plus kind (its node image is not
  pre-pulled -- see provision.sh's own comment on that)
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
