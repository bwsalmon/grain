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
# metadata server. Created only when you ask for it by listing roles.

resource "google_service_account" "agent" {
  count        = length(var.agent_service_account_roles) > 0 ? 1 : 0
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
  count              = length(var.agent_service_account_roles) > 0 ? 1 : 0
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
  count              = length(var.agent_service_account_roles) > 0 ? 1 : 0
  service_account_id = google_service_account.agent[0].name
  role               = "roles/iam.serviceAccountKeyAdmin"
  member             = "serviceAccount:${var.name_prefix}-deployer@${var.project_id}.iam.gserviceaccount.com"
}
