# The host's identity. Everything the VM is allowed to do in GCP is this
# account's roles -- edit vm_service_account_roles in config/grain.tfvars
# and the change arrives as a reviewable diff. It needs no Secret Manager
# grant: the two runtime credentials arrive as instance metadata, pushed
# there directly by the deploy workflow, and the host reads its own
# metadata with no GCP credential at all.

resource "google_service_account" "host" {
  account_id   = "${var.name_prefix}-host"
  display_name = "grain host VM (${var.name_prefix})"
  description  = "Attached to the grain host instance. Holds no secret grant; its two deploy credentials arrive as instance metadata instead."
}

resource "google_project_iam_member" "host" {
  for_each = toset(var.vm_service_account_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.host.email}"
}

# The narrow account a sandbox's own credentials belong to: the controller
# mints a fresh, short-lived key for this account on every dispatch
# (bwsalmon/agents#126, grain/automation/gcp_keys.py) and pushes it into
# the sandbox for the duration of that one task, revoking it once the
# task's slot frees (or, failing that, once it turns 24 hours old -- see
# gcp_keys.py's own docstring). Created when you ask for it by listing
# roles, by turning on agent_can_manage_compute_instances, or both --
# either alone is a real, supported combination, so the account's own
# existence can't gate on agent_service_account_roles specifically.

locals {
  agent_account_needed = length(var.agent_service_account_roles) > 0 || var.agent_can_manage_compute_instances || var.enable_gemini_key || var.agent_can_manage_gke

  # Unconditioned, unlike agent_conditioned_compute_roles below -- see
  # variables.tf's agent_can_manage_gke for why there is no equivalent
  # "the grain host's own cluster" to exclude here.
  agent_gke_roles = var.agent_can_manage_gke ? [
    "roles/container.admin",        # create/resize/delete clusters and node pools, plus the Kubernetes API objects on them
    "roles/artifactregistry.admin", # create/delete repositories, push/pull images
  ] : []

  # Only compute.instanceAdmin.v1 and compute.osLogin -- see
  # variables.tf's agent_can_manage_compute_instances for why
  # iap.tunnelResourceAccessor is granted separately, unconditioned.
  agent_conditioned_compute_roles = var.agent_can_manage_compute_instances ? [
    "roles/compute.instanceAdmin.v1", # create, delete, start, stop, list, get, setMetadata (SSH keys)
    "roles/compute.osLogin",          # SSH: OS Login provisions the POSIX account
  ] : []

  # Matches google_compute_instance.host's own name/zone (instance.tf).
  # Kept here, not cross-referenced, so the exclusion this guards is
  # legible without following it to another resource.
  grain_host_resource = "projects/${var.project_id}/zones/${var.zone}/instances/${var.name_prefix}-host"
}

resource "google_service_account" "agent" {
  count        = local.agent_account_needed ? 1 : 0
  account_id   = "${var.name_prefix}-agent"
  display_name = "grain sandboxed agents (${var.name_prefix})"
  description  = "The controller mints a fresh, short-lived key for this account on every dispatch and pushes it into the sandbox; this is what a sandboxed agent's GCP credentials belong to."
}

resource "google_project_iam_member" "agent" {
  for_each = toset(var.agent_service_account_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.agent[0].email}"
}

# Lets the deploy workflow mint (and invalidate) the agent account's own
# key -- the primary credential grain/automation/gemini_keys.py's own
# gcloud calls authenticate with (see grain's docs/design.md, "GCP
# credentials"), unrelated to the per-dispatch keys host_manages_agent_keys
# below lets the controller mint. bootstrap-gcp.sh, next to this file,
# grants the deployer project-wide serviceAccountAdmin/serviceAccountUser,
# but neither of those covers key management (iam.serviceAccountKeys.*) --
# that needs its own role, and scoping it to just this one account rather
# than granting it project-wide is the whole point of doing it here
# instead of in bootstrap-gcp.sh.
resource "google_service_account_iam_member" "deployer_manages_agent_keys" {
  count              = local.agent_account_needed ? 1 : 0
  service_account_id = google_service_account.agent[0].name
  role               = "roles/iam.serviceAccountKeyAdmin"
  member             = "serviceAccount:${var.name_prefix}-deployer@${var.project_id}.iam.gserviceaccount.com"
}

# bwsalmon/agents#126: lets the controller (which runs *as* google_service_
# account.host, via its own real, native GCE metadata server -- a
# completely different thing from the fake per-sandbox one this same
# change removed) mint and revoke the agent account's per-dispatch keys
# itself, at runtime, with no static credential of its own -- see
# grain/automation/gcp_keys.py's own docstring for the full design and why
# this must be a *different* account from the one being minted for (a
# leaked agent key must never be able to mint itself a fresh one).
resource "google_service_account_iam_member" "host_manages_agent_keys" {
  count              = local.agent_account_needed ? 1 : 0
  service_account_id = google_service_account.agent[0].name
  role               = "roles/iam.serviceAccountKeyAdmin"
  member             = "serviceAccount:${google_service_account.host.email}"
}

# Compute instance lifecycle and SSH, everywhere in this project except
# the grain host VM itself -- see variables.tf's
# agent_can_manage_compute_instances for the full reasoning, including
# why iap.tunnelResourceAccessor (below, separately) is not conditioned
# the same way.
resource "google_project_iam_member" "agent_compute" {
  for_each = toset(local.agent_conditioned_compute_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.agent[0].email}"

  condition {
    title       = "exclude-grain-host"
    description = "Instance management and SSH everywhere in this project except the grain host VM itself."
    expression  = "resource.type != \"compute.googleapis.com/Instance\" || resource.name != \"${local.grain_host_resource}\""
  }
}

# Network-tunnel reachability only -- see variables.tf for why this one
# role can't be conditioned to exclude the host the way the two above
# are, and why that's still safe: this grants no authentication
# capability by itself.
resource "google_project_iam_member" "agent_iap_tunnel" {
  count   = var.agent_can_manage_compute_instances ? 1 : 0
  project = var.project_id
  role    = "roles/iap.tunnelResourceAccessor"
  member  = "serviceAccount:${google_service_account.agent[0].email}"
}

# The Generative Language API itself -- grain/automation/gemini_keys.py's
# `apikeys.googleapis.com` calls fail with API-not-enabled until this
# exists, same as any other GCP API. disable_on_destroy is false: a
# `terraform destroy` (or turning enable_gemini_key back off) must not
# reach into the project and disable an API something else in it might
# also depend on -- the same "don't touch shared project state" instinct
# bootstrap-gcp.sh's own comments apply elsewhere.
resource "google_project_service" "generativelanguage" {
  count              = var.enable_gemini_key ? 1 : 0
  project            = var.project_id
  service            = "generativelanguage.googleapis.com"
  disable_on_destroy = false
}

# Lets the agent account mint and revoke Gemini API keys
# (grain/automation/gemini_keys.py) -- project-wide, since
# `gcloud services api-keys create/delete` operate at the project level,
# not against a single resource. The narrower alternative
# (apikeys.keys.create/.delete/.get without the rest of
# serviceusage.apiKeysAdmin) has no predefined role in GCP as of this
# writing, so a custom role would be the only way to shave this down
# further -- left for an operator who wants it, not the default here.
resource "google_project_iam_member" "agent_gemini_keys" {
  count   = var.enable_gemini_key ? 1 : 0
  project = var.project_id
  role    = "roles/serviceusage.apiKeysAdmin"
  member  = "serviceAccount:${google_service_account.agent[0].email}"
}

# GKE and Artifact Registry APIs -- disable_on_destroy is false for the
# same reason as generativelanguage above: a `terraform destroy` (or
# flipping agent_can_manage_gke back off) must not reach into the project
# and disable an API something else in it might also depend on.
resource "google_project_service" "container" {
  count              = var.agent_can_manage_gke ? 1 : 0
  project            = var.project_id
  service            = "container.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "artifactregistry" {
  count              = var.agent_can_manage_gke ? 1 : 0
  project            = var.project_id
  service            = "artifactregistry.googleapis.com"
  disable_on_destroy = false
}

# GKE cluster and Artifact Registry repository lifecycle, project-wide --
# see variables.tf's agent_can_manage_gke for why this is unconditioned,
# unlike agent_compute above.
resource "google_project_iam_member" "agent_gke" {
  for_each = toset(local.agent_gke_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.agent[0].email}"

  depends_on = [google_project_service.container, google_project_service.artifactregistry]
}
