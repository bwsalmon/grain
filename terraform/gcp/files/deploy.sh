#!/usr/bin/env bash
# Fetched from instance metadata and run by config-sync.sh every time
# grain-deploy-generation changes. Translates this deployment's
# non-secret configuration (the grain-config metadata attribute) and its
# two secrets (also metadata, but never Terraform inputs -- see
# ../deploy/push-secrets.sh) into a call to scripts/setup.sh, which does the
# actual clone/pull/install/restart -- including the breaking-schema
# reformat (bwsalmon/agents#394; see that script's own step 6).
#
# "pull", not "build", since bwsalmon/agents#645: the deployment is a
# container image CI publishes on every commit, so this host no longer
# carries a toolchain or spends a deploy's minutes compiling one.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly SRC_DIR="/opt/grain"
readonly SECRET_DIR="/run/grain-deploy"

log() { echo "grain-deploy: $*"; }

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

# Empty (not a failure) when the attribute was never set -- both secret
# attributes are optional until push-secrets.sh has run at least once.
md_optional() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1" 2>/dev/null || true; }

# make is gone from this list (bwsalmon/agents#645): nothing on this host
# builds grain any more -- scripts/setup.sh pulls the image CI
# published for this commit -- so the Go/Node toolchain, and the `make`
# that used to drive it, are CI's problem rather than a deployed host's.
#
# What is left is what this deploy itself runs on: git (the checkout
# above, which is how a copy of setup.sh gets onto this host at all),
# docker (grain-daemon.service *is* a `docker run` now, so this is a
# runtime dependency, not only a deploy-time one), and jq (the `cfg`
# helper below). jq replaced a python3 that was installed on every host
# for three JSON one-liners and nothing else; two of those three were in
# setup.sh, which needs neither jq nor git any more (see its own header,
# "What this host has to have"), so what is left here is this script's
# own list rather than the deployment's.
install_prerequisites() {
  local missing=0
  for cmd in git docker jq; do
    command -v "$cmd" >/dev/null 2>&1 || missing=1
  done
  if [ "$missing" -eq 0 ]; then
    return
  fi
  log "installing git, docker, jq (needed once; the deploy below clones with one, runs grain with the next, and reads its own config with the last)"
  apt-get update
  apt-get install -y --no-install-recommends git docker.io jq ca-certificates
}

# Ship this host's systemd journal to Cloud Logging, so a failed deploy
# can be read without a shell on the box. The host service account
# already carries roles/logging.logWriter (iam.tf) for exactly this;
# until now nothing used it.
#
# Runs here rather than in startup.sh, and early rather than late, for
# one reason: the failures worth reading happen *below this line*. A
# startup-script install would not take effect until the next boot, and
# a call at the end of this file would never run on the deploys that
# need it. It re-runs on every generation and is retried by
# config-sync's own wake-up loop, so a transient apt or network failure
# heals on its own.
#
# Never fatal. A diagnostic that can abort the deploy it exists to
# debug is the wrong trade -- the same rule terraform/gcp's own
# ensure_ops_agent follows.
ensure_ops_agent() {
  if ! dpkg -s google-cloud-ops-agent >/dev/null 2>&1; then
    log "installing google-cloud-ops-agent"
    local script="/tmp/add-google-cloud-ops-agent-repo.sh"
    if ! curl -fsS -o "$script" https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh \
       || ! bash "$script" --also-install; then
      log "WARNING: could not install google-cloud-ops-agent; this host's logs will not reach Cloud Logging"
    fi
    rm -f "$script"
  fi
  dpkg -s google-cloud-ops-agent >/dev/null 2>&1 || return 0

  # The whole journal, filtered at query time rather than here --
  # upstream offers no unit filter on the receiver. In the Logs
  # Explorer:
  #   jsonPayload._SYSTEMD_UNIT="grain-config-sync.service"
  # There is no second receiver for a nested guest's console, unlike
  # v1: this host runs the daemon directly, so its own journal is the
  # whole story.
  install -d -m 0755 /etc/google-cloud-ops-agent
  cat > /etc/google-cloud-ops-agent/config.yaml <<'YAML'
logging:
  receivers:
    journald:
      type: systemd_journald
  service:
    pipelines:
      default_pipeline:
        receivers: [journald]
YAML

  if systemctl restart google-cloud-ops-agent; then
    log "google-cloud-ops-agent configured; this host's journal now reaches Cloud Logging"
  else
    log "WARNING: could not restart google-cloud-ops-agent; Cloud Logging will not reflect this config"
  fi
}

# Before anything below, because `cfg` shells out to jq and this is what
# guarantees jq exists. It used to run after the block that reads the
# config, which meant every cfg call on a fresh host ran against a parser
# that might not be installed yet -- and under `set -e` the first one took
# the whole deploy down with status 127, reported by config-sync only as
# "exit=127" with nothing naming the missing command.
install_prerequisites

# Immediately after, so everything below this line is readable off-host
# if it fails. Guarded: ensure_ops_agent is written never to fail, and
# `|| true` makes that true even of a bug in it.
ensure_ops_agent || true

# --- read this deployment's configuration off the instance's own metadata --

CONFIG_JSON="$(md instance/attributes/grain-config)"
# An absent key and a null one both read as empty, which is what every
# caller below treats as "not configured". Note that a JSON boolean comes
# out in its JSON spelling ("true"), not Python's ("True") -- see
# GRAIN_KONTUR_ENABLE below, which is the one caller that compares one.
cfg() {
  printf '%s' "$CONFIG_JSON" \
    | jq -r --arg k "$1" 'if (has($k) | not) or (.[$k] == null) then "" else .[$k] end'
}

GRAIN_REPO_URL="$(cfg grain_repo_url)"
GRAIN_REF="$(cfg grain_ref)"
GRAIN_IMAGE="$(cfg grain_image)"
GRAIN_IMAGE_TAG="$(cfg grain_image_tag)"
GRAIN_IMAGE_PULL_USER="$(cfg grain_image_pull_user)"
GITHUB_HOST="$(cfg github_host)"
CREDENTIAL_NAME="$(cfg credential_name)"
DEFAULT_TARGET_REPO="$(cfg default_target_repo)"
TARGET_REPOS="$(cfg target_repos)"
UI_PORT="$(cfg ui_port)"
SLOTS="$(cfg slots)"
POLL_INTERVAL="$(cfg poll_interval)"
# Where this deployment's database lives as text (pkg/staterepo).
# setup.sh writes these into <data-dir>/state-repo.json, so a host that
# has just been rebuilt is pointed at its own repository by the deploy
# rather than by a human opening the UI's bootstrap pane.
STATE_REPO_URL="$(cfg state_repo_url)"
STATE_REPO_BRANCH="$(cfg state_repo_branch)"
GEMINI_MODEL="$(cfg gemini_model)"
CLAUDE_MODEL="$(cfg claude_model)"
CODEX_MODEL="$(cfg codex_model)"
# Where the Antigravity CLI lives, if not on $PATH -- see setup.sh's own
# GRAIN_AGY_PATH and verify_agent_cli.
AGY_PATH="$(cfg agy_path)"
CODEX_PATH="$(cfg codex_path)"
MAX_AGENT_TURNS="$(cfg max_agent_turns)"
GCP_PROJECT="$(cfg gcp_project)"
GCP_AGENT_SERVICE_ACCOUNT="$(cfg gcp_agent_service_account)"

ENABLE_KONTUR_SANDBOXES="$(cfg enable_kontur_sandboxes)"
KONTUR_OCI_IMAGE="$(cfg kontur_oci_image)"
KONTUR_SSH_USER="$(cfg kontur_ssh_user)"
KONTUR_WORKSPACE="$(cfg kontur_workspace)"
KONTUR_BASE_IP="$(cfg kontur_base_ip)"
KONTUR_BASE_PORT="$(cfg kontur_base_port)"

# --- clone or update the checkout this script runs setup.sh out of -----
#
# Kept current here, rather than left to setup.sh to update itself
# mid-run the way it used to (its own sync_repo, and the re-exec that
# went with it). setup.sh no longer clones anything at all -- it needs
# nothing on a host but docker and systemd, and takes the source it does
# need out of the deployment image -- so this checkout exists for one
# reason: to be where this deploy finds a copy of setup.sh. Which makes
# keeping it in step with GRAIN_REF the job of whatever put it there,
# i.e. this script, which is itself refetched from instance metadata by
# config-sync.sh on every deploy.
#
# A hard reset, not a pull: nothing is meant to edit this checkout on a
# deployed host, so a local change here is either a mistake or an
# operator's in-progress debugging, and neither should be able to pin a
# fleet host to a stale setup.sh.
#
# git 2.35.2+ refuses to operate on a repository it does not own
# ("detected dubious ownership in repository at ..."), and this checkout
# can be one: root clones it, and an older deployment may have left it
# owned by the grain account instead. Exempted once, globally, and
# guarded so re-runs do not pile up duplicate entries.
git config --global --get-all safe.directory 2>/dev/null | grep -qxF "$SRC_DIR" \
  || git config --global --add safe.directory "$SRC_DIR"

if [ ! -d "$SRC_DIR/.git" ]; then
  log "cloning $GRAIN_REPO_URL ($GRAIN_REF) into $SRC_DIR"
  mkdir -p "$(dirname "$SRC_DIR")"
  git clone --quiet --branch "$GRAIN_REF" "$GRAIN_REPO_URL" "$SRC_DIR"
else
  log "updating $SRC_DIR to $GRAIN_REF"
  git -C "$SRC_DIR" fetch --quiet origin "$GRAIN_REF"
  git -C "$SRC_DIR" checkout --quiet "$GRAIN_REF"
  git -C "$SRC_DIR" reset --quiet --hard "origin/$GRAIN_REF"
fi

# --- secrets: read from metadata into a tmpfs directory, never a repo,
# never Terraform -- see push-secrets.sh for how they got there ---------

install -d -m 0700 "$SECRET_DIR"
cleanup() { rm -rf "$SECRET_DIR"; }
trap cleanup EXIT

GITHUB_TOKEN="$(md_optional instance/attributes/grain-github-token)"
GITHUB_APP_ID="$(md_optional instance/attributes/grain-github-app-id)"
GITHUB_APP_INSTALLATION_ID="$(md_optional instance/attributes/grain-github-app-installation-id)"
GITHUB_APP_PRIVATE_KEY="$(md_optional instance/attributes/grain-github-app-private-key)"
GEMINI_API_KEY="$(md_optional instance/attributes/grain-gemini-api-key)"
CLAUDE_OAUTH_TOKEN="$(md_optional instance/attributes/grain-claude-oauth-token)"
OPENAI_API_KEY="$(md_optional instance/attributes/grain-openai-api-key)"
# The secrets private key (pkg/secrets), and the one value here whose
# absence a *rebuild* pays for rather than the first deploy: the
# encrypted secrets file travels in the state repository, so a host that
# mints itself a fresh key cannot read the secrets its own repository
# still holds. Seeded once by setup.sh -- a key already on the host
# always wins, since it is the key that host's secrets were encrypted
# to -- and normally unset on a first deploy, where the host minting its
# own is exactly right.
SECRETS_KEY="$(md_optional instance/attributes/grain-secrets-key)"
# Only needed for a private image package; ghcr.io/bwsalmon/grain's is
# public and pulls anonymously (variables.tf's own
# grain_image_pull_user).
IMAGE_PULL_TOKEN="$(md_optional instance/attributes/grain-image-pull-token)"

MINTER_KEY_FILE=""
minter_key_json="$(md_optional instance/attributes/grain-gcp-minter-key)"
if [ -n "$minter_key_json" ]; then
  MINTER_KEY_FILE="$SECRET_DIR/minter-key.json"
  umask 077
  printf '%s' "$minter_key_json" > "$MINTER_KEY_FILE"
fi

if [ -z "$GITHUB_TOKEN" ] && [ -z "$GITHUB_APP_ID" ]; then
  log "no grain-github-token or grain-github-app-id in instance metadata yet -- deploying with no GitHub credential; run push-secrets.sh once one is ready"
fi
if [ -z "$GEMINI_API_KEY" ] && [ -n "$MINTER_KEY_FILE" ]; then
  log "no grain-gemini-api-key in instance metadata -- setup.sh will mint the daemon's own key with the minter credential (terraform/gcp README, \"The daemon's own Gemini key\")"
elif [ -z "$GEMINI_API_KEY" ] && [ -z "$CLAUDE_OAUTH_TOKEN" ] && [ -z "$OPENAI_API_KEY" ]; then
  log "no agent credential in instance metadata (grain-gemini-api-key, grain-claude-oauth-token, grain-openai-api-key) and no minter key either -- grain-daemon.service will run and serve the UI, but no run can dispatch until an agent credential is set there (Settings -> Agent frameworks) or pushed with push-secrets.sh"
fi

# What this deploy is about to hand setup.sh, before handing it over.
# Presence for the three secrets, values for the rest -- none of the
# latter is sensitive, and every one of them has cost a debugging session
# by being empty for a reason nothing printed: an unset gcp_project
# silently skips the Gemini mint, an empty target_repos parks every task,
# and a missing GitHub token leaves a sandbox with nothing cloned into
# it.
log "config for this generation:"
log "  grain_ref=$GRAIN_REF ui_port=$UI_PORT slots=$SLOTS poll_interval=$POLL_INTERVAL"
log "  image=${GRAIN_IMAGE}:${GRAIN_IMAGE_TAG:-<follows grain_ref>}" \
    "| pull credential: $([ -n "$IMAGE_PULL_TOKEN" ] && echo present || echo 'absent, pulling anonymously')"
log "  target_repos=${TARGET_REPOS:-<empty: every task parks>}"
log "  default_target_repo=${DEFAULT_TARGET_REPO:-<empty: a task with no repo parks>}"
log "  state_repo_url=${STATE_REPO_URL:-<empty: state stays in a local-only repository on the host>}" \
    "branch=$STATE_REPO_BRANCH" \
    "| secrets key: $([ -n "$SECRETS_KEY" ] && echo 'pushed, seeded if this host has none' || echo 'absent, the host mints its own')"
log "  gcp_project=${GCP_PROJECT:-<empty: gcp-key and gemini-key are disabled>}"
log "  gcp_agent_service_account=${GCP_AGENT_SERVICE_ACCOUNT:-<empty>}"
log "  github token: $([ -n "$GITHUB_TOKEN" ] && echo present || echo absent)" \
    "| github app: $([ -n "$GITHUB_APP_ID" ] && echo present || echo absent)" \
    "| gemini key: $([ -n "$GEMINI_API_KEY" ] && echo present || echo 'absent, will mint')" \
    "| claude token: $([ -n "$CLAUDE_OAUTH_TOKEN" ] && echo present || echo absent)" \
    "| openai key: $([ -n "$OPENAI_API_KEY" ] && echo present || echo absent)" \
    "| minter key: $([ -n "$MINTER_KEY_FILE" ] && echo present || echo MISSING)"
log "  enable_kontur_sandboxes=$ENABLE_KONTUR_SANDBOXES kontur_oci_image=${KONTUR_OCI_IMAGE:-<empty>}"

# --- run scripts/setup.sh, which does everything else --------------
#
# GRAIN_UI_ADDR binds 0.0.0.0, not setup.sh's own loopback-only default
# -- see variables.tf's ui_port for why that is safe
# here specifically (the firewall only admits Google's own load-balancer
# ranges on this port).
#
# GRAIN_ENABLE_UI_UPGRADE=0: this deployment shape already has its own
# rollout mechanism -- this very script, re-run by config-sync.sh
# whenever Terraform changes grain-deploy-generation -- so the UI's own
# Upgrade button (bwsalmon/agents#396) stays disabled here. Leaving both
# live at once would let an operator's UI click race, or silently drift
# out of sync with, a `terraform apply` that changes grain_ref
# (bwsalmon/agents#405).

env \
  GRAIN_REF="$GRAIN_REF" \
  GRAIN_IMAGE="$GRAIN_IMAGE" \
  GRAIN_IMAGE_TAG="$GRAIN_IMAGE_TAG" \
  GRAIN_IMAGE_PULL_USER="$GRAIN_IMAGE_PULL_USER" \
  GRAIN_IMAGE_PULL_TOKEN="$IMAGE_PULL_TOKEN" \
  GRAIN_UI_ADDR="0.0.0.0:${UI_PORT}" \
  GRAIN_ENABLE_UI_UPGRADE=0 \
  GRAIN_SLOTS="$SLOTS" \
  GRAIN_POLL_INTERVAL="$POLL_INTERVAL" \
  GRAIN_GITHUB_HOST="$GITHUB_HOST" \
  GRAIN_GITHUB_TOKEN="$GITHUB_TOKEN" \
  GRAIN_GITHUB_CREDENTIAL_NAME="$CREDENTIAL_NAME" \
  GRAIN_GITHUB_APP_ID="$GITHUB_APP_ID" \
  GRAIN_GITHUB_APP_INSTALLATION_ID="$GITHUB_APP_INSTALLATION_ID" \
  GRAIN_GITHUB_APP_PRIVATE_KEY="$GITHUB_APP_PRIVATE_KEY" \
  GRAIN_GEMINI_API_KEY="$GEMINI_API_KEY" \
  GRAIN_AGY_PATH="$AGY_PATH" \
  GRAIN_CLAUDE_CODE_OAUTH_TOKEN="$CLAUDE_OAUTH_TOKEN" \
  GRAIN_OPENAI_API_KEY="$OPENAI_API_KEY" \
  GRAIN_CODEX_PATH="$CODEX_PATH" \
  GRAIN_GEMINI_MODEL="$GEMINI_MODEL" \
  GRAIN_CLAUDE_MODEL="$CLAUDE_MODEL" \
  GRAIN_CODEX_MODEL="$CODEX_MODEL" \
  GRAIN_MAX_AGENT_TURNS="$MAX_AGENT_TURNS" \
  GRAIN_GCP_PROJECT="$GCP_PROJECT" \
  GRAIN_GCP_SERVICE_ACCOUNT_EMAIL="$GCP_AGENT_SERVICE_ACCOUNT" \
  GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE="$MINTER_KEY_FILE" \
  GRAIN_STATE_REPO_URL="$STATE_REPO_URL" \
  GRAIN_STATE_REPO_BRANCH="$STATE_REPO_BRANCH" \
  GRAIN_SECRETS_KEY="$SECRETS_KEY" \
  GRAIN_TARGET_REPO="$DEFAULT_TARGET_REPO" \
  GRAIN_TARGET_REPOS="$TARGET_REPOS" \
  GRAIN_KONTUR_ENABLE="$([ "$ENABLE_KONTUR_SANDBOXES" = "true" ] && echo 1 || echo 0)" \
  GRAIN_KONTUR_OCI_IMAGE="$KONTUR_OCI_IMAGE" \
  GRAIN_KONTUR_SSH_USER="$KONTUR_SSH_USER" \
  GRAIN_KONTUR_WORKSPACE="$KONTUR_WORKSPACE" \
  GRAIN_KONTUR_BASE_IP="$KONTUR_BASE_IP" \
  GRAIN_KONTUR_BASE_PORT="$KONTUR_BASE_PORT" \
  "$SRC_DIR/scripts/setup.sh"
