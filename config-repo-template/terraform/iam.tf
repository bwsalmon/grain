# The host's identity. Everything the VM is allowed to do in GCP is this
# account's roles plus the two secret-level grants in secrets.tf -- edit
# vm_service_account_roles in config/grain.tfvars and the change arrives as
# a reviewable diff.

resource "google_service_account" "host" {
  account_id   = "${var.name_prefix}-host"
  display_name = "grain host VM (${var.name_prefix})"
  description  = "Attached to the grain host instance. Reads its own deploy secrets; nothing else by default."
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
