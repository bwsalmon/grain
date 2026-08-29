# Packages the benchmarks/kernel/build.sh output so it can be staged onto
# a GKE node's local filesystem via stage-assets-job.yaml, mirroring how a
# real deployment would pre-populate a node-local image cache (see the
# main README's "How it works" section: kontur never fetches images
# itself).
FROM busybox:stable
COPY vmlinux /assets/vmlinux
COPY initramfs.img /assets/initramfs.img
COPY disk.img /assets/disk.img
