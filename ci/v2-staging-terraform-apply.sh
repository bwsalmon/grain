#!/usr/bin/env bash
# Initialise, validate and apply the v2 staging Terraform module
# (terraform/gcp-v2) against a config repo's own tfvars and backend.
#
# The v1 counterpart of this script is ci/terraform-apply.sh, and the
# reason there are two rather than one parameterised script is that the
# two modules take different variables: v1's takes config_repo, this one
# does not, and the fixture files live at different paths. Sharing them
# would mean a flag for every difference and a script that reads as a
# switch statement.
#
# Called from a config repo's deploy workflow, out of the grain checkout
# it makes -- so a config repo never encodes grain's layout and cannot
# drift from it. See terraform/gcp-v2/README.md.
#
# Required env:
#   CONFIG_DIR         absolute path to the directory holding backend.hcl
#                      and staging.tfvars
#   DEPLOY_GENERATION  the token the host watches for
# Optional env:
#   TFVARS_FILE            tfvars basename in CONFIG_DIR (default staging.tfvars)
#   TF_APPLY_MAX_ATTEMPTS  stock-out retries (default 5)
#   TF_APPLY_RETRY_DELAY   first backoff, seconds, doubling (default 60)
set -euo pipefail

config_dir="${CONFIG_DIR:?CONFIG_DIR is not set: the directory holding backend.hcl and the tfvars}"
deploy_generation="${DEPLOY_GENERATION:?DEPLOY_GENERATION is not set}"
tfvars_file="${TFVARS_FILE:-staging.tfvars}"
max_attempts="${TF_APPLY_MAX_ATTEMPTS:-5}"
delay="${TF_APPLY_RETRY_DELAY:-60}"

for f in backend.hcl "$tfvars_file"; do
  [ -f "$config_dir/$f" ] || {
    echo "::error::$config_dir/$f does not exist" >&2
    exit 1
  }
done

# grain's own root, two levels up from this file -- so the caller never
# names grain's internal layout and a move here breaks nothing downstream.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/terraform/gcp-v2"

echo "--- terraform init"
terraform init -input=false -backend-config="$config_dir/backend.hcl"

echo "--- terraform validate"
terraform validate -no-color

# GCP stock-outs (ZONE_RESOURCE_POOL_EXHAUSTED and its siblings) are a
# common, transient failure creating a VM or disk in a given zone -- a
# plain retry a few minutes later routinely succeeds once the zone frees
# up capacity, so back off and retry instead of failing the whole
# rollout. Any other failure still fails immediately: retrying a bad
# config or a real quota limit only burns the backoff budget. Same
# reasoning, and the same patterns, as ci/terraform-apply.sh.
apply() {
  terraform apply -input=false -auto-approve -no-color \
    -var-file="$config_dir/$tfvars_file" \
    -var="deploy_generation=$deploy_generation" 2>&1 | tee "$out"
}

echo "--- terraform apply"
out="$(mktemp)"
attempt=1
while true; do
  if apply; then
    exit 0
  fi
  if [ "$attempt" -ge "$max_attempts" ] \
     || ! grep -qiE 'RESOURCE_POOL_EXHAUSTED|does not have enough resources available' "$out"; then
    # A brand failure is worth naming: it is not something Terraform can
    # retry, and the fix is to stop asking for a brand at all rather than
    # to edit anything. IAP uses a Google-managed OAuth client when none
    # is configured, so create_iap_brand should normally be false with
    # iap_client_id/iap_client_secret unset.
    if grep -qi 'iap_brand\|OAuth client' "$out"; then
      echo "::error::This looks like an IAP OAuth client failure. A client is not" \
           "required: leave create_iap_brand false and iap_client_id/iap_client_secret" \
           "unset in $tfvars_file, and IAP uses its own Google-managed client. See" \
           "terraform/gcp-v2/README.md, \"No OAuth client needed\"."
    fi
    exit 1
  fi
  echo "::warning::terraform apply hit a stock-out (attempt $attempt/$max_attempts); retrying in ${delay}s"
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$((delay * 2))
done
