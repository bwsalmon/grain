# syntax=docker/dockerfile:1

# Builds the kontur OCI image: a single Go binary that either boots a
# cloud-hypervisor VM as PID 1 of a container ("run", the default) or sets
# up pod-local networking for a group of such VMs as an init container
# ("netshim"), plus the cloud-hypervisor binary itself and a minimal guest
# disk image usable out of the box. See README.md and
# deploy/guest-image/README.md.

ARG GO_VERSION=1.22
ARG CLOUD_HYPERVISOR_VERSION=v53.0
ARG GUEST_SUITE=bookworm
# GUEST_SSH_AUTHORIZED_KEY is an SSH *public* key (see
# deploy/guest-image/README.md), not a secret, so baking it into the image
# history via a plain ARG is fine.
ARG GUEST_SSH_AUTHORIZED_KEY=""

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod ./
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

# Builds the guest disk image baked into the final kontur image: a minimal
# Debian rootfs with sshd, packed directly into an ext4 filesystem image.
# `mke2fs -d` populates the filesystem straight from a directory tree, so
# this needs no loop-mount (and so no extra privileges beyond an ordinary
# docker build). See deploy/guest-image/README.md for what's in it, and
# why SSH sessions on it get mirrored to the guest's serial console.
FROM debian:bookworm-slim AS guest-image
ARG GUEST_SUITE
ARG GUEST_SSH_AUTHORIZED_KEY
RUN apt-get update && apt-get install -y --no-install-recommends \
      debootstrap e2fsprogs ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN debootstrap --variant=minbase \
      --include=openssh-server,systemd-sysv,iproute2,acpid \
      "${GUEST_SUITE}" /rootfs http://deb.debian.org/debian

COPY deploy/guest-image/overlay/ /rootfs/

# - The console wrapper and ACPI power-button handler need their
#   executable bit set (COPY doesn't preserve it from build context
#   permissions reliably across git).
# - kontur-ssh-host-keys.service is enabled by hand (a symlink into
#   multi-user.target.wants) rather than via `systemctl enable`, since
#   systemd isn't running in this chroot to do it for us. acpid's own
#   postinst already enables acpid.socket/acpid.path the same way (via
#   deb-systemd-helper, which just writes symlinks and doesn't need a
#   running systemd), so no equivalent hand-enabling is needed for it --
#   only the event handler itself (overlay/etc/acpi/) is ours to add.
# - openssh-server's postinst already generated host keys during
#   debootstrap; they're removed so every VM booted from this image
#   doesn't share the same ones -- kontur-ssh-host-keys.service
#   regenerates them on first boot instead.
# - GUEST_SSH_AUTHORIZED_KEY, if given, is the only way to log in: root
#   has no password at all (see overlay/etc/ssh/sshd_config.d).
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

# Size the image to the rootfs plus 20% headroom for logs/growth, rounded
# up, plus a fixed 64MiB floor so ext4's own overhead never starves it.
RUN set -eux; \
    size_kb=$(du -sk /rootfs | cut -f1); \
    img_kb=$(( (size_kb * 12 / 10) + 65536 )); \
    truncate -s "${img_kb}K" /disk.img; \
    mke2fs -F -t ext4 -L kontur-root -d /rootfs /disk.img

# netshim mode drives the `ip`/`iptables` CLIs and needs
# CAP_NET_ADMIN/CAP_NET_RAW at runtime, so unlike a chv-runtime-only image
# this can't use a distroless base: the same binary and image now serve
# both roles (VM container and netshim init container), just invoked with
# different args, so both roles' runtime dependencies have to be present.
FROM debian:bookworm-slim AS final
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 iptables \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/kontur/guest
COPY --from=fetch-chv /out/cloud-hypervisor /usr/local/bin/cloud-hypervisor
COPY --from=build /out/kontur /usr/local/bin/kontur
COPY --from=guest-image /disk.img /var/lib/kontur/guest/disk.img

LABEL org.opencontainers.image.title="kontur" \
      org.opencontainers.image.description="Boots a single cloud-hypervisor VM as PID 1 (\"run\", the default) or sets up pod-local VM networking (\"netshim\")" \
      org.opencontainers.image.source="https://github.com/bwsalmon/kontur"

ENTRYPOINT ["/usr/local/bin/kontur"]
