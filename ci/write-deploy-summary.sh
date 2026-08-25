#!/usr/bin/env bash
# Write the rollout summary table into the Actions run summary.
#
# Runs with `if: always()`, so every `terraform output` here is null-safe:
# the point is to say as much as can be said about a failed rollout, and a
# deploy that failed before or during apply has no outputs to read.
#
# Required env:
#   DEPLOY_GENERATION
#   GITHUB_STEP_SUMMARY  set by the Actions runner
set -euo pipefail

deploy_generation="${DEPLOY_GENERATION:?DEPLOY_GENERATION is not set}"
step_summary="${GITHUB_STEP_SUMMARY:?GITHUB_STEP_SUMMARY is not set: is this running outside Actions?}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/terraform/gcp"

out() {  # a terraform output, or "unknown" if there is nothing to read
  terraform output -raw "$1" 2>/dev/null || echo unknown
}

{
  echo "### grain rollout"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| generation | \`${deploy_generation}\` |"
  echo "| instance | \`$(out instance_name)\` |"
  echo "| host service account | \`$(out host_service_account)\` |"
  echo "| ssh | \`$(out ssh_command)\` |"
} >> "$step_summary"
