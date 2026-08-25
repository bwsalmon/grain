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

# The narrow account agents get tokens for, minted by the controller's
# metadata server. Created when you ask for it by listing roles, by
# turning on agent_can_manage_compute_instances, or both -- either alone
# is a real, supported combination, so the account's own existence can't
# gate on agent_service_account_roles specifically.

locals {
  agent_account_needed = length(var.agent_service_account_roles) > 0 || var.agent_can_manage_compute_instances

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
  description  = "Impersonated by the controller's metadata server; this is what a sandboxed agent's ADC resolves to."
}

resource "google_project_iam_member" "agent" {
  for_each = toset(var.agent_service_account_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.agent[0].email}"
}

resource "google_service_account_iam_member" "host_impersonates_agent" {
  count              = local.agent_account_needed ? 1 : 0
  service_account_id = google_service_account.agent[0].name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.host.email}"
}

# Lets the deploy workflow mint (and invalidate) the agent account's own
# key -- the impersonation *source* grain's metadata servers read, never
# handed out directly (see grain's docs/design.md, "GCP credentials").
# scripts/bootstrap-gcp.sh grants the deployer project-wide
# serviceAccountAdmin/serviceAccountUser, but neither of those covers key
# management (iam.serviceAccountKeys.*) -- that needs its own role, and
# scoping it to just this one account rather than granting it project-wide
# is the whole point of doing it here instead of in bootstrap-gcp.sh.
resource "google_service_account_iam_member" "deployer_manages_agent_keys" {
  count              = local.agent_account_needed ? 1 : 0
  service_account_id = google_service_account.agent[0].name
  role               = "roles/iam.serviceAccountKeyAdmin"
  member             = "serviceAccount:${var.name_prefix}-deployer@${var.project_id}.iam.gserviceaccount.com"
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
