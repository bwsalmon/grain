# syntax=docker/dockerfile:1

# Builds the kontur OCI image: a single Go binary that either boots a
# cloud-hypervisor VM as PID 1 of a container ("run", the default) or sets
# up pod-local networking for a group of such VMs as an init container
# ("netshim"), plus the cloud-hypervisor binary itself and a minimal guest
# disk image *and* kernel, both usable out of the box -- the image is
# self-contained: nothing needs to be supplied from outside it (a
# node-local cache, a hostPath, an init container) to boot a VM, in
# either Docker or Kubernetes. See README.md and
# deploy/guest-image/README.md.

ARG GO_VERSION=1.22
ARG CLOUD_HYPERVISOR_VERSION=v53.0
# KONTUR_KERNEL_VERSION pins the guest kernel baked into the final kontur
# image (see the "fetch-kernel" stage below) -- a release of
# cloud-hypervisor/linux, the same known-good, PVH-entry, virtio-pci/
# virtio-blk/virtio-net-enabled kernel already used to validate direct
# kernel boot elsewhere in this repo (see the top-level README's
# "Validated on a real VM with nested virtualization" and
# deploy/k8s/gke-pod-example.yaml's now-removed fetch-kernel init
# container), just baked in at build time instead of fetched at pod
# start.
ARG KONTUR_KERNEL_VERSION=ch-release-v6.16.9-20260508
# GUEST_DISTRO selects the guest disk image baked into the final kontur
# image (not the kontur image's own base, which is always Alpine -- see
# the "final" stage below): "debian" (the default, via debootstrap) or
# "alpine" (via apk). Both produce a minimal rootfs with sshd and acpid;
# see deploy/guest-image/README.md for what else differs between them.
ARG GUEST_DISTRO=debian
# GUEST_SUITE only applies when GUEST_DISTRO=debian.
ARG GUEST_SUITE=bookworm
# GUEST_ALPINE_VERSION only applies when GUEST_DISTRO=alpine.
ARG GUEST_ALPINE_VERSION=3.20
# GUEST_SSH_AUTHORIZED_KEY is an SSH *public* key (see
# deploy/guest-image/README.md), not a secret, so baking it into the image
# history via a plain ARG is fine.
ARG GUEST_SSH_AUTHORIZED_KEY=""

# GUEST_SETUP_SCRIPT, if set, is a shell script run inside the guest rootfs
# at build time -- the build-time counterpart to CHV_SETUP_SCRIPT, for
# customization that belongs in the image rather than in every boot of it.
# It holds the script's own text, not a path, the same way
# GUEST_SSH_AUTHORIZED_KEY holds a key rather than a filename.
#
# deploy/guest-image/README.md's "Running a custom setup script" points at
# CHV_SETUP_SCRIPT instead, on the grounds that a build-time chroot has no
# running kernel or service manager to work with. That reasoning does not
# apply here: see the "guest-customized" stage below, which runs this as an
# ordinary container rather than under chroot. What CHV_SETUP_SCRIPT is
# still the right answer for is anything that has to observe the guest
# actually running; what this is for is installing packages and dropping in
# files, paid once at build time by everyone who pulls the image rather
# than once per host at first boot.
#
# Anything it writes lands in the guest disk image, so do not use it to
# place secrets -- it is as public as the image is.
ARG GUEST_SETUP_SCRIPT=""

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kontur ./cmd/kontur
# kontur-mem-agent is the guest-side half of automatic memory hotplug
# (see internal/memagent and cmd/kontur-mem-agent's own doc comment):
# unlike kontur itself, this one ships *inside* the guest disk image
# (both guest-rootfs-debian and guest-rootfs-alpine below install it),
# not the outer scratch image, so it's built as its own small static
# binary rather than folded into kontur's mode dispatch.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kontur-mem-agent ./cmd/kontur-mem-agent
# Symlinks (not copies, so the final stage's own /usr/local/bin/kontur is
# the only copy of the binary that ships) standing in for /bin/sh,
# /bin/bash and their /usr/bin equivalents -- see cmd/kontur's own doc
# comment and internal/guestexec's ShellCommandLine for why: kontur
# itself tells these two apart from its four real modes by argv[0], so
# all that's needed image-side is for the names a shell would normally
# live at to resolve to it.
RUN mkdir -p /out/shims/bin /out/shims/usr/bin \
    && for shim in /out/shims/bin/sh /out/shims/bin/bash /out/shims/usr/bin/sh /out/shims/usr/bin/bash; do \
         ln -s /usr/local/bin/kontur "${shim}"; \
       done

# cloud-hypervisor publishes fully static release binaries but no
# published checksum file, so the expected sha256 for each architecture
# is pinned here and verified at build time.
FROM debian:bookworm-slim AS fetch-chv
ARG CLOUD_HYPERVISOR_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) asset="cloud-hypervisor-static"; \
             sha256="448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc" ;; \
      arm64) asset="cloud-hypervisor-static-aarch64"; \
             sha256="f192b510eea1c710cbc439d716bb0573c223fc463dbe3e6523788a2b7ef62850" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o cloud-hypervisor \
      "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CLOUD_HYPERVISOR_VERSION}/${asset}"; \
    echo "${sha256}  cloud-hypervisor" | sha256sum -c -; \
    chmod +x cloud-hypervisor

# The guest kernel baked into the final image at
# /var/lib/kontur/guest/vmlinux (see the "final" stage below), so a
# direct-kernel-boot VM works out of the box the same way the bundled
# guest disk image already does -- see internal/config's defaultKernel.
# Same trust model as fetch-chv above: cloud-hypervisor/linux publishes
# release binaries but no published checksum file, so the expected
# sha256 for each architecture is pinned here and verified at build
# time. amd64 gets the ELF vmlinux build (PVH entry point, what
# cloud-hypervisor's x86_64 --kernel wants); arm64 gets the raw Image
# build (cloud-hypervisor's aarch64 --kernel wants a PE-format Image,
# not vmlinux) -- both are renamed to the same "vmlinux" output name so
# the final stage's COPY, and CHV_KERNEL's default, don't need to know
# which architecture produced it.
FROM debian:bookworm-slim AS fetch-kernel
ARG KONTUR_KERNEL_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) asset="vmlinux-x86_64"; \
             sha256="9d3570b47d5abb069ca00edfbfcef4c68306a9c3d078a01f10082b258f1001b8" ;; \
      arm64) asset="Image-arm64"; \
             sha256="69d1b1235381ec50f1b45cf771a7dff4a9013d452833ab34682d6283e2114010" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o vmlinux \
      "https://github.com/cloud-hypervisor/linux/releases/download/${KONTUR_KERNEL_VERSION}/${asset}"; \
    echo "${sha256}  vmlinux" | sha256sum -c -

# Builds the Debian variant of the guest rootfs baked into the final
# kontur image: a minimal Debian rootfs with sshd, via `debootstrap`. See
# the "alpine" variant below and deploy/guest-image/README.md for how the
# two compare.
FROM debian:bookworm-slim AS guest-rootfs-debian
ARG GUEST_SUITE
ARG GUEST_SSH_AUTHORIZED_KEY
RUN apt-get update && apt-get install -y --no-install-recommends \
      debootstrap ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN debootstrap --variant=minbase \
      --include=openssh-server,systemd-sysv,iproute2,acpid,udev \
      "${GUEST_SUITE}" /rootfs http://deb.debian.org/debian

COPY deploy/guest-image/overlay-common/ /rootfs/
COPY deploy/guest-image/overlay-debian/ /rootfs/
COPY --from=build --chmod=0755 /out/kontur-mem-agent /rootfs/usr/local/bin/kontur-mem-agent

# - The console wrapper and ACPI power-button handler need their
#   executable bit set (COPY doesn't preserve it from build context
#   permissions reliably across git).
# - kontur-ssh-host-keys.service is enabled by hand (a symlink into
#   multi-user.target.wants) rather than via `systemctl enable`, since
#   systemd isn't running in this chroot to do it for us. acpid's own
#   postinst already enables acpid.socket/acpid.path the same way (via
#   deb-systemd-helper, which just writes symlinks and doesn't need a
#   running systemd), so no equivalent hand-enabling is needed for it --
#   only the event handler itself (overlay-common/etc/acpi/) is ours to
#   add.
# - openssh-server's postinst already generated host keys during
#   debootstrap; they're removed so every VM booted from this image
#   doesn't share the same ones -- kontur-ssh-host-keys.service
#   regenerates them on first boot instead.
# - No key is authorized at build time. "kontur exec" gets in with a
#   keypair "kontur run" generates for each boot and passes to the guest
#   on the kernel command line, which kontur-authorized-key.service
#   installs before sshd starts (see internal/guestkey) -- so this image
#   carries no secret and can be published. GUEST_SSH_AUTHORIZED_KEY, if
#   given, is still baked in for an operator's own interactive access.
#   Root has no password at all either way (see
#   overlay-common/etc/ssh/sshd_config.d).
# - systemd-udevd's own /lib/udev/rules.d/80-net-setup-link.rules renames
#   NICs to "predictable" names (e.g. virtio-pci's second NIC becomes
#   "ens3", not "eth1") based on each device's PCI slot -- masked here by
#   symlinking the same filename under /etc/udev/rules.d (which
#   overrides it) to /dev/null, so every NIC keeps the kernel's own
#   enumeration order (eth0, eth1, ...) instead. Both the "ip=" kernel
#   parameter flat mode's first NIC boots with (see
#   internal/netshim.FlatGuestConfig) and kontur-control-net's own
#   default second-NIC name assume that naming; without this, the
#   control link silently never comes up, since udev has already renamed
#   "eth1" to "ens3" by the time kontur-control-net.service runs and
#   checks for it.
RUN set -eux; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-ssh-console-wrap; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-control-net; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-authorized-key; \
    for t in sed base64 awk mkdir chmod chown; do \
      [ -e "/rootfs/bin/$t" ] || [ -e "/rootfs/usr/bin/$t" ] || \
        { echo "kontur-authorized-key execs $t, which this rootfs does not have" >&2; exit 1; }; \
    done; \
    chmod 0755 /rootfs/etc/acpi/powerbtn.sh; \
    ln -sf /dev/null /rootfs/etc/udev/rules.d/80-net-setup-link.rules; \
    ln -sf /etc/systemd/system/kontur-ssh-host-keys.service \
      /rootfs/etc/systemd/system/multi-user.target.wants/kontur-ssh-host-keys.service; \
    ln -sf /etc/systemd/system/kontur-mem-agent.service \
      /rootfs/etc/systemd/system/multi-user.target.wants/kontur-mem-agent.service; \
    ln -sf /etc/systemd/system/kontur-control-net.service \
      /rootfs/etc/systemd/system/multi-user.target.wants/kontur-control-net.service; \
    ln -sf /etc/systemd/system/kontur-authorized-key.service \
      /rootfs/etc/systemd/system/multi-user.target.wants/kontur-authorized-key.service; \
    rm -f /rootfs/etc/ssh/ssh_host_*; \
    install -d -m0700 /rootfs/root/.ssh; \
    : > /rootfs/root/.ssh/authorized_keys; \
    if [ -n "${GUEST_SSH_AUTHORIZED_KEY}" ]; then \
      printf '%s\n' "${GUEST_SSH_AUTHORIZED_KEY}" >> /rootfs/root/.ssh/authorized_keys; \
    fi; \
    chmod 0600 /rootfs/root/.ssh/authorized_keys

# Builds the Alpine variant of the guest rootfs, via `apk --root` instead
# of `debootstrap` -- the same technique Alpine's own official base image
# is built with, run from a live Alpine environment (this stage's own
# `/etc/apk`) targeting a fresh directory. `openrc`/`busybox` (both pulled
# in by alpine-base) stand in for `systemd-sysv` as PID 1 and service
# manager; see deploy/guest-image/README.md for how the two variants'
# overlays differ as a result -- notably, unlike Debian's openssh-server,
# Alpine's doesn't generate host keys at package-install time, and its own
# sshd OpenRC init script (checkconfig -> generate_host_keys) already
# fills in whatever's missing on every start, so there's no
# Alpine equivalent of kontur-ssh-host-keys.service needed at all.
FROM alpine:${GUEST_ALPINE_VERSION} AS guest-rootfs-alpine
ARG GUEST_ALPINE_VERSION
ARG GUEST_SSH_AUTHORIZED_KEY
# --root only changes where packages are installed; apk still reads its
# own trusted keys and repository list from this stage's own /etc/apk
# (i.e. this Alpine image's), not /rootfs's, unless told otherwise -- so
# --keys-dir/--repositories-file are pinned explicitly here rather than
# relying on the default (which is this stage's own /etc/apk, correct by
# accident, not by being told to).
RUN apk add --no-cache --initdb --root /rootfs \
      --keys-dir /etc/apk/keys --repositories-file /etc/apk/repositories \
      alpine-base openssh-server acpid iproute2 util-linux
RUN printf 'http://dl-cdn.alpinelinux.org/alpine/v%s/main\nhttp://dl-cdn.alpinelinux.org/alpine/v%s/community\n' \
      "${GUEST_ALPINE_VERSION}" "${GUEST_ALPINE_VERSION}" > /rootfs/etc/apk/repositories

COPY deploy/guest-image/overlay-common/ /rootfs/
COPY deploy/guest-image/overlay-alpine/ /rootfs/
COPY --from=build --chmod=0755 /out/kontur-mem-agent /rootfs/usr/local/bin/kontur-mem-agent

# Nothing enables OpenRC services on install the way Debian's
# deb-systemd-helper does (see the Debian stage above), so every service
# this guest needs is enabled by hand -- the OpenRC equivalent of a
# systemd unit's [Install] symlink is just a symlink of its own, from
# /etc/init.d/<service> into a runlevel directory:
# - sysinit/boot: the baseline a busybox-init/OpenRC system needs to reach
#   a usable state at all (device nodes, /proc & /sys, hostname, cleaning
#   /tmp, syslog) -- Debian's systemd does the equivalent internally, with
#   nothing to enable by hand for it.
# - default: sshd and acpid, this guest's actual raison d'etre.
RUN set -eux; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-ssh-console-wrap; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-control-net; \
    chmod 0755 /rootfs/etc/acpi/powerbtn.sh; \
    chmod 0755 /rootfs/etc/init.d/kontur-mem-agent; \
    chmod 0755 /rootfs/etc/init.d/kontur-control-net; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-authorized-key; \
    chmod 0755 /rootfs/etc/init.d/kontur-authorized-key; \
    for t in sed base64 awk mkdir chmod chown; do \
      [ -e "/rootfs/bin/$t" ] || [ -e "/rootfs/usr/bin/$t" ] || \
        { echo "kontur-authorized-key execs $t, which this rootfs does not have" >&2; exit 1; }; \
    done; \
    for svc in devfs dmesg mdev hwdrivers; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/sysinit/"${svc}"; \
    done; \
    for svc in hwclock modules sysctl hostname bootmisc syslog; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/boot/"${svc}"; \
    done; \
    for svc in local sshd acpid kontur-control-net kontur-mem-agent kontur-authorized-key; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/default/"${svc}"; \
    done; \
    install -d -m0700 /rootfs/root/.ssh; \
    : > /rootfs/root/.ssh/authorized_keys; \
    if [ -n "${GUEST_SSH_AUTHORIZED_KEY}" ]; then \
      printf '%s\n' "${GUEST_SSH_AUTHORIZED_KEY}" >> /rootfs/root/.ssh/authorized_keys; \
    fi; \
    chmod 0600 /rootfs/root/.ssh/authorized_keys

# Selects between the two guest-rootfs-* stages above -- an indirection
# forced by BuildKit not resolving ARGs in COPY --from, only FROM.
FROM guest-rootfs-${GUEST_DISTRO} AS guest-rootfs

# Applies GUEST_SETUP_SCRIPT, if one was given, inside the guest rootfs.
#
# The rootfs is promoted to an image stage of its own first, so the script
# runs as an ordinary RUN -- with a real /proc and /dev, working network,
# and the rootfs as / exactly as it will look booted -- rather than under
# `chroot`. That distinction is what preserves the "no extra privileges
# beyond an ordinary docker build" property the guest-image stage below
# already relies on for `mke2fs -d`: bind-mounting /proc and /dev into a
# chroot needs CAP_SYS_ADMIN, which a docker build does not have, and
# without them apt postinsts, `systemctl enable` and `update-initramfs`
# variously misbehave or fail outright.
#
# Both rootfs variants provide the /bin/sh this RUN needs (debootstrap
# --variant=minbase on Debian, busybox via alpine-base on Alpine), so this
# works the same way for either GUEST_DISTRO. With no script given the RUN
# is a no-op, and the stage is just a copy of the rootfs.
FROM scratch AS guest-customized
COPY --from=guest-rootfs /rootfs/ /
# A scratch-derived stage inherits no PATH, and while the runtime supplies
# a default when an image config sets none, this stage is the one place
# where the whole point is running a caller's own script -- so it is set
# explicitly rather than left to that fallback.
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ARG GUEST_SETUP_SCRIPT
RUN if [ -n "${GUEST_SETUP_SCRIPT}" ]; then \
      printf '%s\n' "${GUEST_SETUP_SCRIPT}" > /tmp/kontur-guest-setup.sh; \
      chmod 0755 /tmp/kontur-guest-setup.sh; \
      /tmp/kontur-guest-setup.sh; \
      rm -f /tmp/kontur-guest-setup.sh; \
    fi

# Packs guest-customized (the rootfs GUEST_DISTRO selected, with
# GUEST_SETUP_SCRIPT applied to it) into disk.img. `mke2fs -d` populates
# the filesystem straight from a directory tree, so this needs no
# loop-mount (and so no extra privileges beyond an ordinary docker build)
# regardless of which rootfs it's packing. Sized to the rootfs plus 20%
# headroom for logs/growth, rounded up, plus a fixed 64MiB floor so
# ext4's own overhead never starves it.
FROM debian:bookworm-slim AS guest-image
RUN apt-get update && apt-get install -y --no-install-recommends e2fsprogs \
    && rm -rf /var/lib/apt/lists/*
COPY --from=guest-customized / /rootfs
RUN set -eux; \
    size_kb=$(du -sk /rootfs | cut -f1); \
    img_kb=$(( (size_kb * 12 / 10) + 65536 )); \
    truncate -s "${img_kb}K" /disk.img; \
    mke2fs -F -t ext4 -L kontur-root -d /rootfs /disk.img

# A customized guest may bring its own kernel -- a distro kernel package
# installed by GUEST_SETUP_SCRIPT, for a guest whose workload needs more
# than the fetch-kernel stage's own build provides. Publish it beside
# disk.img when there is one, so a caller booting that guest has all of
# what it needs from this one build. Nothing is produced when there is
# not, and CHV_KERNEL's default (fetch-kernel's vmlinux, baked into the
# final image) still applies to the bundled guest either way.
RUN set -eux; \
    mkdir -p /out; \
    for pattern in vmlinuz initrd.img; do \
      newest="$(ls -1 /rootfs/boot/${pattern}-* 2>/dev/null | sort -V | tail -1 || true)"; \
      if [ -n "${newest}" ]; then cp "${newest}" "/out/${pattern}"; fi; \
    done

# Everything a caller needs to boot this guest elsewhere, and nothing
# else, so `docker build --target guest-artifacts --output type=local,dest=<dir>`
# yields exactly those files rather than a whole builder filesystem.
FROM scratch AS guest-artifacts
COPY --from=guest-image /disk.img /disk.img
COPY --from=guest-image /out/ /

# netshim mode still needs CAP_NET_ADMIN/CAP_NET_RAW at runtime (creating
# the bridge/taps and nftables NAT rules), but as of `internal/netshim`
# talking to the kernel directly via netlink/nftables Go libraries instead
# of exec'ing the `ip`/`iptables` CLIs, it no longer needs a userland that
# provides those binaries -- so unlike before, this image doesn't need a
# real distro base at all. `scratch` (literally nothing but the two
# binaries and the guest disk image below) is the smallest possible base;
# it costs the `iptables`/`ip`-based debuggability a real userland gave
# (no shell, no coreutils, no package manager to `apk add` a debug tool
# into), traded for the smaller image and attack surface. Both `kontur`
# and `cloud-hypervisor` are statically linked (see the "build" and
# "fetch-chv" stages above), so neither needs a libc from the base image.
FROM scratch AS final
COPY --from=fetch-chv /out/cloud-hypervisor /usr/local/bin/cloud-hypervisor
COPY --from=build /out/kontur /usr/local/bin/kontur
# See the "build" stage above -- these are symlinks back to the same
# /usr/local/bin/kontur binary just copied in, not real shells: this
# image is still "scratch" underneath (no coreutils, no package manager),
# it just also answers to "sh"/"bash" the way one would.
COPY --from=build /out/shims/ /
COPY --from=guest-image /disk.img /var/lib/kontur/guest/disk.img
# See the "fetch-kernel" stage above and internal/config's defaultKernel:
# a direct-kernel-boot VM works out of the box, the same way the guest
# disk image above does, without anything needing to be supplied from
# outside the image (a node-local kernel cache, a hostPath, or an
# emptyDir populated by a fetch-at-pod-start init container).
COPY --from=fetch-kernel /out/vmlinux /var/lib/kontur/guest/vmlinux

LABEL org.opencontainers.image.title="kontur" \
      org.opencontainers.image.description="Boots a single cloud-hypervisor VM as PID 1 (\"run\", the default), sets up pod-local VM networking (\"netshim\"), or execs a command inside the VM guest over SSH (\"exec\")" \
      org.opencontainers.image.source="https://github.com/bwsalmon/kontur"

ENTRYPOINT ["/usr/local/bin/kontur"]
