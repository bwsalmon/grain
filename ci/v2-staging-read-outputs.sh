#!/usr/bin/env bash
# Publish the v2 staging Terraform outputs the rest of the deploy
# workflow reads as GitHub Actions step outputs.
#
# Every value a later step reads as steps.<id>.outputs.<name> is written
# here. Adding a consumer without adding it here is the bug that made a
# v1 host come up with no GCP access at all (bwsalmon/agents#69): the
# expression resolved to empty rather than failing, so the step that
# minted and pushed the key was silently skipped. minter_service_account
# is exactly that shape again here -- push-secrets.sh mints no minter key
# at all when it is empty, and the gcp-key and gemini-key capabilities
# then have no credential to mint with.
#
# Required env:
#   GITHUB_OUTPUT  set by the Actions runner
set -euo pipefail

github_output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set (is this running outside Actions?)}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/terraform/gcp-v2"

# Read null-safe: `terraform output -raw` errors rather than printing
# nothing on a null, and agent_service_account and minter_service_account
# are both null whenever no agent account is configured.
out() { terraform output -raw "$1" 2>/dev/null || true; }

{
  echo "project_id=$(out project_id)"
  echo "instance=$(out instance_name)"
  echo "zone=$(out zone)"
  echo "url=$(out url)"
  # Empty exactly when expose_ui_publicly is off, which is how the
  # summary decides whether to print a URL or the tunnel command.
  echo "tunnel_command=$(out tunnel_command)"
  echo "agent_service_account=$(out agent_service_account)"
  echo "minter_service_account=$(out minter_service_account)"
} >> "$github_output"
