#!/bin/bash
# Builds the grain sandbox guest image: boots the pinned kontur base,
# runs guest-setup.sh inside it, and commits the result.
#
# What comes out is an ordinary OCI image, and specifically the same kind
# of image as the base -- kontur, cloud-hypervisor and a bootable guest
# disk -- so `docker run` on it boots a VM. That is why grain has one
# artifact here rather than two: this image *is* the sandbox container a
# deployment runs, and the guest inside it is the one every dispatched
# task gets. cmd/grain/sandboximage.go stamps its reference into the
# binary, and scripts/setup.sh pulls it and nothing else.
#
# This replaces a `docker build` that produced disk.img/vmlinuz/initrd.img
# for every deployment host to build for itself. The reason that build
# existed at all -- guest-setup.sh baking a per-deployment SSH key --
# stopped being true when kontur moved to a per-boot keypair, and
# bwsalmon/kontur#36 gave `konturctl guest build` as the way to derive a
# guest from a published image instead. See third_party/kontur/VENDORED.md.
#
# Needs: docker, /dev/kvm, and Go (to build konturctl from the vendored
# tree). The KVM requirement is new and is the cost of provisioning
# inside a booted VM rather than a container: it buys a setup script that
# runs against the real kernel and a real systemd, with none of the
# install-only restrictions a container build imposes. CI has KVM;
# a machine without it cannot build this image.
#
# Usage:
#   ./build-guest.sh                       # -> grain-guest:dev
#   IMAGE=ghcr.io/... ./build-guest.sh     # -> that reference
set -euo pipefail
cd "$(dirname "$0")"

# The base is pinned to the exact kontur commit third_party/kontur is
# vendored from, by the immutable per-commit tag kontur's CI writes once
# for that SHA. konturctl below is built from that same tree, and the two
# only agree on the guest-side contract -- the authorized-key installer,
# the control-net overlay, the mem-agent, the disk modes -- because they
# are one commit. Re-vendoring moves both, and this SHA appearing in
# VENDORED.md too is what makes a mismatch visible.
KONTUR_GUEST_BASE="${KONTUR_GUEST_BASE:-ghcr.io/bwsalmon/kontur:debian12-e2b8b4506babe9c787f6b3943d8a20cfd549eeb1}"
IMAGE="${IMAGE:-grain-guest:dev}"

if ! command -v docker >/dev/null 2>&1; then
  echo "build-guest.sh: docker not found" >&2
  exit 1
fi
if [ ! -e /dev/kvm ]; then
  echo "build-guest.sh: /dev/kvm is not present -- this build boots the guest to provision it" >&2
  exit 1
fi

# Built from the vendored tree rather than taken from PATH: the whole
# point of the pin above is that one commit drives both halves, and a
# konturctl that happened to be installed on this machine is not that.
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
echo "building konturctl from third_party/kontur"
(cd ../../third_party/kontur && go build -o "$workdir/konturctl" ./cmd/konturctl)

echo "building ${IMAGE} from ${KONTUR_GUEST_BASE}"
"$workdir/konturctl" guest build \
  -from "$KONTUR_GUEST_BASE" \
  -setup ./guest-setup.sh \
  -t "$IMAGE" \
  "$@"

# Re-label, because `docker commit` carries the base image's config
# forward -- including org.opencontainers.image.source, which on the
# kontur base names *kontur's* repository. GHCR uses that label alone to
# decide which repository a package belongs to, so publishing this
# unchanged would attach grain's guest to kontur's namespace, silently:
# the build succeeds, the push succeeds, and the package lands somewhere
# nobody is looking. Left unset the vendored label stands, which is right
# for an operator building into their own registry.
if [ -n "${GUEST_SOURCE_REPO:-}" ]; then
  echo "labelling ${IMAGE} as ${GUEST_SOURCE_REPO}"
  printf 'FROM %s\nLABEL org.opencontainers.image.source=%s\n' "$IMAGE" "$GUEST_SOURCE_REPO" \
    | docker build -t "$IMAGE" -f - "$workdir"
fi

echo "built: ${IMAGE}"
