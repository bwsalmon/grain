# Organization-policy guardrails against external exposure -- narrower and
# blunter than IAM: these hold for every identity in the project, not just
# the ones this deployment grants roles to, and turning one back off needs
# another apply, not a revoked grant. See variables.tf's lock_down_project
# for what turns them on and why they are opt-in.

resource "google_org_policy_policy" "vm_external_ip_access" {
  count  = var.lock_down_project ? 1 : 0
  name   = "projects/${var.project_id}/policies/compute.vmExternalIpAccess"
  parent = "projects/${var.project_id}"

  spec {
    rules {
      values {
        denied_values = ["*"]
      }
    }
  }
}

resource "google_org_policy_policy" "storage_public_access_prevention" {
  count  = var.lock_down_project ? 1 : 0
  name   = "projects/${var.project_id}/policies/storage.publicAccessPrevention"
  parent = "projects/${var.project_id}"

  spec {
    rules {
      enforce = "TRUE"
    }
  }
}
