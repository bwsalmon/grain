#!/usr/bin/env bash
# Block until the host reports it has converged on this deploy generation.
#
# The host publishes progress through guest attributes, so CI fails when
# the rollout fails rather than when `terraform apply` returns -- applying
# the Terraform is only the start of a deploy, and the interesting half
# happens on the VM afterwards.
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
    --project="$project" --zone="$zone" --query-path="grain/$1" \
    --format='value(value)' 2>/dev/null || true
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
        echo "::error::deploy failed on the host: $(attr deploy-detail)"
        echo "Inspect it with:"
        echo "  gcloud compute ssh $instance --zone $zone --project $project --tunnel-through-iap \\"
        echo "    --command 'sudo journalctl -u grain-config-sync -n 200 --no-pager'"
        exit 1
        ;;
    esac
  fi
  echo "waiting: generation=${generation:-none} status=${status:-none}"
  sleep "$poll_seconds"
done

echo "::error::host did not report success within ${timeout_minutes}m"
echo "A first deploy downloads a base image and boots several VMs;"
echo "raise the ROLLOUT_TIMEOUT_MINUTES repository variable if it is just slow."
exit 1
