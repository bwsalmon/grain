#!/usr/bin/env bash
# Builds kontur from this repo's Dockerfile and pushes it to the local
# registry started by local-registry.sh, so the containerd configured by
# install.sh (which mirrors localhost:5000, see containerd-config.toml)
# can pull it without anything leaving the machine. Run local-registry.sh
# first.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
registry="${REGISTRY:-localhost:5000}"
tag="${TAG:-latest}"

echo "==> building kontur"
docker build -t "${registry}/kontur:${tag}" "${repo_root}"

echo "==> pushing to ${registry}"
docker push "${registry}/kontur:${tag}"
