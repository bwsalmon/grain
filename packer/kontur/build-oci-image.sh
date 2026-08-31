#!/bin/bash
# Builds and pushes the bwsalmon/kontur OCI image (third_party/kontur's
# own Dockerfile) -- the other half of what a kontur-backed deployment
# needs published ahead of time, alongside this directory's own build.sh
# (the guest image, built via debootstrap+chroot, no Docker involved).
# See terraform/gcp-v2/README.md, "Kontur sandboxing", for how the two
# fit together and where v2/scripts/setup.sh fetches each from.
#
# Run this once (and again whenever third_party/kontur changes -- see
# that directory's own VENDORED.md), not on every deploy: it's a plain
# `docker build`/`docker push`, no root needed, but still real work worth
# doing ahead of a deploy rather than inside one.
#
# The image this produces is deliberately generic: it leaves
# GUEST_SSH_AUTHORIZED_KEY at the Dockerfile's own empty default (there is
# no second build arg for customizing the guest beyond that -- an earlier
# version of this comment named a GUEST_SETUP_SCRIPT that
# third_party/kontur has never actually had) because a real deployment
# never boots its bundled guest disk at all -- KonturConfig.CreateArgs always points
# -disk/-kernel/-initramfs at this directory's own build.sh output
# instead (see README.md's own -kontur-create-arg example). All this
# image contributes at runtime is the `kontur` binary and the pinned
# cloud-hypervisor release inside it; v2/scripts/setup.sh's own
# ensure_kontur_images retags whatever is pulled here to konturctl's own
# default "localhost:5000/kontur:latest" so no -kontur-image override is
# needed either.
#
# Needs: docker, authenticated to push to KONTUR_OCI_IMAGE's own registry
# (e.g. `gcloud auth configure-docker <region>-docker.pkg.dev` for
# Artifact Registry) -- this script does not attempt that itself, since
# the right auth depends on where you're pushing from. Not needed at all
# when KONTUR_OCI_SKIP_PUSH=1 (below) -- v2/scripts/setup.sh's own
# ensure_kontur_images sets exactly that to build straight into the local
# docker image store, with no registry involved.
#
# Usage:
#   KONTUR_OCI_IMAGE=us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest \
#     ./build-oci-image.sh
set -euo pipefail
cd "$(dirname "$0")/../../third_party/kontur"

: "${KONTUR_OCI_IMAGE:?set KONTUR_OCI_IMAGE to the full image reference to build and push, e.g. us-central1-docker.pkg.dev/<project>/<repo>/kontur:latest}"

if ! command -v docker >/dev/null 2>&1; then
  echo "build-oci-image.sh: docker not found" >&2
  exit 1
fi

echo "building ${KONTUR_OCI_IMAGE} from $(pwd)"
# The Dockerfile uses `RUN --mount=type=cache`, which only the BuildKit
# builder understands -- the classic builder fails outright on it ("the
# --mount option requires BuildKit").
DOCKER_BUILDKIT=1 docker build -t "$KONTUR_OCI_IMAGE" .

# KONTUR_OCI_SKIP_PUSH=1 stops here, leaving the image built but only in
# this host's own local docker image store -- exactly what a deployment
# building its own image on the target host itself (rather than pushing
# it somewhere for that host to pull back down again) needs, and nothing
# else in this script's contract changes either way.
if [ "${KONTUR_OCI_SKIP_PUSH:-0}" = "1" ]; then
  echo "built: ${KONTUR_OCI_IMAGE} (KONTUR_OCI_SKIP_PUSH=1 -- not pushed)"
  exit 0
fi

echo "pushing ${KONTUR_OCI_IMAGE}"
docker push "$KONTUR_OCI_IMAGE"

echo "published: ${KONTUR_OCI_IMAGE}"
