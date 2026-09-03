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

# The base is built from third_party/kontur, not pulled.
#
# It was pulled at first, by the immutable per-commit tag kontur's CI
# publishes -- which fails for a reason worth recording, because it is
# not a permissions bug to work around. A GitHub Actions GITHUB_TOKEN is
# scoped to its own repository: grain's can push grain's packages and
# cannot read kontur's, so `docker pull` of a private kontur package from
# grain's CI is denied however the login went. Granting grain read on
# that package would fix it, and building the base here is better anyway.
#
# Better because of what the pin was for. konturctl below is built from
# the vendored tree, and it only agrees with the guest on the guest-side
# contract -- the authorized-key installer, the control-net overlay, the
# mem-agent, the disk modes -- if the two come from one commit. A pinned
# tag asserts that by convention, kept true by remembering to move two
# things at once. Building the base from the same tree makes it true by
# construction: there is no second version to keep in step, and a resync
# moves both because they are the same files.
#
# The three build args are the ones kontur's CI publishes its "debian12"
# variant with. A distro kernel, because docker and kind inside the guest
# need overlayfs, cgroup v2, bridge netfilter and veth, which a kernel
# built for cloud-hypervisor's own CI does not promise; no console
# wrapper, because it runs every SSH session under a pty, which rewrites
# newlines and merges stderr into stdout -- corrupting every file grain's
# sandbox tools read back; and disk headroom, without which
# guest-setup.sh below cannot install anything at all.
#
# That last one is the consequence of provisioning a booted guest rather
# than a rootfs mid-build. kontur sizes the filesystem to what it packs
# plus 20%, which was sized *after* the toolchain landed when
# guest-setup.sh ran inside the image build. It runs after packing now,
# so everything it installs has to fit in headroom asked for up front:
# without enough, the guest boots perfectly and apt fails with "You don't
# have enough free space in /var/cache/apt/archives/", which reads as a
# broken setup script rather than as a disk sized before anyone knew what
# would go on it.
#
# 6144 is sized against what guest-setup.sh actually installs, not
# against what feels safe: ~434MB of the base package set, ~450MB of
# docker and containerd, ~1.2GB of google-cloud-cli and terraform, and
# ~1GB of Go, Node and their warm caches -- plus the node_modules `npm
# ci` builds and throws away, which is peak rather than residue. It is
# not free: `truncate` leaves the extra as a hole and a layer of zeros
# pushes as almost nothing, but extracting a layer materializes the hole,
# so every GB here is a GB of disk on every machine that pulls the image
# and on every CI runner that builds it. 8192 was tried in kontur's own
# CI first and made identical builds flake on runner disk.
#
# KONTUR_GUEST_BASE overrides it with an image of your own, published or
# local, and skips the build.
KONTUR_GUEST_BASE="${KONTUR_GUEST_BASE:-}"
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
# point above is that one commit drives both halves, and a konturctl that
# happened to be installed on this machine is not that.
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
echo "building konturctl from third_party/kontur"
(cd ../../third_party/kontur && go build -o "$workdir/konturctl" ./cmd/konturctl)

if [ -z "$KONTUR_GUEST_BASE" ]; then
  KONTUR_GUEST_BASE="kontur-guest-base:vendored"
  echo "building ${KONTUR_GUEST_BASE} from third_party/kontur"
  DOCKER_BUILDKIT=1 docker build \
    --build-arg GUEST_KERNEL_PACKAGE=linux-image-amd64 \
    --build-arg GUEST_CONSOLE_WRAP=0 \
    --build-arg GUEST_DISK_EXTRA_MB=6144 \
    -t "$KONTUR_GUEST_BASE" \
    ../../third_party/kontur
fi

# shquote renders a value as a single-quoted POSIX shell word, so it can
# be embedded in the script text below whatever it contains.
shquote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# The Go the guest carries, read out of go.mod with the same expression
# the Makefile uses for Dockerfile.build's own GO_VERSION -- one version,
# written down once, in the file that already has to be right. A guest
# whose Go disagreed with the module's would fail a dispatched task's
# `go build` for a reason that has nothing to do with that task.
go_version="$(sed -n 's/^go[[:space:]]\{1,\}//p' ../../go.mod)"
if [ -z "$go_version" ]; then
  echo "build-guest.sh: no 'go' line in ../../go.mod, so there is no toolchain version to install" >&2
  exit 1
fi

# What the guest's module and npm caches get warmed from -- see
# guest-setup.sh's "The Go and Node toolchains" for why an image that
# already holds the whole dependency graph is worth its size.
#
# Compressed and base64'd rather than carried as files because the script
# reaches the guest as one command-line argument: konturctl base64-encodes
# the whole thing into a `kontur exec` command, and Linux caps a single
# argument at 128KB (MAX_ARG_STRLEN). ui/package-lock.json alone is ~125KB
# raw; gzipped and base64'd, all four manifests together are ~48KB, which
# leaves the spliced script at ~75KB and konturctl's own encoding of it at
# ~100KB. Worth knowing if that lock file ever doubles: the failure would
# be an E2BIG out of konturctl, not from anything here.
manifests="$(tar -C ../.. -czf - go.mod go.sum ui/package.json ui/package-lock.json | base64 -w0)"

# konturctl runs the setup script with only the guest's own environment,
# so every variable guest-setup.sh reads has to travel inside the script
# text itself. They go immediately after the shebang, which has to stay
# on line 1.
#
# There used to be a third, OPERATOR_SSH_PUBLIC_KEY, and it was the reason
# the image this produces was deployment-specific. kontur now generates a
# keypair per boot and passes the public half to the guest on the kernel
# command line, so what comes out of here is generic -- see
# guest-setup.sh's own header.
setup="$workdir/guest-setup.sh"
{
  head -n 1 guest-setup.sh
  printf 'GO_VERSION=%s\n' "$(shquote "$go_version")"
  printf 'GRAIN_DEP_MANIFESTS=%s\n' "$(shquote "$manifests")"
  tail -n +2 guest-setup.sh
} > "$setup"

echo "building ${IMAGE} from ${KONTUR_GUEST_BASE}"
"$workdir/konturctl" guest build \
  -from "$KONTUR_GUEST_BASE" \
  -setup "$setup" \
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
