#!/usr/bin/env bash
# Initialise, validate and apply the GCP Terraform module against a config
# repo's own tfvars and backend.
#
# Called from a config repo's .github/workflows/deploy.yml, out of the
# grain checkout it makes. The workflow supplies values (which config
# directory, which repo, which generation); everything about *how* to run
# Terraform -- including where the module lives -- is here, so a config
# repo never encodes grain's layout and cannot drift from it.
#
# Required env:
#   CONFIG_DIR         absolute path to the config repo's config/ directory
#                      (holds backend.hcl and grain.tfvars)
#   CONFIG_REPO        owner/repo of the config repo, passed as the
#                      config_repo Terraform variable
#   DEPLOY_GENERATION  the token the host watches for
# Optional env:
#   TF_APPLY_MAX_ATTEMPTS  stock-out retries (default 5)
#   TF_APPLY_RETRY_DELAY   first backoff, seconds, doubling (default 60)
set -euo pipefail

config_dir="${CONFIG_DIR:?CONFIG_DIR is not set: the config repo config directory}"
config_repo="${CONFIG_REPO:?CONFIG_REPO is not set (owner/repo of the config repo)}"
deploy_generation="${DEPLOY_GENERATION:?DEPLOY_GENERATION is not set}"
max_attempts="${TF_APPLY_MAX_ATTEMPTS:-5}"
delay="${TF_APPLY_RETRY_DELAY:-60}"

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
# up capacity, so back off and retry instead of failing the whole rollout
# on what is usually a non-issue. Any other failure (a bad config, a real
# quota limit, ...) still fails immediately -- retrying those would just
# waste the backoff budget.
#
# google_compute_instance.host's own metadata update (grain-deploy-generation,
# every apply that changes anything in grain_config -- see instance.tf) is a
# second source of a transient, retry-worthy failure, and not a GCP stock-out
# at all: the google provider retries that update itself, to absorb a 412
# where another update raced it for the instance's metadata fingerprint, but
# hardcodes the *whole* retry -- GET the instance, POST the new metadata, wait
# for the resulting operation to finish -- to a one-minute budget it never
# derives from this resource's own `timeouts` (confirmed in the pinned
# hashicorp/google provider's resourceComputeInstanceUpdate, which calls
# transport_tpg.Retry with no Timeout set, so transport_tpg.Retry's own
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
    -var-file="$config_dir/grain.tfvars" \
    -var="config_repo=$config_repo" \
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
    exit 1
  fi
  echo "::warning::terraform apply hit a transient failure (a stock-out, or the provider's own one-minute metadata-update timeout; attempt $attempt/$max_attempts); retrying in ${delay}s"
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$((delay * 2))
done
