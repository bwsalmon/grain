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

# instance.tf folds a short hash of grain_config's own content onto the
# end of the value it writes to grain-deploy-generation (bwsalmon/agents#592:
# "changing max concurrent agents takes no effect even after reboot"), so
# the host reports "$deploy_generation-<hash>", never the bare token CI
# passed to `terraform apply` as var.deploy_generation. A plain string
# comparison against that bare token therefore never matched, even on a
# rollout the host had already finished -- every deploy after #592
# landed waited out the full timeout and was reported as hung/failed
# despite the host converging within seconds (bwsalmon/agents#633).
generation_matches() {
  case "$1" in
    "$deploy_generation" | "$deploy_generation"-*) return 0 ;;
    *) return 1 ;;
  esac
}

deadline=$(( SECONDS + timeout_minutes * 60 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  status="$(attr deploy-status)"
  generation="$(attr deploy-generation)"
  if generation_matches "$generation"; then
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
