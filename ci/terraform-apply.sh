#!/usr/bin/env bash
# Initialise, validate and apply the terraform/gcp module against a
# config repo's own tfvars and backend.
#
# One script for every deployment of that module: bwsalmon/agents runs
# both a main and a staging one, and they differ only in which
# CONFIG_DIR and TFVARS_FILE they point at. Nothing here is per-
# deployment, which is why TFVARS_FILE has no default -- a wrong one
# would apply another deployment's configuration to this one's state.
#
# Called from a config repo's deploy workflow, out of the grain checkout
# it makes -- so a config repo never encodes grain's layout and cannot
# drift from it. See terraform/gcp/README.md.
#
# Required env:
#   CONFIG_DIR         absolute path to the directory holding backend.hcl
#                      and the tfvars file
#   TFVARS_FILE        tfvars basename within CONFIG_DIR
#   DEPLOY_GENERATION  the token the host watches for
# Optional env:
#   TF_APPLY_MAX_ATTEMPTS  stock-out retries (default 5)
#   TF_APPLY_RETRY_DELAY   first backoff, seconds, doubling (default 60)
set -euo pipefail

config_dir="${CONFIG_DIR:?CONFIG_DIR is not set: the directory holding backend.hcl and the tfvars}"
deploy_generation="${DEPLOY_GENERATION:?DEPLOY_GENERATION is not set}"
tfvars_file="${TFVARS_FILE:?TFVARS_FILE is not set: the tfvars basename within CONFIG_DIR}"
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
cd "$root/terraform/gcp"

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
#
# google_compute_instance.host's own metadata update (grain-deploy-generation,
# every apply that changes anything in grain_config -- see instance.tf) is a
# second source of a transient, retry-worthy failure, and not a GCP stock-out
# at all: the google provider retries that update itself, to absorb a 412
# where another update raced it for the instance's metadata fingerprint, but
# hardcodes the *whole* retry -- GET the instance, POST the new metadata, wait
# for the resulting operation to finish -- to a one-minute budget it never
# derives from this resource's own `timeouts` (confirmed in the pinned
# hashicorp/google ~> 6.8 provider's resourceComputeInstanceUpdate, which
# calls transport_tpg.Retry with no Timeout set, so transport_tpg.Retry's own
# default of 1*time.Minute applies regardless of how long a real update is
# allowed to take). Ordinary GCP latency can eat that minute with no fault of
# ours, and the provider then reports exactly "Error: timeout while waiting
# for state to become 'success' (timeout: 1m0s)" and abandons an update that
# was very likely about to succeed. A plain retry of the whole `terraform
# apply` is safe here the same way it is for a stock-out: this resource's
# state was never advanced, so the next attempt just retries the same
# SetMetadata call. See bwsalmon/agents#636.
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
     || ! grep -qiE "RESOURCE_POOL_EXHAUSTED|does not have enough resources available|timeout while waiting for state to become 'success' \\(timeout: 1m0s\\)" "$out"; then
    # Name only what a reader could not diagnose from the Terraform error
    # itself, and match on something that cannot appear in a *successful*
    # run's noise. An earlier version of this grepped for "iap_brand",
    # which the provider emits as a deprecation warning on every single
    # run whether or not anything failed -- so the hint fired on every
    # failure and talked over the real error underneath it.
    #
    # A 409 alreadyExists is the one worth a hint: in a project shared
    # with another grain deployment it means a resource name is claimed,
    # and the Terraform error names the resource but not the fix.
    if grep -q 'Error 409' "$out" && grep -qi 'already exists' "$out"; then
      echo "::error::A resource this deployment wants already exists in the project," \
           "which usually means another grain deployment there already owns that name." \
           "The Terraform error above names it. Set a distinct agent_account_id or" \
           "minter_account_id in $tfvars_file (or a distinct name_prefix) -- never" \
           "reuse the other deployment's account, which carries its grants, not this" \
           "one's. See terraform/gcp/README.md, \"Sharing a project\"."
    fi
    exit 1
  fi
  echo "::warning::terraform apply hit a transient failure (a stock-out, or the provider's own one-minute metadata-update timeout; attempt $attempt/$max_attempts); retrying in ${delay}s"
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$((delay * 2))
done
