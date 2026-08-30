#!/usr/bin/env bash
# One-time setup, run once by a human with project-owner rights, from a
# clone of grain (this script is not vendored into a config repo -- see
# templates/gcp/README.md). Everything after this is done by CI
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
# Empty here, derived from NAME_PREFIX after argument parsing (--prefix
# may not have been seen yet) unless --pool/--provider say otherwise.
#
# A workload identity pool is a *project-level* resource, so two grain
# deployments sharing a project -- which this README says works fine,
# given different name_prefix and backend prefixes -- would both land on
# an unprefixed name. The second bootstrap then takes the update branch
# on the first's provider and rewrites its attribute condition to name
# the second's repo, and the first's deploy workflow silently loses the
# ability to authenticate at all. Prefixing is what makes that README
# claim true of the workload identity half too, rather than only of the
# state bucket and the service account, which were already prefixed.
POOL_ID=""
PROVIDER_ID=""

# What this script used to hardcode for both. A deployment bootstrapped
# before the prefixing above is still wired to it, so it is adopted
# rather than abandoned -- see the "legacy" block below.
LEGACY_POOL_ID="github"
LEGACY_PROVIDER_ID="github"

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
  --pool       ID             workload identity pool id. default: PREFIX, so two
                              deployments in one project never share a pool. An
                              existing unprefixed "github" pool already wired to
                              --repo is adopted instead, so re-running this against
                              a deployment set up before prefixing changes nothing
  --provider   ID             workload identity provider id. default: as --pool
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --project) PROJECT_ID="$2"; shift 2 ;;
    --repo)    GITHUB_REPO="$2"; shift 2 ;;
    --region)  REGION="$2"; shift 2 ;;
    --prefix)  NAME_PREFIX="$2"; shift 2 ;;
    --bucket)  BUCKET="$2"; shift 2 ;;
    --pool)    POOL_ID="$2"; shift 2 ;;
    --provider) PROVIDER_ID="$2"; shift 2 ;;
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
  orgpolicy.googleapis.com \
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
# compute.admin already covers. serviceusage.serviceUsageAdmin (distinct
# from the serviceUsageConsumer below) is what lets Terraform's own
# google_project_service.generativelanguage (iam.tf, enable_gemini_key)
# and google_project_service.container/artifactregistry (iam.tf,
# agent_can_manage_gke) turn an API on -- serviceUsageConsumer only
# covers *using* an already-enabled one. orgpolicy.policyAdmin is what
# lets Terraform's google_org_policy_policy resources (lockdown.tf,
# lock_down_project) set a project-level policy at all -- granted
# unconditionally, same as the
# gemini-key role above, so turning the tfvars flag on later needs no
# second bootstrap run.
for role in \
  roles/compute.admin \
  roles/iam.serviceAccountAdmin \
  roles/iam.serviceAccountUser \
  roles/orgpolicy.policyAdmin \
  roles/resourcemanager.projectIamAdmin \
  roles/serviceusage.serviceUsageAdmin \
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

# Which pool and provider, now that --prefix and --pool have both been
# seen. Prefixed by default so two deployments in one project never
# collide -- with one exception, below.
if [ -z "$POOL_ID" ] && [ -z "$PROVIDER_ID" ]; then
  # A deployment bootstrapped before prefixing is wired to the old
  # unprefixed names, and its GCP_WORKLOAD_IDENTITY_PROVIDER secret still
  # names them. Switching it to a prefixed pool on a routine re-run would
  # leave that secret pointing at a provider this script no longer
  # maintains, so adopt what is already there instead.
  #
  # Adopted only when the legacy provider's attribute condition already
  # names *this* --repo -- that is what makes it this deployment's own
  # earlier output rather than some other deployment's pool, which is the
  # collision the prefixing exists to prevent. Anything else takes the
  # prefixed default.
  legacy_condition="$(gcloud iam workload-identity-pools providers describe "$LEGACY_PROVIDER_ID" \
    --project="$PROJECT_ID" --location=global \
    --workload-identity-pool="$LEGACY_POOL_ID" \
    --format='value(attributeCondition)' 2>/dev/null || true)"
  case "$legacy_condition" in
    *"'${GITHUB_REPO}'"*)
      POOL_ID="$LEGACY_POOL_ID"
      PROVIDER_ID="$LEGACY_PROVIDER_ID"
      say "Reusing this project's existing \"$LEGACY_POOL_ID\" workload identity pool"
      echo "  It is already wired to $GITHUB_REPO, so this deployment predates prefixed"
      echo "  pool names and its GCP_WORKLOAD_IDENTITY_PROVIDER secret still points at it."
      echo "  Nothing changes, and re-running this stays a no-op."
      echo
      echo "  To move it onto a pool of its own -- worth doing before a second"
      echo "  deployment shares this project -- re-run with:"
      echo "      --pool $NAME_PREFIX --provider $NAME_PREFIX"
      echo "  and update that secret to the provider printed at the end."
      ;;
  esac
fi
POOL_ID="${POOL_ID:-$NAME_PREFIX}"
PROVIDER_ID="${PROVIDER_ID:-$NAME_PREFIX}"

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
