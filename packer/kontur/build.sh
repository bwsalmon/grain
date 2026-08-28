#!/bin/bash
# Builds packer/kontur/image.pkr.hcl and, if KONTUR_IMAGE_BUCKET is set,
# publishes the result. See README.md in this directory for the full
# picture; this script is deliberately just the mechanical wrapper Packer
# itself doesn't provide out of the box: an ephemeral build-time keypair and
# NoCloud seed (Packer's qemu builder needs some way to reach the VM it
# just booted from a stock cloud image -- see README.md, "How the build
# reaches the VM at all"), and a publish step matching this repo's existing
# GCS conventions (terraform/gcp/versions.tf's own "the bucket name lives in
# the repo as configuration, not here").
set -euo pipefail
cd "$(dirname "$0")"

: "${OPERATOR_SSH_PUBLIC_KEY:?set OPERATOR_SSH_PUBLIC_KEY to the operator SSH public key this image should carry (see README.md)}"

seed_dir="$(mktemp -d)"
trap 'rm -rf "$seed_dir"' EXIT

# A build-time-only keypair -- Packer's own access to the VM while
# provision.sh runs. Never the operator's key: provision.sh's last step
# overwrites this key out of the shipped image before Packer ever shuts the
# VM down.
ssh-keygen -t ed25519 -N "" -q -f "$seed_dir/packer_ssh_key"
packer_pubkey="$(cat "$seed_dir/packer_ssh_key.pub")"

instance_id="kontur-guest-build-$(date +%s)"
cat > "$seed_dir/meta-data" <<EOF
instance-id: ${instance_id}
local-hostname: kontur-guest-build
EOF
cat > "$seed_dir/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - ${packer_pubkey}
EOF

image_name="${IMAGE_NAME:-kontur-guest}"
version="$(git -C .. rev-parse --short HEAD 2>/dev/null || echo unknown)-$(date -u +%Y%m%d%H%M%S)"
output_directory="output/${image_name}-${version}"

packer init image.pkr.hcl
packer build \
  -var "operator_ssh_public_key=${OPERATOR_SSH_PUBLIC_KEY}" \
  -var "packer_ssh_private_key_file=${seed_dir}/packer_ssh_key" \
  -var "seed_dir=${seed_dir}" \
  -var "image_name=${image_name}" \
  -var "output_directory=${output_directory}" \
  image.pkr.hcl

built_image="${output_directory}/${image_name}.qcow2"
echo "built: ${built_image}"

if [ -n "${KONTUR_IMAGE_BUCKET:-}" ]; then
  dest="gs://${KONTUR_IMAGE_BUCKET}/kontur-guest/${image_name}-${version}.qcow2"
  gsutil cp "${built_image}" "${dest}"
  echo "published: ${dest}"
else
  echo "KONTUR_IMAGE_BUCKET not set -- not published, image left at ${built_image}"
fi
