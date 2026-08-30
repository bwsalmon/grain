#!/usr/bin/env bash
# The rollout mechanism. Runs forever as a systemd service.
#
# Mirrors terraform/gcp/files/config-sync.sh's v1 mechanism exactly:
# Terraform writes a `grain-deploy-generation` attribute into instance
# metadata, and this service hangs on the metadata server's
# wait_for_change endpoint, re-fetching and running deploy.sh from
# metadata every time that value changes. No inbound SSH, no runner with
# credentials, no agent polling GitHub -- just `terraform apply` (or a
# metadata edit) changing one value the host is already watching.
#
# A failed deploy is retried on the next wake-up, so a transient failure
# (an apt mirror, a Docker pull, GitHub, a secret not pushed yet)
# self-heals without a second apply.
set -euo pipefail

readonly MD="http://metadata.google.internal/computeMetadata/v1"
readonly SELF="/opt/grain-deploy/config-sync.sh"
readonly DEPLOY="/opt/grain-deploy/deploy.sh"
readonly STATE="/var/lib/grain/.deploy-state"
readonly WAIT_SECS=300
# Generous: a cold `make container-build` pulls a Go+Node builder image,
# downloads every Go module and npm package, and compiles both a Go
# binary and a Vite frontend, on top of the daemon's own restart.
readonly DEPLOY_TIMEOUT_SECS=2700

log() { echo "grain-v2-config-sync: $*"; }

md() { curl -fsS -H "Metadata-Flavor: Google" "$MD/$1"; }

# Guest attributes are readable from outside with
# `gcloud compute instances get-guest-attributes`, which is how CI (or
# an operator) watches a rollout land. Never put anything sensitive here.
guest_attr() {
  curl -fsS -X PUT --data "$2" -H "Metadata-Flavor: Google" \
    "$MD/instance/guest-attributes/grain-v2/$1" >/dev/null 2>&1 || true
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

run_deploy() {
  local generation="$1" new rc
  new="$(mktemp)"
  if ! md instance/attributes/grain-deploy-script > "$new" || [ ! -s "$new" ]; then
    log "could not fetch deploy script from metadata"
    rm -f "$new"
    return 1
  fi
  install -m 0700 "$new" "$DEPLOY"
  rm -f "$new"

  log "deploying generation $generation (timeout ${DEPLOY_TIMEOUT_SECS}s)"
  guest_attr deploy-status "running"
  guest_attr deploy-generation "$generation"

  # Tee'd rather than run bare so a failure can publish its own last
  # words. "exit=127 generation=..." alone says a command was not found
  # but never which one, and reading the journal that does say needs SSH
  # to the host -- which is exactly what an operator locked out by OS
  # Login, or debugging from CI, does not have. The tail goes into a
  # guest attribute, which is readable with
  # `gcloud compute instances get-guest-attributes` and no shell at all.
  # The status is taken in the `||` branch, not from a PIPESTATUS read on
  # the next line. `pipeline || true` runs `true`, and `true` is itself a
  # pipeline, so it resets PIPESTATUS -- a later ${PIPESTATUS[0]} then
  # reads 0 no matter how the deploy exited. That mistake reported every
  # failed rollout as converged, which is worse than the missing detail
  # this tee was added to provide.
  local out rc=0
  out="$(mktemp)"
  timeout --signal=TERM --kill-after=60 "$DEPLOY_TIMEOUT_SECS" "$DEPLOY" 2>&1 \
    | tee /dev/stderr > "$out" || rc="${PIPESTATUS[0]}"

  if [ "$rc" -eq 0 ]; then
    log "generation $generation deployed"
    echo "ok $generation" > "$STATE"
    guest_attr deploy-status "ok"
  else
    log "generation $generation FAILED (exit $rc); will retry"
    echo "failed $generation" > "$STATE"
    guest_attr deploy-status "failed"
    # Bounded hard: a guest attribute is a small value, and this is a
    # pointer at the failure, not a log shipper. The journal remains the
    # full record.
    guest_attr deploy-tail "$(tail -c 1200 "$out" | tr -d '\000')"
  fi
  guest_attr deploy-detail "exit=$rc generation=$generation"
  rm -f "$out"
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
