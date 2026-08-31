#!/bin/bash
# Builds a grain sandbox guest image through bwsalmon/kontur's own guest
# build -- one `docker build` against third_party/kontur's Dockerfile,
# with guest-setup.sh handed to its GUEST_SETUP_SCRIPT hook and its
# `guest-artifacts` target exporting the result.
#
# This replaces the debootstrap-and-chroot build that used to live in
# build.sh/provision.sh. That build duplicated, as root, work kontur's own
# Dockerfile already does without any privileges at all: kontur's
# guest-rootfs-debian stage debootstraps the rootfs, its guest-customized
# stage runs a caller's script inside it as an ordinary RUN (no chroot, so
# no CAP_SYS_ADMIN), and its guest-image stage packs it with `mke2fs -d`
# (no loop mount). Building on top of that instead of beside it also means
# the guest carries kontur's own guest overlays -- among them
# kontur-control-net, which configures the control link kontur's flat
# networking mode reaches the guest on, and which a rootfs built here from
# scratch would not have had.
#
# What it produces is unchanged: ${OUTPUT_DIR}/{disk.img,vmlinuz,initrd.img},
# published to gs://${KONTUR_IMAGE_BUCKET}/kontur-guest/ when that is set.
# The kernel is still Debian's own linux-image-amd64, installed by
# guest-setup.sh rather than kontur's baked-in cloud-hypervisor release
# build -- see README.md, "Why no custom kernel".
#
# Needs: docker. Notably *not* root, debootstrap, or mke2fs, all of which
# the previous build did need.
#
# Usage:
#   OPERATOR_SSH_PUBLIC_KEY="$(cat ~/.ssh/id_ed25519.pub)" ./build-guest.sh
set -euo pipefail
cd "$(dirname "$0")"

: "${OPERATOR_SSH_PUBLIC_KEY:?set OPERATOR_SSH_PUBLIC_KEY to the operator SSH public key this image should carry (see README.md)}"

if ! command -v docker >/dev/null 2>&1; then
  echo "build-guest.sh: docker not found" >&2
  exit 1
fi

# shquote renders a value as a single-quoted POSIX shell word, so it can
# be embedded in the script text below whatever it contains.
shquote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# kontur's hook writes GUEST_SETUP_SCRIPT to a file and execs it with only
# its own build stage's environment (third_party/kontur's Dockerfile,
# guest-customized stage), so the two variables guest-setup.sh reads have
# to travel inside the script text itself. They are inserted immediately
# after the shebang, which has to stay on line 1 for the exec to work.
setup_script="$(
  head -n 1 guest-setup.sh
  printf 'OPERATOR_SSH_PUBLIC_KEY=%s\n' "$(shquote "${OPERATOR_SSH_PUBLIC_KEY}")"
  printf 'SANDBOX_SETUP_SCRIPT=%s\n' "$(shquote "${SANDBOX_SETUP_SCRIPT:-}")"
  tail -n +2 guest-setup.sh
)"

image_name="${IMAGE_NAME:-kontur-guest}"
version="$(git -C .. rev-parse --short HEAD 2>/dev/null || echo unknown)-$(date -u +%Y%m%d%H%M%S)"
# OUTPUT_DIR lets a caller that already knows exactly where it wants the
# result -- v2/scripts/setup.sh's own ensure_kontur_images, building this
# locally on every host and caching the result by a content hash of its
# own choosing -- skip parsing this script's stdout (or the timestamp in
# $version, different on every invocation) to find it afterward. Unset,
# this is exactly the path a human running this by hand always got.
output_dir="${OUTPUT_DIR:-output/${image_name}-${version}}"
mkdir -p "$output_dir"

echo "building guest into ${output_dir} from ../../third_party/kontur"
# The Dockerfile uses `RUN --mount=type=cache`, which only the BuildKit
# builder understands -- the classic builder fails outright on it ("the
# --mount option requires BuildKit"). --output likewise needs BuildKit.
DOCKER_BUILDKIT=1 docker build \
  --target guest-artifacts \
  --build-arg GUEST_SETUP_SCRIPT="$setup_script" \
  --output "type=local,dest=${output_dir}" \
  ../../third_party/kontur

# guest-artifacts publishes vmlinuz/initrd.img only when the guest has its
# own -- i.e. when the setup script installed a kernel package. This one
# does (linux-image-amd64), so their absence means that install silently
# didn't happen, and a disk.img alone would boot under kontur's own baked
# kernel instead: a guest without the config docker and kind need, failing
# much later and much less legibly than here.
for f in disk.img vmlinuz initrd.img; do
  if [ ! -s "${output_dir}/${f}" ]; then
    echo "build-guest.sh: ${output_dir}/${f} missing or empty after the build -- guest-setup.sh's linux-image-amd64 install is the usual cause" >&2
    exit 1
  fi
done

echo "built: ${output_dir}/{vmlinuz,initrd.img,disk.img}"

if [ -n "${KONTUR_IMAGE_BUCKET:-}" ]; then
  dest="gs://${KONTUR_IMAGE_BUCKET}/kontur-guest/${image_name}-${version}"
  gsutil -m cp "${output_dir}/vmlinuz" "${output_dir}/initrd.img" "${output_dir}/disk.img" "${dest}/"
  echo "published: ${dest}/{vmlinuz,initrd.img,disk.img}"

  # Also publish under a stable "latest" prefix, alongside the versioned
  # one above -- v2/scripts/setup.sh's own ensure_kontur_images always
  # fetches this fixed location rather than discovering or hardcoding
  # today's <git-sha>-<timestamp> version string itself. The versioned
  # copy above is kept too, so a previous guest image is still there to
  # roll back to by hand if a new one turns out to be broken.
  latest="gs://${KONTUR_IMAGE_BUCKET}/kontur-guest/latest"
  gsutil -m cp "${output_dir}/vmlinuz" "${output_dir}/initrd.img" "${output_dir}/disk.img" "${latest}/"
  echo "published: ${latest}/{vmlinuz,initrd.img,disk.img} (alias for ${image_name}-${version})"
else
  echo "KONTUR_IMAGE_BUCKET not set -- not published, image left at ${output_dir}"
fi
