#!/bin/sh
# Customizes a bwsalmon/kontur guest image into a grain sandbox guest.
#
# This is the script handed to kontur's own build-time guest setup hook,
# replacing scripts/kontur/provision.sh and the debootstrap pipeline
# build.sh drove it from: kontur's Dockerfile already builds a guest
# rootfs (debootstrap --variant=minbase, plus its own overlays) and packs
# it into a disk image with `mke2fs -d`, so grain no longer builds one of
# its own -- it only says what to add.
#
# CONTRACT: runs as root with the guest rootfs as /, after kontur's own
# guest stage has finished and before that rootfs is packed into
# disk.img, with a real /proc and /dev and working network access. Not a
# chroot, as an earlier version of this header assumed: kontur's
# guest-customized stage (bwsalmon/kontur#28) promotes the rootfs to an
# image of its own and runs this as an ordinary Dockerfile RUN, precisely
# so that a chroot's need for CAP_SYS_ADMIN to bind-mount /proc and /dev
# never arises. The practical guarantees are the ones provision.sh ran
# under, which is why this stayed a port rather than becoming a rewrite;
# the one difference worth knowing is that /sys may be read-only under
# BuildKit where provision.sh's chroot had it read-write. Nothing here
# writes to /sys. Neither environment has a running service manager:
# `systemctl enable` works, `systemctl start` does not.
#
# ORDERING: the kernel package must already be installed by the time this
# runs -- see "Networking" below, whose final `update-initramfs` has
# nothing to regenerate otherwise, and whose failure mode is a guest that
# boots with no address at all.
#
# Inputs, both as environment variables rather than files (nothing here is
# ever written into this repo):
#   SANDBOX_SETUP_SCRIPT     optional. An operator's own extra
#                            customization, run after everything below.
#
# Notably no SSH key: see "No SSH key is baked in" at the bottom. This
# image is generic, and every VM booted from it authorizes a different
# key that kontur generates at boot.
#
# What this deliberately does NOT do, because kontur's own guest stage
# already does it: install openssh-server/systemd-sysv/iproute2/acpid,
# wire up the ACPI power button for graceful shutdown, arrange for fresh
# SSH host keys per VM, or authorize kontur's own `kontur exec` keypair.
#
# One thing it deliberately UNDOES: kontur's ForceCommand console wrapper.
# See "Stop forcing kontur's console wrapper" below -- it is the one place
# where building on kontur's guest actively breaks grain's tools rather
# than merely not helping them.
set -eux

KIND_VERSION="v0.32.0"

export DEBIAN_FRONTEND=noninteractive
apt-get update

# The sandbox toolchain -- "whatever v1's own sandbox image already
# carries" (bwsalmon/agents#267), matching provision/sandbox.sh's list.
# sudo and libnss-myhostname are not toolchain: see the "debian" account
# and hostname blocks below for what needs each.
# klibc-utils is named explicitly rather than left to arrive as a
# transitive dependency: /usr/lib/klibc/bin/ipconfig is what the
# networking unit below actually execs, and it reaches this image only via
# linux-image-amd64 -> initramfs-tools -> klibc-utils. kontur's guest is
# debootstrap --variant=minbase, which has none of that chain, so anything
# that changes how the kernel gets in (a copied-in kernel rather than the
# Debian package, say) would silently take ipconfig with it and leave
# every guest without an address.
# e2fsprogs is named for the same reason klibc-utils is: resize2fs is what
# the grain-growfs unit below execs, and while e2fsprogs is a
# Priority: required package that debootstrap --variant=minbase does
# install today, a guest that silently lost it would boot fine and simply
# never grow onto the disk its VM was created with -- a failure that looks
# like "-disk-size-gb did nothing" several layers away from its cause.
apt-get install -y --no-install-recommends \
  linux-image-amd64 \
  sudo libnss-myhostname klibc-utils e2fsprogs \
  git curl jq ripgrep fd-find build-essential python3 python3-venv \
  pipx tmux unzip ca-certificates bubblewrap gnupg

# --- Hostname resolution. The chroot this runs in shares the build host's
# UTS namespace, so anything above that asked for "the current hostname"
# saw the build host's, not the guest's. Every guest getting the same
# fixed name is fine -- nothing here needs a unique one, and konturctl's
# addressing never relies on it -- but inheriting an arbitrary build
# host's name would be actively misleading.
echo kontur-guest > /etc/hostname

# libnss-myhostname's postinst does not itself edit this conffile (an
# existing /etc/nsswitch.conf predates it in every debootstrap run), so
# the module has to be wired into the hosts line by hand to apply.
# Without it sudo -- which resolves the local hostname for its own
# logging -- blocks for a long DNS timeout against a guest with no real
# nameserver. Confirmed by hand.
sed -i 's/^hosts:.*/hosts:          files myhostname dns/' /etc/nsswitch.conf

# --- The "debian" account. On v1's sandbox base (a stock Debian cloud
# image) this was cloud-init's default_user; this image has no cloud-init,
# so it is created here instead -- same name, so every assumption
# downstream of it keeps holding: the operator's authorized_keys below,
# grain/adapter/libvirt.py's v1 convention, and the docker group grant.
# Passwordless sudo matches what cloud-init's default_user grants on every
# cloud image v1 and this image's predecessor both used.
#
# kontur's own guest authorizes its exec keypair for root; this account is
# grain's, and -kontur-ssh-user names it.
useradd -m -s /bin/bash debian
echo 'debian ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-debian
chmod 0440 /etc/sudoers.d/90-debian

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

# Docker's own default bridge subnet, 172.17.0.0/16, is not a guest-image
# choice -- it is dockerd's hardcoded first entry in its default address
# pool, so an operator host that also runs unconfigured Docker (the common
# case: scripts/setup.sh needs docker to build/run the OCI image and
# to attach a kontur VM's own container to it) ends up with its docker0
# gateway at that exact address too. GRAIN_KONTUR_GIT_PROXY_HOST
# (scripts/setup.sh's ensure_kontur_git_proxy_host) defaults a VM's
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

# kind itself. The node image is not pre-pulled: that needs a running
# docker daemon, which a chroot cannot provide. A first
# `kind create cluster` inside a dispatched task pays that pull once
# instead of paying it here on every image build.
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

# --- Networking: kernel interface naming, and static addressing from the
# kernel's own "ip=" boot parameter.
#
# kontur's own guest image needs neither, and its deploy/guest-image
# README says so explicitly ("it relies on the kernel's built-in ip=
# boot-time autoconfiguration ... no extra guest-side networking setup was
# needed"). That holds only for a kernel with CONFIG_IP_PNP, and the
# kernel this guest actually boots -- Debian's stock linux-image-amd64,
# installed above rather than kontur's own (see README.md, "Why no custom
# kernel") -- does NOT enable it, so nothing acts on "ip=" without the
# unit below. konturctl derives that "ip=" value itself with "eth0"
# hard-coded (internal/staticpod/spec.go), so the guest has to guarantee
# that name rather than the other way around.
#
# Naming is done by turning systemd's predictable-naming policy off
# wholesale rather than by pinning a name, because this guest can have
# more than one NIC. kontur's flat networking mode gives it two: the
# first is spliced onto the container's own network segment and carries
# the identity "ip=" configures, the second is the private control link
# "kontur exec" and the memory agent reach the guest on (kontur's
# internal/netshim, "Flat mode"). A link file matching Type=ether and
# forcing Name=eth0 -- which is what this did before -- matches *both* of
# them and can only win once, leaving the other NIC under whatever name
# systemd falls back to and the control link unconfigured. Masking the
# default .link is the documented way to disable predictable naming, and
# leaves the kernel's own names in place: eth0 and eth1, in the PCI probe
# order cloud-hypervisor attaches them, which is the order kontur passes
# --net (spliced NIC first, control link second -- netshim's
# FlatGuestConfig). A single-NIC guest, i.e. kontur's original NAT mode,
# still gets exactly eth0 out of this.
#
# Both were found by hand against a real booted guest. Their failure mode
# is a VM that boots fine and has no address at all, which reaches the
# daemon only as "the guest never became reachable".
ln -sf /dev/null /etc/systemd/network/99-default.link

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

# --- Grow the root filesystem onto whatever disk this VM was actually
# given (grain/task-41).
#
# `konturctl vm create -disk-size-gb` sizes the VM's own writable qcow2
# overlay, and that is all it can do: the filesystem packed into the
# backing guest image (kontur's `mke2fs -d`, sized to the rootfs plus 20%
# headroom) still ends where it ended, so a VM asked for 40 GiB boots
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

# --- Stop forcing kontur's console wrapper on every SSH session.
#
# kontur's own guest overlay ships
# /etc/ssh/sshd_config.d/10-console.conf with an unconditional
# `ForceCommand /usr/local/libexec/kontur-ssh-console-wrap`, and that
# wrapper runs the session's real command under `script`, mirroring its
# output to the serial console so SSH activity shows up in the container's
# own logs. That is a good property for kontur's reference guest. It is
# incompatible with grain's sandbox tools, because `script` runs the
# command under a *pty*, and a pty is not a transparent pipe.
#
# Measured against the real wrapper rather than reasoned about:
#
#   - Every "\n" on output becomes "\r\n" (the pty's ONLCR). read_file
#     (`cat -- path`) would hand back every file with CRLF line endings it
#     does not have on disk, and a write_file/read_file round trip would
#     no longer agree with itself.
#   - stdout and stderr are merged onto the one pty. run_command reports
#     the two separately, and sshReadRemote/sshWriteRemote report a failed
#     `cat`/`dd` by its stderr -- which would arrive empty, giving errors
#     with no message.
#
# (Exit status and stdin both survive intact: `script --return` propagates
# the command's status, and input passes through byte-for-byte. Only the
# two above break.)
#
# So the drop-in is replaced rather than removed: its two hardening lines
# are worth keeping, and only the ForceCommand has to go. The wrapper
# script itself is left in place, unreferenced, for anyone who wants to
# invoke it deliberately.
cat > /etc/ssh/sshd_config.d/10-console.conf <<'EOF'
# grain (scripts/kontur/guest-setup.sh) replaced kontur's own version of
# this file. The hardening below is kept verbatim; the ForceCommand that
# mirrored every session to the serial console is deliberately not, since
# it runs each command under a pty -- which merges stdout with stderr and
# rewrites every newline as CRLF, both of which grain's sandbox tools
# depend on not happening. See guest-setup.sh for the measurements.
PermitRootLogin prohibit-password
PasswordAuthentication no
EOF

# --- Optional operator-supplied customization, run once everything above
# has finished, so a custom script can rely on all of it being in place.
if [ -n "${SANDBOX_SETUP_SCRIPT:-}" ]; then
  script="$(mktemp)"
  printf '%s\n' "${SANDBOX_SETUP_SCRIPT}" > "${script}"
  chmod 0755 "${script}"
  "${script}"
  rm -f "${script}"
fi

# --- No SSH key is baked in, and that is the point.
#
# This used to install OPERATOR_SSH_PUBLIC_KEY -- the public half of a
# keypair scripts/setup.sh generated per deployment -- as the debian
# account's only authorized_keys entry. That is what made a guest image
# deployment-specific, and so what stopped a published one from existing:
# a generic disk would have carried either a private key everybody has or
# no way in at all.
#
# kontur now generates a keypair in the VM's own container on every boot
# and hands the guest the public half on the kernel command line, which
# kontur-authorized-key installs before sshd starts (third_party/kontur's
# internal/guestkey). The account it installs for is named by
# `konturctl vm create -guest-user`, which setup.sh passes as "debian" --
# the account created above. So nothing here has to know a key at all,
# and every VM gets a different one that exists only while it runs.
#
# The .ssh directory is still created here: the guest-side installer
# creates it too, but leaving it to that would mean an image whose only
# correct permissions came from a script that had not run yet.
install -d -m0700 -o debian -g debian /home/debian/.ssh

# initramfs-tools' hooks bake a snapshot of /etc/udev's rules and
# /etc/modules-load.d into the initramfs when update-initramfs runs. The
# kernel package's own postinst already triggered at least one such run,
# from before the eth0/ip= units above existed, so it has to be
# regenerated now that they do: confirmed by hand, the guest's NIC still
# got renamed away from "eth0" by the initramfs' stale udev snapshot until
# this final regeneration was added.
update-initramfs -u -k all

mkdir -p /etc/kontur-guest
cat > /etc/kontur-guest/README <<'DOC'
This is a grain sandbox guest: a bwsalmon/kontur guest image plus the
customization scripts/kontur/guest-setup.sh applies at build time. See that
directory's README.md for the pipeline.

Added on top of kontur's own guest image:
- the "debian" account (passwordless sudo, docker group), with the
  operator's public key as its only authorized_keys entry and no password
  login -- this is the account -kontur-ssh-user names
- a systemd unit that statically addresses eth0 from the kernel's own
  "ip=" boot parameter, and predictable interface naming disabled so the
  kernel's own eth0/eth1 names survive, neither of which kontur's own
  guest needs (see guest-setup.sh's "Networking"). kontur's flat-mode
  control link (its own kontur-control-net service, from kontur's guest
  overlay) configures eth1 on top of that
- a systemd unit (grain-growfs) that grows the root filesystem onto the
  whole virtual disk on each boot, so a VM created with
  `konturctl vm create -disk-size-gb` actually has that space rather than
  just a larger empty device. A no-op on a VM whose disk was not enlarged
- git curl jq ripgrep fd-find build-essential python3 python3-venv pipx
  tmux unzip ca-certificates bubblewrap gnupg
- docker-ce plus kind (its node image is not pre-pulled -- see
  guest-setup.sh's own comment on that)
- google-cloud-cli and terraform, for a task whose deployment mints a
  per-task GCP key at dispatch time (nothing here bakes the key itself)

Not baked in, on purpose:
- any credential -- git config/credentials are set per-dispatch
- Claude Code -- it runs on the controller side against this VM's four MCP
  sandbox tools, not on the guest itself
DOC
