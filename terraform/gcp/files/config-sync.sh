#!/usr/bin/env bash
# The rollout mechanism. Runs forever as a systemd service.
#
# Terraform writes a `grain-deploy-generation` attribute into instance
# metadata; CI sets it to the commit SHA of the config repo. This service
# hangs on the metadata server's wait_for_change endpoint, and every time
# that value changes it re-fetches deploy.sh from metadata and runs it.
# That is the whole "push to the config repo and it rolls out" path -- no
# inbound SSH, no runner with credentials, no agent polling GitHub.
#
# A failed deploy is retried on the next wake-up, so a transient failure
# (apt mirror, GitHub, a secret not pushed yet) self-heals without a
# second push.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly SELF="/opt/grain-deploy/config-sync.sh"
readonly DEPLOY="/opt/grain-deploy/deploy.sh"
readonly STATE="/var/lib/grain/.deploy-state"
readonly WAIT_SECS=300
readonly DEFAULT_DEPLOY_TIMEOUT=2700

log() { echo "config-sync: $*"; }

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

# Guest attributes are readable from outside with
# `gcloud compute instances get-guest-attributes`, which is how CI watches
# a rollout land. Never put anything sensitive here.
guest_attr() {
  curl -fsS -X PUT --data "$2" -H "Metadata-Flavor: Google" \
    "$MD/instance/guest-attributes/grain/$1" >/dev/null 2>&1 || true
}

# Replace ourselves if the metadata copy has changed, so an edit to this
# file rolls out like anything else. systemd's Restart=always brings the
# new one straight back up.
self_update() {
  local new
  new="$(mktemp)"
  if md instance/attributes/grain-config-sync-script > "$new" && [ -s "$new" ] \
     && ! cmp -s "$new" "$SELF"; then
    log "config-sync.sh changed in metadata; installing and restarting"
    install -m 0700 "$new" "$SELF"
    rm -f "$new"
    exit 0
  fi
  rm -f "$new"
}

# Fetch the current generation. With an etag, block until it differs or
# WAIT_SECS elapse -- so the common case is a long-lived hanging GET, not
# a poll.
GEN=""
ETAG=""
fetch_generation() {
  local url="$MD/instance/attributes/grain-deploy-generation"
  local hdr body
  if [ -n "$ETAG" ]; then
    url="$url?wait_for_change=true&timeout_sec=$WAIT_SECS&last_etag=$ETAG"
  fi
  hdr="$(mktemp)"; body="$(mktemp)"
  if curl -fsS --max-time $((WAIT_SECS + 30)) -D "$hdr" -o "$body" \
       -H "Metadata-Flavor: Google" "$url"; then
    GEN="$(cat "$body")"
    ETAG="$(grep -i '^etag:' "$hdr" | tail -1 | awk '{print $2}' | tr -d '\r')"
  else
    log "metadata fetch failed; retrying shortly"
    sleep 10
  fi
  rm -f "$hdr" "$body"
}

deploy_timeout() {
  md instance/attributes/grain-config 2>/dev/null \
    | python3 -c 'import json,sys; print(int(json.load(sys.stdin).get("deploy_timeout_secs") or 0))' \
      2>/dev/null || echo 0
}

run_deploy() {
  local generation="$1" timeout_secs new rc
  timeout_secs="$(deploy_timeout)"
  [ "$timeout_secs" -gt 0 ] 2>/dev/null || timeout_secs="$DEFAULT_DEPLOY_TIMEOUT"

  new="$(mktemp)"
  if ! md instance/attributes/grain-deploy-script > "$new" || [ ! -s "$new" ]; then
    log "could not fetch deploy script from metadata"
    rm -f "$new"
    return 1
  fi
  install -m 0700 "$new" "$DEPLOY"
  rm -f "$new"

  log "deploying generation $generation (timeout ${timeout_secs}s)"
  guest_attr deploy-status "running"
  guest_attr deploy-generation "$generation"

  rc=0
  timeout --signal=TERM --kill-after=60 "$timeout_secs" "$DEPLOY" || rc=$?

  if [ "$rc" -eq 0 ]; then
    log "generation $generation deployed"
    echo "ok $generation" > "$STATE"
    guest_attr deploy-status "ok"
  else
    log "generation $generation FAILED (exit $rc); will retry"
    echo "failed $generation" > "$STATE"
    guest_attr deploy-status "failed"
  fi
  guest_attr deploy-detail "exit=$rc generation=$generation"
  return "$rc"
}

main() {
  mkdir -p "$(dirname "$STATE")" 2>/dev/null || true
  log "started; watching grain-deploy-generation"

  while true; do
    self_update
    fetch_generation
    [ -n "$GEN" ] || continue

    local last_status="" last_gen=""
    if [ -f "$STATE" ]; then
      read -r last_status last_gen < "$STATE" || true
    fi

    if [ "$GEN" != "$last_gen" ] || [ "$last_status" != "ok" ]; then
      run_deploy "$GEN" || true
    fi
  done
}

main "$@"
