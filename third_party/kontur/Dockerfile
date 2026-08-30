# syntax=docker/dockerfile:1

# Builds the kontur OCI image: a single Go binary that either boots a
# cloud-hypervisor VM as PID 1 of a container ("run", the default) or sets
# up pod-local networking for a group of such VMs as an init container
# ("netshim"), plus the cloud-hypervisor binary itself and a minimal guest
# disk image usable out of the box. See README.md and
# deploy/guest-image/README.md.

ARG GO_VERSION=1.22
ARG CLOUD_HYPERVISOR_VERSION=v53.0
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
# GUEST_SETUP_SCRIPT, if set, is the text of a script run (via chroot) against
# the guest rootfs before it's packed into disk.img -- see
# deploy/guest-image/README.md for what it can and can't do there.
ARG GUEST_SETUP_SCRIPT=""

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kontur ./cmd/kontur

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

# Builds the Debian variant of the guest rootfs baked into the final
# kontur image: a minimal Debian rootfs with sshd, via `debootstrap`. See
# the "alpine" variant below and deploy/guest-image/README.md for how the
# two compare.
FROM debian:bookworm-slim AS guest-rootfs-debian
ARG GUEST_SUITE
ARG GUEST_SSH_AUTHORIZED_KEY
ARG GUEST_SETUP_SCRIPT
RUN apt-get update && apt-get install -y --no-install-recommends \
      debootstrap ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN debootstrap --variant=minbase \
      --include=openssh-server,systemd-sysv,iproute2,acpid \
      "${GUEST_SUITE}" /rootfs http://deb.debian.org/debian

COPY deploy/guest-image/overlay-common/ /rootfs/
COPY deploy/guest-image/overlay-debian/ /rootfs/

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
# - GUEST_SSH_AUTHORIZED_KEY, if given, is the only way to log in: root
#   has no password at all (see overlay-common/etc/ssh/sshd_config.d).
RUN set -eux; \
    chmod 0755 /rootfs/usr/local/libexec/kontur-ssh-console-wrap; \
    chmod 0755 /rootfs/etc/acpi/powerbtn.sh; \
    ln -sf /etc/systemd/system/kontur-ssh-host-keys.service \
      /rootfs/etc/systemd/system/multi-user.target.wants/kontur-ssh-host-keys.service; \
    rm -f /rootfs/etc/ssh/ssh_host_*; \
    if [ -n "${GUEST_SSH_AUTHORIZED_KEY}" ]; then \
      install -d -m0700 /rootfs/root/.ssh; \
      printf '%s\n' "${GUEST_SSH_AUTHORIZED_KEY}" > /rootfs/root/.ssh/authorized_keys; \
      chmod 0600 /rootfs/root/.ssh/authorized_keys; \
    fi

# Runs GUEST_SETUP_SCRIPT, if given, against the rootfs via chroot -- the
# same mechanism debootstrap itself already used above to configure
# packages, so this needs no privileges beyond what this stage already
# has, and network access works fine (chroot doesn't touch the network
# namespace). See deploy/guest-image/README.md for what it can't do
# (no /proc, /sys, or running service manager).
RUN set -eux; \
    if [ -n "${GUEST_SETUP_SCRIPT}" ]; then \
      script=/rootfs/tmp/kontur-setup-script; \
      printf '%s\n' "${GUEST_SETUP_SCRIPT}" > "${script}"; \
      chmod 0755 "${script}"; \
      chroot /rootfs /tmp/kontur-setup-script; \
      rm -f "${script}"; \
    fi

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
ARG GUEST_SETUP_SCRIPT
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
    chmod 0755 /rootfs/etc/acpi/powerbtn.sh; \
    for svc in devfs dmesg mdev hwdrivers; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/sysinit/"${svc}"; \
    done; \
    for svc in hwclock modules sysctl hostname bootmisc syslog; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/boot/"${svc}"; \
    done; \
    for svc in local sshd acpid; do \
      ln -sf /etc/init.d/"${svc}" /rootfs/etc/runlevels/default/"${svc}"; \
    done; \
    if [ -n "${GUEST_SSH_AUTHORIZED_KEY}" ]; then \
      install -d -m0700 /rootfs/root/.ssh; \
      printf '%s\n' "${GUEST_SSH_AUTHORIZED_KEY}" > /rootfs/root/.ssh/authorized_keys; \
      chmod 0600 /rootfs/root/.ssh/authorized_keys; \
    fi

# See the Debian stage above -- same mechanism, run from within this
# stage's own matching Alpine/musl environment instead.
RUN set -eux; \
    if [ -n "${GUEST_SETUP_SCRIPT}" ]; then \
      script=/rootfs/tmp/kontur-setup-script; \
      printf '%s\n' "${GUEST_SETUP_SCRIPT}" > "${script}"; \
      chmod 0755 "${script}"; \
      chroot /rootfs /tmp/kontur-setup-script; \
      rm -f "${script}"; \
    fi

# Selects between the two guest-rootfs-* stages above -- an indirection
# forced by BuildKit not resolving ARGs in COPY --from, only FROM.
FROM guest-rootfs-${GUEST_DISTRO} AS guest-rootfs

# Packs guest-rootfs (whichever of the two stages above GUEST_DISTRO
# selected) into disk.img. `mke2fs -d` populates the filesystem straight
# from a directory tree, so this needs no loop-mount (and so no extra
# privileges beyond an ordinary docker build) regardless of which rootfs
# it's packing. Sized to the rootfs plus 20% headroom for logs/growth,
# rounded up, plus a fixed 64MiB floor so ext4's own overhead never
# starves it.
FROM debian:bookworm-slim AS guest-image
RUN apt-get update && apt-get install -y --no-install-recommends e2fsprogs \
    && rm -rf /var/lib/apt/lists/*
COPY --from=guest-rootfs /rootfs /rootfs
RUN set -eux; \
    size_kb=$(du -sk /rootfs | cut -f1); \
    img_kb=$(( (size_kb * 12 / 10) + 65536 )); \
    truncate -s "${img_kb}K" /disk.img; \
    mke2fs -F -t ext4 -L kontur-root -d /rootfs /disk.img

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
COPY --from=guest-image /disk.img /var/lib/kontur/guest/disk.img

LABEL org.opencontainers.image.title="kontur" \
      org.opencontainers.image.description="Boots a single cloud-hypervisor VM as PID 1 (\"run\", the default) or sets up pod-local VM networking (\"netshim\")" \
      org.opencontainers.image.source="https://github.com/bwsalmon/kontur"

ENTRYPOINT ["/usr/local/bin/kontur"]
