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

# make is here because v2/scripts/setup.sh builds the binary with
# `make -C v2 container-build`. It is not in a Debian cloud image, and
# nothing else pulls it in -- the compile happens inside Docker, so make
# is the only part of that toolchain the host needs, and the easiest to
# overlook. Missing, the deploy died on `make: command not found`, which
# config-sync reported as a bare "exit=127".
install_prerequisites() {
  local missing=0
  for cmd in git docker python3 make; do
    command -v "$cmd" >/dev/null 2>&1 || missing=1
  done
  if [ "$missing" -eq 0 ]; then
    return
  fi
  log "installing git, docker, python3, make (needed once; v2/scripts/setup.sh's own build/install steps need them)"
  apt-get update
  apt-get install -y --no-install-recommends git docker.io python3 make ca-certificates
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
  #   jsonPayload._SYSTEMD_UNIT="grain-v2-config-sync.service"
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

# Before anything below, because `cfg` shells out to python3 and this is
# what guarantees python3 exists. It used to run after the block that
# reads the config, which meant every cfg call on a fresh host ran
# against an interpreter that might not be installed yet -- and under
# `set -e` the first one took the whole deploy down with status 127,
# reported by config-sync only as "exit=127" with nothing naming the
# missing command.
install_prerequisites

# Immediately after, so everything below this line is readable off-host
# if it fails. Guarded: ensure_ops_agent is written never to fail, and
# `|| true` makes that true even of a bug in it.
ensure_ops_agent || true

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
#
# GRAIN_ENABLE_UI_UPGRADE=0: this deployment shape already has its own
# rollout mechanism -- this very script, re-run by config-sync.sh
# whenever Terraform changes grain-deploy-generation -- so the UI's own
# Upgrade button (bwsalmon/agents#396) stays disabled here. Leaving both
# live at once would let an operator's UI click race, or silently drift
# out of sync with, a `terraform apply` that changes grain_ref
# (bwsalmon/agents#405).

env \
  GRAIN_REPO_URL="$GRAIN_REPO_URL" \
  GRAIN_REF="$GRAIN_REF" \
  GRAIN_SRC_DIR="$SRC_DIR" \
  GRAIN_UI_ADDR="0.0.0.0:${UI_PORT}" \
  GRAIN_ENABLE_UI_UPGRADE=0 \
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
