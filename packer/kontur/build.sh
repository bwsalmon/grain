#!/bin/bash
# Builds a kontur guest image straight from a debootstrap rootfs -- no
# Packer, no QEMU, no VM boot at all -- and, if KONTUR_IMAGE_BUCKET is
# set, publishes the result. See README.md in this directory for the full
# picture, especially "Why no VM boot to build this" and "Why no custom
# kernel".
#
# Needs root: debootstrap, mount (bind-mounts for chroot), and mke2fs -d
# all do. Run as `sudo -E ./build.sh` (-E so OPERATOR_SSH_PUBLIC_KEY/
# SANDBOX_SETUP_SCRIPT/KONTUR_IMAGE_BUCKET/IMAGE_NAME survive into root's
# environment) rather than as root directly, so a real login shell's own
# PATH/environment is what runs it.
set -euo pipefail
cd "$(dirname "$0")"

: "${OPERATOR_SSH_PUBLIC_KEY:?set OPERATOR_SSH_PUBLIC_KEY to the operator SSH public key this image should carry (see README.md)}"

if [ "$(id -u)" -ne 0 ]; then
  echo "build.sh: must run as root (debootstrap, mount and mke2fs all need it) -- try sudo -E ./build.sh" >&2
  exit 1
fi

for bin in debootstrap mke2fs; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "build.sh: $bin not found -- on Debian/Ubuntu, apt-get install debootstrap e2fsprogs" >&2
    exit 1
  fi
done

work_dir="$(mktemp -d)"
rootfs="${work_dir}/rootfs"
mkdir -p "$rootfs"

cleanup() {
  # Reverse order of the mounts below; -l (lazy) so a stray process this
  # script itself never started (there shouldn't be one -- nothing here
  # execs anything long-lived inside the chroot) can't wedge the unmount.
  umount -l "${rootfs}/dev/pts" 2>/dev/null || true
  umount -l "${rootfs}/dev" 2>/dev/null || true
  umount -l "${rootfs}/proc" 2>/dev/null || true
  umount -l "${rootfs}/sys" 2>/dev/null || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

echo "debootstrapping bookworm into ${rootfs}"
debootstrap --variant=minbase bookworm "$rootfs" http://deb.debian.org/debian

# provision.sh needs real network access (apt-get, curl) and, for
# update-initramfs/systemctl/depmod, a real /proc and /dev -- chroot does
# not create a new network or PID namespace, so binding these in is
# enough; no separate VM boot needed the way this directory's previous
# Packer-based build required one just to get a shell inside the image
# being built. See README.md, "Why no VM boot to build this".
cp /etc/resolv.conf "${rootfs}/etc/resolv.conf"
mount --bind /dev "${rootfs}/dev"
mount --bind /dev/pts "${rootfs}/dev/pts"
mount -t proc proc "${rootfs}/proc"
mount -t sysfs sysfs "${rootfs}/sys"

install -m0755 provision.sh "${rootfs}/provision.sh"
chroot "$rootfs" /usr/bin/env \
  "OPERATOR_SSH_PUBLIC_KEY=${OPERATOR_SSH_PUBLIC_KEY}" \
  "SANDBOX_SETUP_SCRIPT=${SANDBOX_SETUP_SCRIPT:-}" \
  /provision.sh
rm -f "${rootfs}/provision.sh"

umount -l "${rootfs}/dev/pts"
umount -l "${rootfs}/dev"
umount -l "${rootfs}/proc"
umount -l "${rootfs}/sys"

kernel_path="$(compgen -G "${rootfs}/boot/vmlinuz-*" | head -1)"
initrd_path="$(compgen -G "${rootfs}/boot/initrd.img-*" | head -1)"
if [ -z "$kernel_path" ] || [ -z "$initrd_path" ]; then
  echo "build.sh: no kernel/initramfs found under ${rootfs}/boot after provisioning" >&2
  exit 1
fi

image_name="${IMAGE_NAME:-kontur-guest}"
version="$(git -C .. rev-parse --short HEAD 2>/dev/null || echo unknown)-$(date -u +%Y%m%d%H%M%S)"
output_dir="output/${image_name}-${version}"
mkdir -p "$output_dir"

cp "$kernel_path" "${output_dir}/vmlinuz"
cp "$initrd_path" "${output_dir}/initrd.img"

# Sized to the rootfs plus 20% headroom for logs/growth, rounded up, plus
# a fixed 64MiB floor so ext4's own overhead never starves it -- the same
# formula bwsalmon/kontur's own guest-image Dockerfile stage uses
# (third_party/kontur/Dockerfile) for the same reason: mke2fs -d packs
# the image directly from the rootfs directory, no loop-mount or
# separate partitioning step, so there is no partition table and no
# bootloader/firmware in this image at all -- see README.md, "Why no
# custom kernel" for why cloud-hypervisor boots it directly instead.
size_kb=$(du -sk "$rootfs" | cut -f1)
img_kb=$(( (size_kb * 12 / 10) + 65536 ))
disk_path="${output_dir}/disk.img"
truncate -s "${img_kb}K" "$disk_path"
mke2fs -F -q -t ext4 -L kontur-root -d "$rootfs" "$disk_path"

echo "built: ${output_dir}/{vmlinuz,initrd.img,disk.img}"

if [ -n "${KONTUR_IMAGE_BUCKET:-}" ]; then
  dest="gs://${KONTUR_IMAGE_BUCKET}/kontur-guest/${image_name}-${version}"
  gsutil -m cp "${output_dir}/vmlinuz" "${output_dir}/initrd.img" "${output_dir}/disk.img" "${dest}/"
  echo "published: ${dest}/{vmlinuz,initrd.img,disk.img}"
else
  echo "KONTUR_IMAGE_BUCKET not set -- not published, image left at ${output_dir}"
fi
