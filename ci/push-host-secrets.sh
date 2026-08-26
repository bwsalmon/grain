#!/usr/bin/env bash
# Push the deployment secrets into the host instance own metadata, and
# mint a fresh agent service account key for this run.
#
# The only place a secret value is ever decrypted. It goes straight into
# the instance own metadata over the Compute API -- never through Secret
# Manager, so the deployer needs no project-wide secret access, and never
# through Terraform, so it is not in the state file. The host reads it
# back locally, with no GCP credential at all: metadata is local to the
# instance that owns it.
#
# The secret values arrive as environment variables because only the
# calling workflow can read the secrets context; nothing here is ever
# passed on a command line, where it would be visible in a process list.
#
# Required env:
#   PROJECT, INSTANCE, ZONE   from the Terraform outputs
# Optional env (empty leaves the host copy untouched):
#   GRAIN_GITHUB_TOKEN
#   GRAIN_GITHUB_KEYS         additional named credentials (bwsalmon/
#                             agents#134), one "NAME=TOKEN" pair per line;
#                             deploy.sh on the host splits these into the
#                             per-name files `grain host bootstrap
#                             --github-key` wants
#   GRAIN_CLAUDE_CODE_OAUTH_TOKEN
#   AGENT_SERVICE_ACCOUNT     email of the agent account, when configured
#   HOST_SERVICE_ACCOUNT      email of the host account -- the identity the
#                             controller mints agent keys as (agents#131)
set -euo pipefail

project="${PROJECT:?PROJECT is not set: the terraform project_id output}"
instance="${INSTANCE:?INSTANCE is not set: the terraform instance_name output}"
zone="${ZONE:?ZONE is not set: the terraform zone output}"
agent_service_account="${AGENT_SERVICE_ACCOUNT:-}"
host_service_account="${HOST_SERVICE_ACCOUNT:-}"

push_secret() {
  local key="$1" value="$2" tmp
  if [ -z "$value" ]; then
    echo "::warning::$key is empty; leaving the host copy untouched"
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
push_secret "grain-github-keys" "${GRAIN_GITHUB_KEYS:-}"
push_secret "grain-claude-token" "${GRAIN_CLAUDE_CODE_OAUTH_TOKEN:-}"

# Minted fresh every run, straight to instance metadata, never a repo
# secret: the short-lived-credential principle grain own docs/design.md
# argues for at the sandbox layer, applied one layer up to the
# impersonation source itself. Every key older than the previous run's is
# deleted right after -- otherwise a run that never gets read back (a host
# that is down, or a config-sync cycle that has not reached this
# generation yet) leaves an old key valid indefinitely, and GCP allows at
# most 10 per account.
#
# Keeping the *previous* run's key too (not just this run's), rather than
# deleting down to one, is deliberate: terraform-apply.sh bumps
# grain-deploy-generation, which is what wakes config-sync up, before this
# step ever runs -- so a host can race ahead, notice the new generation,
# and re-converge before the key minted below has even been pushed to
# metadata. deploy.sh's fetch_secret_to_file has no way to tell "this
# metadata value is stale" from "this metadata value is current"; it just
# reads whatever is there, which in that race is still the *previous*
# run's key. Deleting down to one key immediately used to delete exactly
# that key out from under the host mid-race, and since nothing re-triggers
# config-sync until the next generation bump, every gcloud call on that
# host then failed with `invalid_grant: Invalid JWT Signature` forever --
# not transient, and no retry on the host side could fix it (bwsalmon/
# agents#93). Keeping two generations of keys -- well inside the 10-key
# cap -- means a host that raced ahead stays valid through this run and
# picks up the correct key on the next one.
if [ -n "$agent_service_account" ]; then
  umask 077
  key_file="$(mktemp)"
  new_key_id="$(gcloud iam service-accounts keys create "$key_file" \
    --iam-account="$agent_service_account" --format='value(name.basename())')"
  push_secret "grain-agent-service-account-key" "$(cat "$key_file")"
  shred -u "$key_file" 2>/dev/null || rm -f "$key_file"
  echo "minted key: $new_key_id"

  gcloud iam service-accounts keys list --iam-account="$agent_service_account" \
    --managed-by=user --sort-by='~validAfterTime' --format='value(name.basename())' \
    | tail -n +3 \
    | while read -r old_key_id; do
        gcloud iam service-accounts keys delete "$old_key_id" \
          --iam-account="$agent_service_account" --quiet
        echo "invalidated previous key: $old_key_id"
      done
fi

# bwsalmon/agents#131: the credential grain/automation/gcp_keys.py
# authenticates as to mint the per-dispatch agent keys. It was written
# assuming the controller could use the host account's *native* GCE
# identity -- true of the host, false of the controller, which is a nested
# libvirt guest with no attached service account and no route to the
# metadata server. So the host account travels as a key file instead.
#
# Deliberately the host account and not the agent account: the agent must
# not be able to mint its own replacement, which is the entire premise of
# the 24-hour expiry (iam.tf grants the host serviceAccountKeyAdmin on the
# agent account, and not the reverse). Rotated on the same two-generation
# schedule and for the same reason as the agent key above.
if [ -n "$host_service_account" ] && [ -n "$agent_service_account" ]; then
  umask 077
  minter_file="$(mktemp)"
  minter_key_id="$(gcloud iam service-accounts keys create "$minter_file" \
    --iam-account="$host_service_account" --format='value(name.basename())')"
  push_secret "grain-key-minter-key" "$(cat "$minter_file")"
  shred -u "$minter_file" 2>/dev/null || rm -f "$minter_file"
  echo "minted minter key: $minter_key_id"

  gcloud iam service-accounts keys list --iam-account="$host_service_account" \
    --managed-by=user --sort-by='~validAfterTime' --format='value(name.basename())' \
    | tail -n +3 \
    | while read -r old_key_id; do
        gcloud iam service-accounts keys delete "$old_key_id" \
          --iam-account="$host_service_account" --quiet
        echo "invalidated previous minter key: $old_key_id"
      done
fi
