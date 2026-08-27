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
#                             --github-key` wants. Superseded, but still
#                             read and merged, by GITHUB_SECRETS_JSON below
#                             (bwsalmon/agents#187) -- a deployment that
#                             already has one of these keeps working
#                             untouched.
#   GITHUB_SECRETS_JSON       `toJSON(secrets)` (bwsalmon/agents#187):
#                             every secret named `GRAIN_GITHUB_KEY_<NAME>`
#                             in it becomes a named credential `<name>`
#                             (the part after the prefix, lowercased) --
#                             adding or removing one is then adding or
#                             removing a single repo secret, never
#                             hand-editing a blob that also holds every
#                             other name's token. GitHub Actions has no way
#                             to select secrets by name pattern short of
#                             dumping the whole context; this script is
#                             already the one place a secret value is ever
#                             decrypted, so nothing new is trusted with it
#                             by receiving it.
#   GRAIN_CLAUDE_CODE_OAUTH_TOKEN
#   HOST_SERVICE_ACCOUNT      email of the host account -- the identity the
#                             controller mints agent keys as (agents#131)
set -euo pipefail

project="${PROJECT:?PROJECT is not set: the terraform project_id output}"
instance="${INSTANCE:?INSTANCE is not set: the terraform instance_name output}"
zone="${ZONE:?ZONE is not set: the terraform zone output}"
host_service_account="${HOST_SERVICE_ACCOUNT:-}"

# Empty is never a legitimate state: `host_service_account` is an
# unconditional Terraform output (the host always exists), so an empty
# value means the calling workflow simply did not pass it. That is a
# hand-edit every config repo owes this script whenever grain starts
# reading a new value -- deploy.yml is forked and owned per deployment,
# so the *values* side of bwsalmon/grain#78's split still drifts even
# though the logic side no longer does.
#
# Found the slow way (bwsalmon/agents#140): the minter key silently never
# got minted, the controller never got a credential, and every dispatch
# failed -- with nothing anywhere saying why, because the block below just
# skipped. Say so instead.
if [ -z "$host_service_account" ]; then
  echo "::warning::HOST_SERVICE_ACCOUNT is not set, so no minter key will be"
  echo "::warning::minted or pushed -- the controller cannot mint per-dispatch"
  echo "::warning::GCP keys and every dispatch will fail. Add"
  echo "::warning::  HOST_SERVICE_ACCOUNT: \${{ steps.tf.outputs.host_service_account }}"
  echo "::warning::to the 'Push secrets to the host' step in this repo's deploy.yml"
  echo "::warning::(grain's templates/gcp/ has the current version)."
fi

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

# Merges the legacy packed blob with any `GRAIN_GITHUB_KEY_<NAME>` secrets
# discovered in GITHUB_SECRETS_JSON into the single "NAME=TOKEN per line"
# blob deploy.sh on the host already knows how to split -- so the host
# side (bwsalmon/agents#134) needs no change at all for this. Per-secret
# entries are appended after the legacy blob's own lines, so one wins a
# name collision between the two mechanisms: whichever named the credential
# through its own dedicated secret, the newer and more direct of the two.
collect_github_keys() {
  python3 - "${GRAIN_GITHUB_KEYS:-}" <<'PY'
import json
import os
import sys

lines = [line for line in sys.argv[1].splitlines() if line.strip()]

prefix = "GRAIN_GITHUB_KEY_"
secrets = json.loads(os.environ.get("GITHUB_SECRETS_JSON") or "{}")
for key, value in secrets.items():
    if key.startswith(prefix) and value:
        lines.append(f"{key[len(prefix):].lower()}={value}")

print("\n".join(lines))
PY
}

push_secret "grain-github-token" "${GRAIN_GITHUB_TOKEN:-}"
push_secret "grain-github-keys" "$(collect_github_keys)"
push_secret "grain-claude-token" "${GRAIN_CLAUDE_CODE_OAUTH_TOKEN:-}"

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
if [ -n "$host_service_account" ]; then
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
