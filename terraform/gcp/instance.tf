locals {
  # The config repo is the task repo unless you say otherwise: issues filed
  # here and labelled `grain-agent` are the queue. CI supplies config_repo
  # from github.repository, so the common case needs no configuration at
  # all.
  task_repo = var.task_repo != "" ? var.task_repo : var.config_repo

  # Empty when local.agent_account_needed (iam.tf) is false --
  # google_service_account.agent doesn't exist in that case, and no agent
  # identity is part of this deployment at all.
  agent_service_account_email = local.agent_account_needed ? google_service_account.agent[0].email : ""

  # Non-secret (docs/roadmap.md's "config, like task_repo" precedent for
  # agent_service_account_email above): just the project id that turns the
  # grain-gemini-key task label on, read by deploy.sh alongside the agent
  # account's own key. Empty when enable_gemini_key is false, so deploy.sh
  # never passes --gemini-project-id and the feature stays off.
  gemini_project_id = var.enable_gemini_key ? var.project_id : ""

  # bwsalmon/agents#113: 0 (falsy on the shell side, same "empty/zero is
  # the off switch" idiom gemini_project_id above already uses) when
  # enable_janitor is false, so deploy.sh never passes --janitor-ttl-hours
  # and the feature stays off.
  janitor_ttl_hours = var.enable_janitor ? var.janitor_ttl_hours : 0

  # Everything the on-VM deploy script needs, and nothing it does not: no
  # secret values here either -- the runtime credentials arrive separately,
  # pushed straight into instance metadata by the deploy workflow after
  # `terraform apply` returns, never through Terraform.
  grain_config = {
    grain_repo_url                = var.grain_repo_url
    grain_ref                     = var.grain_ref
    debian_image_url              = var.debian_image_url
    sandbox_count                 = var.sandbox_count
    cluster_overrides             = var.cluster_overrides
    task_repo                     = local.task_repo
    target_repos                  = var.target_repos
    default_target_repo           = var.default_target_repo
    credential_name               = var.credential_name
    agent_service_account_email   = local.agent_service_account_email
    gemini_project_id             = local.gemini_project_id
    deploy_timeout_secs           = var.deploy_timeout_minutes * 60
    bootstrap_ssh_timeout_seconds = var.bootstrap_ssh_timeout_seconds
    name_prefix                   = var.name_prefix
    janitor_ttl_hours             = local.janitor_ttl_hours
    scheduled_jobs                = var.scheduled_jobs
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
    enable-oslogin          = var.enable_os_login ? "TRUE" : "FALSE"
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

  # Catch the repo-wiring mistakes in the plan, where they cost a comment
  # on a pull request, rather than on the host, where they cost a failed
  # deploy and a journalctl session.
  lifecycle {
    precondition {
      condition     = local.task_repo != ""
      error_message = "Neither task_repo nor config_repo is set. CI passes config_repo from github.repository; running Terraform by hand needs one of the two in config/grain.tfvars."
    }

    precondition {
      condition     = can(regex("^[^/[:space:]]+/[^/[:space:]]+$", local.task_repo))
      error_message = "task_repo must be owner/name, got '${local.task_repo}'."
    }

    precondition {
      condition     = alltrue([for r in var.target_repos : can(regex("^[^/[:space:]]+/[^/[:space:]]+$", r))])
      error_message = "every entry in target_repos must be owner/name."
    }

    precondition {
      # grain refuses to dispatch a task whose default target is not an
      # allow-listed one. With target_repos empty the task repo is the
      # sole target, and so the only legal default.
      condition = var.default_target_repo == "" || (
        length(var.target_repos) == 0
        ? var.default_target_repo == local.task_repo
        : contains(var.target_repos, var.default_target_repo)
      )
      error_message = "default_target_repo must be one of target_repos (or, with target_repos empty, the task repo itself)."
    }

    precondition {
      # lockdown.tf's compute.vmExternalIpAccess policy denies external
      # IPs project-wide, this host included -- catch the
      # self-contradiction here rather than have the host's own
      # access_config fail against a policy this same apply just put in
      # place.
      condition     = !(var.lock_down_project && var.assign_external_ip)
      error_message = "lock_down_project denies external IPs project-wide, including on this host; set assign_external_ip = false (and enable_cloud_nat = true for egress) before turning lock_down_project on."
    }

    # grain-github-token and grain-claude-token are never declared here --
    # the deploy workflow adds them directly with `gcloud compute
    # instances add-metadata` after this resource exists, so the value
    # never passes through Terraform or lands in the state file. Without
    # this, the next apply would see them as drift and remove them.
    ignore_changes = [
      metadata["grain-github-token"],
      metadata["grain-claude-token"],
    ]
  }
}
