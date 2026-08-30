#!/usr/bin/env bash
# Fetched from instance metadata and run by config-sync.sh every time
# grain-deploy-generation changes. Translates this deployment's
# non-secret configuration (the grain-config metadata attribute) and its
# two secrets (also metadata, but never Terraform inputs -- see
# ../push-secrets.sh) into a call to v2/scripts/setup.sh, which does the
# actual clone/build/install/restart -- including the breaking-schema
# reformat (bwsalmon/agents#394; see that script's own step 6).
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly SRC_DIR="/opt/grain"
readonly SECRET_DIR="/run/grain-v2-deploy"

log() { echo "grain-v2-deploy: $*"; }

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

# Empty (not a failure) when the attribute was never set -- both secret
# attributes are optional until push-secrets.sh has run at least once.
md_optional() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1" 2>/dev/null || true; }

install_prerequisites() {
  if command -v git >/dev/null 2>&1 && command -v docker >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
    return
  fi
  log "installing git, docker, python3 (needed once; v2/scripts/setup.sh's own build/install steps need them)"
  apt-get update
  apt-get install -y --no-install-recommends git docker.io python3 ca-certificates
}

# --- read this deployment's configuration off the instance's own metadata --

CONFIG_JSON="$(md instance/attributes/grain-config)"
cfg() {
  python3 -c 'import json, sys; v = json.loads(sys.argv[2]).get(sys.argv[1], ""); print(v if v is not None else "")' "$1" "$CONFIG_JSON"
}

GRAIN_REPO_URL="$(cfg grain_repo_url)"
GRAIN_REF="$(cfg grain_ref)"
GITHUB_HOST="$(cfg github_host)"
CREDENTIAL_NAME="$(cfg credential_name)"
DEFAULT_TARGET_REPO="$(cfg default_target_repo)"
TARGET_REPOS="$(cfg target_repos)"
UI_PORT="$(cfg ui_port)"
SLOTS="$(cfg slots)"
POLL_INTERVAL="$(cfg poll_interval)"
GEMINI_MODEL="$(cfg gemini_model)"
GCP_PROJECT="$(cfg gcp_project)"
GCP_AGENT_SERVICE_ACCOUNT="$(cfg gcp_agent_service_account)"

install_prerequisites

# --- clone (once) or leave the update to setup.sh's own sync_repo -----

if [ ! -d "$SRC_DIR/.git" ]; then
  log "cloning $GRAIN_REPO_URL ($GRAIN_REF) into $SRC_DIR"
  mkdir -p "$(dirname "$SRC_DIR")"
  git clone --quiet --branch "$GRAIN_REF" "$GRAIN_REPO_URL" "$SRC_DIR"
fi

# --- secrets: read from metadata into a tmpfs directory, never a repo,
# never Terraform -- see push-secrets.sh for how they got there ---------

install -d -m 0700 "$SECRET_DIR"
cleanup() { rm -rf "$SECRET_DIR"; }
trap cleanup EXIT

GITHUB_TOKEN="$(md_optional instance/attributes/grain-github-token)"
GEMINI_API_KEY="$(md_optional instance/attributes/grain-gemini-api-key)"

MINTER_KEY_FILE=""
minter_key_json="$(md_optional instance/attributes/grain-gcp-minter-key)"
if [ -n "$minter_key_json" ]; then
  MINTER_KEY_FILE="$SECRET_DIR/minter-key.json"
  umask 077
  printf '%s' "$minter_key_json" > "$MINTER_KEY_FILE"
fi

if [ -z "$GITHUB_TOKEN" ]; then
  log "no grain-github-token in instance metadata yet -- deploying with no GitHub credential; run push-secrets.sh once it's ready"
fi
if [ -z "$GEMINI_API_KEY" ] && [ -n "$MINTER_KEY_FILE" ]; then
  log "no grain-gemini-api-key in instance metadata -- setup.sh will mint the daemon's own key with the minter credential (terraform/gcp-v2 README, \"The daemon's own Gemini key\")"
elif [ -z "$GEMINI_API_KEY" ]; then
  log "no grain-gemini-api-key in instance metadata and no minter key either -- grain-daemon.service will install but stay stopped; run push-secrets.sh once one of them is ready"
fi

# --- run v2/scripts/setup.sh, which does everything else --------------
#
# GRAIN_UI_ADDR binds 0.0.0.0, not setup.sh's own loopback-only default
# -- see terraform/gcp-v2/variables.tf's ui_port for why that is safe
# here specifically (the firewall only admits Google's own load-balancer
# ranges on this port).

env \
  GRAIN_REPO_URL="$GRAIN_REPO_URL" \
  GRAIN_REF="$GRAIN_REF" \
  GRAIN_SRC_DIR="$SRC_DIR" \
  GRAIN_UI_ADDR="0.0.0.0:${UI_PORT}" \
  GRAIN_SLOTS="$SLOTS" \
  GRAIN_POLL_INTERVAL="$POLL_INTERVAL" \
  GRAIN_GITHUB_HOST="$GITHUB_HOST" \
  GRAIN_GITHUB_TOKEN="$GITHUB_TOKEN" \
  GRAIN_GITHUB_CREDENTIAL_NAME="$CREDENTIAL_NAME" \
  GRAIN_GEMINI_API_KEY="$GEMINI_API_KEY" \
  GRAIN_GEMINI_MODEL="$GEMINI_MODEL" \
  GRAIN_GCP_PROJECT="$GCP_PROJECT" \
  GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="$GCP_AGENT_SERVICE_ACCOUNT" \
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="$MINTER_KEY_FILE" \
  GRAIN_TARGET_REPO="$DEFAULT_TARGET_REPO" \
  GRAIN_TARGET_REPOS="$TARGET_REPOS" \
  "$SRC_DIR/v2/scripts/setup.sh"
