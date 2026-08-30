#!/usr/bin/env bash
# Block until the v2 staging host reports it has converged on this
# deploy generation.
#
# The host publishes progress through guest attributes
# (terraform/gcp-v2/files/config-sync.sh), so CI fails when the rollout
# fails rather than when `terraform apply` returns -- applying the
# Terraform is only the start of a deploy, and the interesting half
# happens on the VM afterwards.
#
# The guest attribute namespace is "grain-v2", not v1's "grain", which is
# what lets a v1 and a v2 deployment coexist in one project without
# reading each other's status.
#
# Required env:
#   PROJECT, INSTANCE, ZONE   from the Terraform outputs
#   DEPLOY_GENERATION         the token the host is expected to report
# Optional env:
#   TIMEOUT_MINUTES  give up after this long (default 45)
#   POLL_SECONDS     seconds between polls (default 20)
set -euo pipefail

project="${PROJECT:?PROJECT is not set: the terraform project_id output}"
instance="${INSTANCE:?INSTANCE is not set: the terraform instance_name output}"
zone="${ZONE:?ZONE is not set: the terraform zone output}"
deploy_generation="${DEPLOY_GENERATION:?DEPLOY_GENERATION is not set}"
timeout_minutes="${TIMEOUT_MINUTES:-45}"
poll_seconds="${POLL_SECONDS:-20}"

attr() {
  gcloud compute instances get-guest-attributes "$instance" \
    --project="$project" --zone="$zone" --query-path="grain-v2/$1" \
    --format='value(value)' 2>/dev/null || true
}

log_hint() {
  echo "Read the host's own log with:"
  echo "  gcloud compute ssh $instance --zone $zone --project $project \\"
  echo "    --tunnel-through-iap --command 'sudo journalctl -u grain-v2-config-sync -n 200'"
}

deadline=$(( SECONDS + timeout_minutes * 60 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  status="$(attr deploy-status)"
  generation="$(attr deploy-generation)"
  if [ "$generation" = "$deploy_generation" ]; then
    case "$status" in
      ok)
        echo "host converged on $deploy_generation"
        exit 0
        ;;
      failed)
        echo "::error::the host failed to converge on $deploy_generation: $(attr deploy-detail)"
        tail="$(attr deploy-tail)"
        if [ -n "$tail" ]; then
          echo "--- the last of the host's own deploy output ---"
          echo "$tail"
          echo "--- end ---"
        fi
        log_hint
        exit 1
        ;;
    esac
  fi
  echo "waiting: status=${status:-<none>} generation=${generation:-<none>} (want $deploy_generation)"
  sleep "$poll_seconds"
done

# A timeout is not the same as a failure: config-sync retries on its own
# roughly every five minutes, so a rollout that is merely slow (a first
# boot downloads a base image and builds the binary) may still land after
# this gives up.
echo "::error::timed out after ${timeout_minutes}m waiting for the host to converge on $deploy_generation."
echo "The host keeps retrying on its own; this step gave up, the rollout may not have."
log_hint
exit 1
