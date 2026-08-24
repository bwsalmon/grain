# Terraform creates the secret *containers* and grants the host account
# read access to them. It never creates a version, so no secret value
# passes through Terraform or lands in the state file.
#
# The values come from GitHub Actions secrets and are pushed straight into
# Secret Manager by .github/workflows/deploy.yml, which is also the only
# place they are ever decrypted. The host then reads them with its own
# instance identity -- the arrangement docs/design.md prefers over storing
# credentials anywhere a workflow could read them back.

resource "google_secret_manager_secret" "github_token" {
  secret_id = "${var.name_prefix}-github-token"
  labels    = var.labels

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
}

resource "google_secret_manager_secret" "claude_token" {
  secret_id = "${var.name_prefix}-claude-code-oauth-token"
  labels    = var.labels

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
}

resource "google_secret_manager_secret_iam_member" "host_reads_github_token" {
  secret_id = google_secret_manager_secret.github_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.host.email}"
}

resource "google_secret_manager_secret_iam_member" "host_reads_claude_token" {
  secret_id = google_secret_manager_secret.claude_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.host.email}"
}
