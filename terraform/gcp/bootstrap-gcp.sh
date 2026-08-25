#!/usr/bin/env bash
# One-time setup, run once by a human with project-owner rights, from a
# clone of grain (this script is not vendored into a config repo -- see
# config-repo-template/README.md). Everything after this is done by CI
# from a pull request.
#
# It creates the things that cannot bootstrap themselves: the Terraform
# state bucket, the deployer service account CI runs as, and the workload
# identity federation that lets the config repo's workflows authenticate
# to GCP *without a service account key* -- so the only GitHub secrets
# that hold a real credential are grain's own two runtime tokens.
#
# Idempotent: safe to re-run after changing anything.
#
#   ./terraform/gcp/bootstrap-gcp.sh --project my-project --repo my-org/my-config-repo
#
set -euo pipefail

PROJECT_ID=""
GITHUB_REPO=""
REGION="us-central1"
NAME_PREFIX="grain"
BUCKET=""
POOL_ID="github"
PROVIDER_ID="github"

usage() {
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'USAGE'

Options:
  --project    PROJECT_ID     GCP project (required)
  --repo       OWNER/NAME     the GitHub repo these workflows run in (required)
  --region     REGION         default: us-central1
  --prefix     PREFIX         resource-name prefix, must match name_prefix
                              in config/grain.tfvars. default: grain
  --bucket     NAME           Terraform state bucket. default: PROJECT-PREFIX-tfstate
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --project) PROJECT_ID="$2"; shift 2 ;;
    --repo)    GITHUB_REPO="$2"; shift 2 ;;
    --region)  REGION="$2"; shift 2 ;;
    --prefix)  NAME_PREFIX="$2"; shift 2 ;;
    --bucket)  BUCKET="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$PROJECT_ID" ]  || { echo "--project is required" >&2; exit 2; }
[ -n "$GITHUB_REPO" ] || { echo "--repo is required" >&2; exit 2; }
case "$GITHUB_REPO" in
  */*) : ;;
  *) echo "--repo must be OWNER/NAME" >&2; exit 2 ;;
esac
BUCKET="${BUCKET:-${PROJECT_ID}-${NAME_PREFIX}-tfstate}"

DEPLOYER="${NAME_PREFIX}-deployer"
DEPLOYER_EMAIL="${DEPLOYER}@${PROJECT_ID}.iam.gserviceaccount.com"

say() { printf '\n== %s\n' "$*"; }

say "Enabling APIs"
gcloud services enable --project="$PROJECT_ID" \
  cloudresourcemanager.googleapis.com \
  compute.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  iap.googleapis.com \
  serviceusage.googleapis.com \
  storage.googleapis.com \
  sts.googleapis.com

say "Terraform state bucket: gs://$BUCKET"
if ! gcloud storage buckets describe "gs://$BUCKET" --project="$PROJECT_ID" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://$BUCKET" \
    --project="$PROJECT_ID" --location="$REGION" --uniform-bucket-level-access
fi
# State records every resource this repo manages. Versioning turns a bad
# apply into a recoverable one; public access is never appropriate.
gcloud storage buckets update "gs://$BUCKET" --project="$PROJECT_ID" \
  --versioning --public-access-prevention

say "Deployer service account: $DEPLOYER_EMAIL"
if ! gcloud iam service-accounts describe "$DEPLOYER_EMAIL" --project="$PROJECT_ID" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$DEPLOYER" --project="$PROJECT_ID" \
    --display-name="grain config repo CI (${GITHUB_REPO})"
fi

say "Granting the deployer what Terraform needs"
# Deliberately not owner/editor. Each role here maps to something in
# terraform/: the instance and network, the host's service account, and
# the IAM bindings that account gets. No Secret Manager role: the two
# runtime credentials go straight into instance metadata, which
# compute.admin already covers.
for role in \
  roles/compute.admin \
  roles/iam.serviceAccountAdmin \
  roles/iam.serviceAccountUser \
  roles/resourcemanager.projectIamAdmin \
  roles/serviceusage.serviceUsageConsumer
do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member="serviceAccount:${DEPLOYER_EMAIL}" --role="$role" \
    --condition=None --quiet >/dev/null
  echo "  $role"
done

# State access is scoped to the one bucket, not the project.
for role in roles/storage.objectAdmin roles/storage.legacyBucketReader; do
  gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --project="$PROJECT_ID" \
    --member="serviceAccount:${DEPLOYER_EMAIL}" --role="$role" >/dev/null
  echo "  $role on gs://$BUCKET"
done

say "Workload identity federation for $GITHUB_REPO"
if ! gcloud iam workload-identity-pools describe "$POOL_ID" \
     --project="$PROJECT_ID" --location=global >/dev/null 2>&1; then
  gcloud iam workload-identity-pools create "$POOL_ID" \
    --project="$PROJECT_ID" --location=global \
    --display-name="GitHub Actions"
fi

# The attribute condition is the security boundary: without it, *any*
# GitHub repository in the world could mint tokens for this project.
if ! gcloud iam workload-identity-pools providers describe "$PROVIDER_ID" \
     --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL_ID" >/dev/null 2>&1; then
  gcloud iam workload-identity-pools providers create-oidc "$PROVIDER_ID" \
    --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL_ID" \
    --display-name="GitHub Actions OIDC" \
    --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner" \
    --attribute-condition="assertion.repository == '${GITHUB_REPO}'"
else
  gcloud iam workload-identity-pools providers update-oidc "$PROVIDER_ID" \
    --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL_ID" \
    --attribute-condition="assertion.repository == '${GITHUB_REPO}'" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner"
fi

PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"
POOL_NAME="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL_ID}"
PROVIDER_NAME="${POOL_NAME}/providers/${PROVIDER_ID}"

gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_EMAIL" \
  --project="$PROJECT_ID" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/${POOL_NAME}/attribute.repository/${GITHUB_REPO}" \
  --quiet >/dev/null

cat <<DONE

== Done. Two things left.

1. Put this in config/backend.hcl:

     bucket = "${BUCKET}"
     prefix = "${NAME_PREFIX}/prod"

   and set project_id = "${PROJECT_ID}" in config/grain.tfvars.

2. Set the repository secrets:

     gh secret set GCP_WORKLOAD_IDENTITY_PROVIDER --repo ${GITHUB_REPO} \\
       --body '${PROVIDER_NAME}'
     gh secret set GCP_DEPLOYER_SERVICE_ACCOUNT --repo ${GITHUB_REPO} \\
       --body '${DEPLOYER_EMAIL}'

     gh secret set GRAIN_GITHUB_TOKEN --repo ${GITHUB_REPO}
     gh secret set GRAIN_CLAUDE_CODE_OAUTH_TOKEN --repo ${GITHUB_REPO}

   The last two are grain's own runtime credentials -- a GitHub token for
   the repos it works on, and the output of \`claude setup-token\`. The
   deploy workflow reads them once and pushes them straight into the
   host's own instance metadata; nothing else in the project ever holds
   them.

Then push to main.
DONE
