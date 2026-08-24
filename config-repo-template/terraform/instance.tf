locals {
  # Everything the on-VM deploy script needs, and nothing it does not: no
  # secret values, only the names of the secrets to read with the
  # instance's own identity.
  grain_config = {
    project_id          = var.project_id
    grain_repo_url      = var.grain_repo_url
    grain_ref           = var.grain_ref
    debian_image_url    = var.debian_image_url
    sandbox_count       = var.sandbox_count
    cluster_overrides   = var.cluster_overrides
    task_repo           = var.task_repo
    target_repos        = var.target_repos
    default_target_repo = var.default_target_repo
    credential_name     = var.credential_name
    github_token_secret = google_secret_manager_secret.github_token.secret_id
    claude_token_secret = google_secret_manager_secret.claude_token.secret_id
    deploy_timeout_secs = var.deploy_timeout_minutes * 60
  }
}

resource "google_compute_disk" "data" {
  name   = "${var.name_prefix}-data"
  type   = var.data_disk_type
  zone   = var.zone
  size   = var.data_disk_gb
  labels = var.labels

  # This disk is /var/lib/grain: the guest disks, the admin SSH key, and
  # -- inside the controller's disk -- /data, which holds every credential
  # and all automation state. Losing it is not recoverable from this repo.
  # Remove this block deliberately if you really mean to destroy it.
  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_instance" "host" {
  name         = "${var.name_prefix}-host"
  machine_type = var.machine_type
  zone         = var.zone
  tags         = [local.host_tag]
  labels       = var.labels

  # Metadata updates (a new deploy_generation) apply to a running
  # instance; a machine_type change needs a stop, and this permits it.
  allow_stopping_for_update = true

  # The whole point of this machine: grain runs libvirt guests on it.
  # Without this there is no /dev/kvm and the deploy fails loudly.
  advanced_machine_features {
    enable_nested_virtualization = true
  }

  boot_disk {
    initialize_params {
      image  = var.boot_image
      size   = var.boot_disk_gb
      type   = "pd-balanced"
      labels = var.labels
    }
  }

  attached_disk {
    source      = google_compute_disk.data.id
    device_name = "grain-data"
    mode        = "READ_WRITE"
  }

  network_interface {
    network    = local.network_name
    subnetwork = local.subnetwork_name

    dynamic "access_config" {
      for_each = var.assign_external_ip ? [1] : []
      content {}
    }
  }

  service_account {
    email = google_service_account.host.email
    # Scopes are the legacy control; the roles on the account are the real
    # one. cloud-platform here means "let IAM decide", which is what
    # vm_service_account_roles is for.
    scopes = ["cloud-platform"]
  }

  scheduling {
    # Nested virtualization and live migration have a history; terminating
    # and restarting is the boring choice, and config-sync reconverges on
    # boot anyway.
    on_host_maintenance = "TERMINATE"
    automatic_restart   = true
  }

  dynamic "shielded_instance_config" {
    for_each = var.enable_shielded_vm ? [1] : []
    content {
      enable_secure_boot          = true
      enable_vtpm                 = true
      enable_integrity_monitoring = true
    }
  }

  metadata = {
    enable-oslogin          = "TRUE"
    enable-guest-attributes = "TRUE"

    startup-script = file("${path.module}/files/startup.sh")

    # Shipped as metadata rather than fetched from this repo, so the host
    # needs no credential for a repo that may well be private.
    grain-config-sync-script = file("${path.module}/files/config-sync.sh")
    grain-deploy-script      = file("${path.module}/files/deploy.sh")

    grain-config = jsonencode(local.grain_config)

    # Changing this is what triggers a rollout. CI sets it to the commit
    # SHA; config-sync notices within seconds and redeploys.
    grain-deploy-generation = var.deploy_generation
  }

  depends_on = [
    google_secret_manager_secret_iam_member.host_reads_github_token,
    google_secret_manager_secret_iam_member.host_reads_claude_token,
  ]
}
