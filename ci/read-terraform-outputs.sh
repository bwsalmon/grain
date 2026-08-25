#!/usr/bin/env bash
# Publish the Terraform outputs the rest of the deploy workflow needs as
# GitHub Actions step outputs.
#
# Every value a later step reads as steps.<id>.outputs.<name> is written
# here. Adding a consumer without adding it here is the bug that made a
# host come up with no GCP access at all (bwsalmon/agents#69): the
# expression resolved to empty rather than failing, so the block that
# minted and pushed the agent key was silently skipped.
#
# Required env:
#   GITHUB_OUTPUT  set by the Actions runner
set -euo pipefail

github_output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set (is this running outside Actions?)}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/terraform/gcp"

{
  echo "project_id=$(terraform output -raw project_id)"
  echo "instance=$(terraform output -raw instance_name)"
  echo "zone=$(terraform output -raw zone)"
  # agent_service_account is null (not just absent) whenever no agent
  # account is configured -- `-raw` errors on a null value, so that case
  # has to fall back to empty rather than aborting the whole step, the
  # same way write-deploy-summary.sh's null-safe outputs do.
  echo "agent_service_account=$(terraform output -raw agent_service_account 2>/dev/null || true)"
} >> "$github_output"
