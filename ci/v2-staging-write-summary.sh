#!/usr/bin/env bash
# Write the v2 staging deploy's job summary. Runs with if: always(), so
# it must never fail the job itself -- every value is read null-safe and
# a missing one prints as empty rather than aborting.
#
# Required env:
#   GITHUB_STEP_SUMMARY  set by the Actions runner
# Optional env:
#   URL, INSTANCE, ZONE, PROJECT, DEPLOY_GENERATION
set -euo pipefail

summary="${GITHUB_STEP_SUMMARY:?GITHUB_STEP_SUMMARY is not set (is this running outside Actions?)}"

{
  echo "## grain v2 staging"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| URL | ${URL:-<not applied>} |"
  echo "| Instance | ${INSTANCE:-<not applied>} |"
  echo "| Zone | ${ZONE:-<not applied>} |"
  echo "| Project | ${PROJECT:-<not applied>} |"
  echo "| Generation | ${DEPLOY_GENERATION:-<none>} |"
  echo
  echo "The URL is reachable only after signing in as one of \`iap_members\`."
  echo
  echo "Host log:"
  echo
  echo '```sh'
  echo "gcloud compute ssh ${INSTANCE:-INSTANCE} --zone ${ZONE:-ZONE} --project ${PROJECT:-PROJECT} \\"
  echo "  --tunnel-through-iap --command 'sudo journalctl -u grain-v2-config-sync -f'"
  echo '```'
} >> "$summary"
