#!/usr/bin/env bash
# One-time setup, run once by a human with project-owner rights.
#
# Mirrors terraform/gcp/bootstrap-gcp.sh's v1 mechanism: it creates the
# Terraform state bucket and the deployer service account CI (or a human
# applying by hand) runs as, and -- given --repo -- workload identity
# federation so CI authenticates to GCP with no long-lived key at all.
# Idempotent: safe to re-run after changing anything.
#
#   ./terraform/gcp-v2/bootstrap-gcp.sh --project my-staging-project
#   ./terraform/gcp-v2/bootstrap-gcp.sh --project my-staging-project --repo my-org/my-config-repo
#
set -euo pipefail

PROJECT_ID=""
GITHUB_REPO=""
REGION="us-central1"
NAME_PREFIX="grain-v2-staging"
BUCKET=""
POOL_ID="github"
PROVIDER_ID="github"

usage() {
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'USAGE'

Options:
  --project    PROJECT_ID     GCP project (required)
  --repo       OWNER/NAME     if set, wires workload identity federation for this
                              GitHub repo's Actions workflows; omit for a purely
                              by-hand deployment
  --region     REGION         default: us-central1
  --prefix     PREFIX         resource-name prefix, must match name_prefix in
                              your tfvars. default: grain-v2-staging
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

[ -n "$PROJECT_ID" ] || { echo "--project is required" >&2; exit 2; }
if [ -n "$GITHUB_REPO" ]; then
  case "$GITHUB_REPO" in
    */*) : ;;
    *) echo "--repo must be OWNER/NAME" >&2; exit 2 ;;
  esac
fi
BUCKET="${BUCKET:-${PROJECT_ID}-${NAME_PREFIX}-tfstate}"

DEPLOYER="${NAME_PREFIX}-deployer"
DEPLOYER_EMAIL="${DEPLOYER}@${PROJECT_ID}.iam.gserviceaccount.com"

say() { printf '\n== %s\n' "$*"; }

say "Enabling APIs"
gcloud services enable --project="$PROJECT_ID" \
  cloudresourcemanager.googleapis.com \
  compute.googleapis.com \
  dns.googleapis.com \
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
gcloud storage buckets update "gs://$BUCKET" --project="$PROJECT_ID" \
  --versioning --public-access-prevention

say "Deployer service account: $DEPLOYER_EMAIL"
if ! gcloud iam service-accounts describe "$DEPLOYER_EMAIL" --project="$PROJECT_ID" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$DEPLOYER" --project="$PROJECT_ID" \
    --display-name="grain v2 staging deployer${GITHUB_REPO:+ ($GITHUB_REPO)}"
fi

say "Granting the deployer what Terraform needs"
# compute.admin covers every google_compute_* resource this module
# creates -- the instance, the disk, the network/firewalls, and the
# whole load balancer chain (health check, backend service, URL map,
# managed cert, target proxy, forwarding rule, global address).
# dns.admin only matters if dns_managed_zone is set; harmless otherwise.
# iap.admin is what lets Terraform's google_iap_brand/google_iap_client
# (iap.tf) and the roles/iap.httpsResourceAccessor bindings apply at
# all. resourcemanager.projectIamAdmin is what lets iam.tf's own
# google_project_iam_member resources grant roles in the first place.
for role in \
  roles/compute.admin \
  roles/dns.admin \
  roles/iam.serviceAccountAdmin \
  roles/iam.serviceAccountUser \
  roles/iap.admin \
  roles/resourcemanager.projectIamAdmin \
  roles/serviceusage.serviceUsageAdmin \
  roles/serviceusage.serviceUsageConsumer
do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member="serviceAccount:${DEPLOYER_EMAIL}" --role="$role" \
    --condition=None --quiet >/dev/null
  echo "  $role"
done

for role in roles/storage.objectAdmin roles/storage.legacyBucketReader; do
  gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --project="$PROJECT_ID" \
    --member="serviceAccount:${DEPLOYER_EMAIL}" --role="$role" >/dev/null
  echo "  $role on gs://$BUCKET"
done

if [ -n "$GITHUB_REPO" ]; then
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
fi

cat <<DONE

== Done.

Put this in your backend.hcl:

  bucket = "${BUCKET}"
  prefix = "${NAME_PREFIX}/staging"

and set project_id = "${PROJECT_ID}", deployer_member =
"serviceAccount:${DEPLOYER_EMAIL}" in your tfvars.

Next: terraform init -backend-config=backend.hcl, terraform apply, then
run push-secrets.sh once (see this directory's README) to give the host
a GitHub token and a Gemini API key.
DONE
