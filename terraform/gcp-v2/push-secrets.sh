#!/usr/bin/env bash
# Push this deployment's secrets into the host instance's own metadata,
# and mint a fresh minter service account key for this run.
#
# Mirrors ../../ci/push-host-secrets.sh's exact mechanism for v1: the
# only place a secret value is ever decrypted, going straight into the
# instance's own metadata over the Compute API -- never through Secret
# Manager, never through Terraform, so none of it is in the state file.
# The host reads it back locally over the metadata server, with no GCP
# credential of its own (files/deploy.sh's own md_optional).
#
# Run this once by hand after the first `terraform apply` (its outputs
# name PROJECT/INSTANCE/ZONE/MINTER_SERVICE_ACCOUNT for you), and again
# whenever a secret changes -- each run is a plain overwrite, safe to
# repeat.
#
# Secret values arrive as environment variables, deliberately never a
# command-line argument, where they would sit in a process list or shell
# history: the same rule v2/pkg/secrets's own `grain secrets set`
# follows for -value-file/stdin.
#
# Required env:
#   PROJECT, INSTANCE, ZONE            terraform output project_id/instance_name/zone
# Optional env (empty leaves the host's existing copy untouched):
#   GRAIN_GITHUB_TOKEN                 the scoped PAT for this deployment's test_repos
#   GRAIN_GEMINI_API_KEY               the daemon's own operating key (pkg/agent/gemini) --
#                                      distinct from the gemini-key *capability*, which
#                                      mints its own short-lived keys per task once this
#                                      one gets the daemon running at all
#   MINTER_SERVICE_ACCOUNT             terraform output minter_service_account -- if set,
#                                      mints a fresh key for it and pushes that too (see below)
set -euo pipefail

project="${PROJECT:?PROJECT is not set: the terraform project_id output}"
instance="${INSTANCE:?INSTANCE is not set: the terraform instance_name output}"
zone="${ZONE:?ZONE is not set: the terraform zone output}"
minter_service_account="${MINTER_SERVICE_ACCOUNT:-}"

push_secret() {
  local key="$1" value="$2" tmp
  if [ -z "$value" ]; then
    echo "$key: empty; leaving the host's copy untouched"
    return 0
  fi
  tmp="$(mktemp)"
  umask 077
  printf '%s' "$value" > "$tmp"
  gcloud compute instances add-metadata "$instance" \
    --project="$project" --zone="$zone" \
    --metadata-from-file="$key=$tmp" >/dev/null
  rm -f "$tmp"
  echo "$key: pushed"
}

push_secret "grain-github-token" "${GRAIN_GITHUB_TOKEN:-}"
push_secret "grain-gemini-api-key" "${GRAIN_GEMINI_API_KEY:-}"

# The credential pkg/capability/gcpkey authenticates as to mint (and
# revoke) the agent account's per-task keys. Minted fresh on every run of
# this script rather than once -- iam.tf's deployer_manages_minter_keys
# is what lets the identity running this script do so -- and the two
# oldest keys beyond the newest are invalidated afterward, the same
# rotate-and-prune-to-two schedule ci/push-host-secrets.sh already uses
# for v1's own minter key, so a stale credential from a previous run
# does not linger indefinitely.
#
# v2/scripts/setup.sh's own seed_gcp_minter_key only ever seeds the
# host's local secrets database once (it never overwrites an existing
# gcp-key-minter entry) -- so a *rotated* key reaching this metadata
# attribute does not by itself reach the running daemon. Clear it by
# hand first (`grain secrets delete gcp-key-minter key.json` on the
# host, over `gcloud compute ssh --tunnel-through-iap`) if you need a
# rotated key to actually take effect, then bump deploy_generation so
# config-sync re-runs setup.sh.
if [ -n "$minter_service_account" ]; then
  umask 077
  minter_file="$(mktemp)"
  minter_key_id="$(gcloud iam service-accounts keys create "$minter_file" \
    --iam-account="$minter_service_account" --format='value(name.basename())')"
  push_secret "grain-gcp-minter-key" "$(cat "$minter_file")"
  shred -u "$minter_file" 2>/dev/null || rm -f "$minter_file"
  echo "minted minter key: $minter_key_id"

  gcloud iam service-accounts keys list --iam-account="$minter_service_account" \
    --managed-by=user --sort-by='~validAfterTime' --format='value(name.basename())' \
    | tail -n +3 \
    | while read -r old_key_id; do
        gcloud iam service-accounts keys delete "$old_key_id" \
          --iam-account="$minter_service_account" --quiet
        echo "invalidated previous minter key: $old_key_id"
      done
else
  echo "MINTER_SERVICE_ACCOUNT is not set -- no minter key minted or pushed;" \
       "the gcp-key/gemini-key capabilities will have no credential to mint with."
fi
