#!/usr/bin/env bash
# Starts a local, unauthenticated image registry on localhost:5000. Both
# `docker push` (from build-and-push.sh) and containerd's CRI registry
# mirror (see containerd-config.toml) point at it, so kontur images never
# need to round-trip through ghcr.io just to test a change locally.
#
# Requires Docker on this machine (only to run the registry image and, in
# build-and-push.sh, to build kontur's own images) -- separate from the
# containerd install.sh sets up for kubelet, which is unaffected by this.
set -euo pipefail

name="kontur-local-registry"

if docker inspect "${name}" >/dev/null 2>&1; then
  echo "==> ${name} already exists, starting if needed"
  docker start "${name}" >/dev/null
else
  echo "==> starting ${name} on localhost:5000"
  docker run -d --name "${name}" --restart unless-stopped \
    -p 127.0.0.1:5000:5000 \
    -v kontur-local-registry-data:/var/lib/registry \
    registry:2 >/dev/null
fi

echo "registry ready at localhost:5000"
