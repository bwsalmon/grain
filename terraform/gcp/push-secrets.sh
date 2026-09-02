#!/usr/bin/env bash
# Push this deployment's secrets into the host instance's own metadata,
# and mint a fresh minter service account key for this run.
#
# Mirrors the mechanism v1's own ci/push-host-secrets.sh used: the
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
# history: the same rule pkg/secrets's own `grain secrets set`
# follows for -value-file/stdin.
#
# Required env:
#   PROJECT, INSTANCE, ZONE            terraform output project_id/instance_name/zone
# Optional env (empty leaves the host's existing copy untouched):
#   GRAIN_GITHUB_TOKEN                 the scoped PAT for this deployment's test_repos
#   GRAIN_GITHUB_APP_ID                 a GitHub App's own ID, together with
#   GRAIN_GITHUB_APP_INSTALLATION_ID    its installation ID on test_repos, and
#   GRAIN_GITHUB_APP_PRIVATE_KEY        its downloaded PEM private key -- an
#                                      alternative to GRAIN_GITHUB_TOKEN, stored under the
#                                      same credential_name, that pkg/gitproxy.CredentialSet
#                                      mints a refreshing installation token from instead of
#                                      reading as a bare PAT -- unlike a fine-grained PAT, it
#                                      can read the Checks API, which is what auto-merge
#                                      needs from a non-Actions CI provider. See this
#                                      directory's README, "There is no Checks permission to
#                                      grant". Registering the App itself is still a manual,
#                                      browser-based step on GitHub's own side; only pushing
#                                      the resulting three values here is automated. Set all
#                                      three together, or none -- scripts/setup.sh ignores
#                                      a partial set
#   GRAIN_GEMINI_API_KEY               the daemon's own operating key (pkg/agent/gemini) --
#                                      distinct from the gemini-key *capability*, which
#                                      mints its own short-lived keys per task once this
#                                      one gets the daemon running at all. Optional: with
#                                      enable_gemini_key on, the host mints this key for
#                                      itself from the minter credential pushed below --
#                                      see this directory's README, "The daemon's own
#                                      Gemini key". Set it only to use a key of your own.
#   GRAIN_CLAUDE_CODE_OAUTH_TOKEN      the Claude Code OAuth token agent/claude
#                                      authenticates as, for a deployment whose agent
#                                      framework is (or may be set to) "claude" -- the
#                                      counterpart of GRAIN_GEMINI_API_KEY above. Optional,
#                                      and so is that one: both credentials can be pasted
#                                      into the UI instead (Settings -> Agent frameworks),
#                                      which stores them in the host's own secrets database
#                                      and takes precedence over whatever is pushed here.
#                                      Push one to have a deployment come up already able to
#                                      dispatch, without anyone opening the UI first.
#   GRAIN_IMAGE_PULL_TOKEN             the password half of a `docker login` against the
#                                      registry grain_image lives in, for a deployment
#                                      running a private image (a GitHub PAT with
#                                      read:packages, for a private GHCR package). Optional
#                                      and normally unset: ghcr.io/bwsalmon/grain's own
#                                      package is public, and the host pulls anonymously.
#                                      The username half is not a secret and is a Terraform
#                                      input instead (grain_image_pull_user)
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
push_secret "grain-github-app-id" "${GRAIN_GITHUB_APP_ID:-}"
push_secret "grain-github-app-installation-id" "${GRAIN_GITHUB_APP_INSTALLATION_ID:-}"
push_secret "grain-github-app-private-key" "${GRAIN_GITHUB_APP_PRIVATE_KEY:-}"
push_secret "grain-gemini-api-key" "${GRAIN_GEMINI_API_KEY:-}"
push_secret "grain-claude-oauth-token" "${GRAIN_CLAUDE_CODE_OAUTH_TOKEN:-}"
# Only for a deployment whose grain_image lives in a registry that needs
# credentials -- ghcr.io/bwsalmon/grain's own package is public and pulls
# anonymously, so this is empty (and skipped) in the ordinary case. The
# username half is not a secret and is a Terraform input
# (grain_image_pull_user); this is the token.
push_secret "grain-image-pull-token" "${GRAIN_IMAGE_PULL_TOKEN:-}"

# The credential pkg/capability/gcpkey authenticates as to mint (and
# revoke) the agent account's per-task keys. Minted fresh on every run of
# this script rather than once -- iam.tf's deployer_manages_minter_keys
# is what lets the identity running this script do so -- and the two
# oldest keys beyond the newest are invalidated afterward, the same
# rotate-and-prune-to-two schedule ci/push-host-secrets.sh already uses
# for v1's own minter key, so a stale credential from a previous run
# does not linger indefinitely.
#
# scripts/setup.sh's own seed_gcp_minter_key only ever seeds the
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
