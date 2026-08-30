# This module deploys a *staging* environment of v2 (bwsalmon/agents#394):
# one small VM running `grain daemon` (v2/README.md, "Deploying it"), its
# SQLite store and secrets on a separate persistent disk so the VM itself
# is disposable, exposed at a fixed DNS name through an IAP-protected
# HTTPS load balancer, with the service accounts v2's gcp-key/gemini-key
# capabilities need (plus, for staging, room to create VMs and GKE
# clusters too).
#
# Unlike terraform/gcp/ (v1: a controller running nested libvirt guests,
# meant to be forked as a generic, any-org deployment via templates/gcp/)
# this module is deliberately narrower: v2 has no host adapter yet
# (v2/README.md, "What this does not have yet") and its daemon already
# defaults to plain host directories for sandboxing (no nested
# virtualization needed at all), so there is no controller/sandbox fleet
# here to generalize over -- just the one VM v2/scripts/setup.sh already
# knows how to install and update.
#
# Nothing secret lives here, the same split terraform/gcp/variables.tf
# documents: the GitHub token, and (if minted) the GCP key-minter's own
# key, are never Terraform inputs -- see push-secrets.sh and instance.tf's
# own lifecycle.ignore_changes for how they reach the VM instead.

# ---------------------------------------------------------------- project --

variable "project_id" {
  type        = string
  description = "GCP project that holds the host VM, its disks, the load balancer, and its service accounts."
}

variable "region" {
  type        = string
  description = "Region for the subnet, the router, and the managed instance's zone."
  default     = "us-central1"
}

variable "zone" {
  type        = string
  description = "Zone for the host VM and its data disk."
  default     = "us-central1-a"
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name, so more than one deployment can share a project."
  default     = "grain-v2-staging"

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,20}[a-z0-9])?$", var.name_prefix))
    error_message = "name_prefix must be a lowercase RFC1035 label, 22 characters or fewer."
  }
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to the instance, disks, and service accounts."
  default = {
    managed-by  = "terraform"
    system      = "grain-v2"
    environment = "staging"
  }
}

# ------------------------------------------------------------------- host --

variable "machine_type" {
  type        = string
  description = <<-EOT
    Host machine type. Unlike terraform/gcp's v1 host, this VM does not
    run nested virtualization -- v2's daemon dispatches onto plain host
    directories by default (v2/README.md, "What this does not have
    yet") -- so any family works, including E2. e2-standard-2 (2 vCPU,
    8 GB) is comfortably enough to build grain (`make container-build`:
    a Go compile plus a Vite frontend build, both inside Docker) and run
    one daemon against a handful of test repos.
  EOT
  default     = "e2-standard-2"
}

variable "boot_image" {
  type        = string
  description = "Boot image for the host. v2's own scripts/setup.sh assumes Debian 12."
  default     = "debian-cloud/debian-12"
}

variable "boot_disk_gb" {
  type        = number
  description = <<-EOT
    Host boot disk: the OS, the grain checkout, the Docker build cache,
    and nothing that must survive a redeploy -- the SQLite store and
    secrets live on the separate data disk instead (see data_disk_gb),
    which is the whole point of splitting them: this disk can be
    recreated from scratch (a new boot_image, a bigger machine_type) with
    no state lost.
  EOT
  default     = 40
}

variable "data_disk_gb" {
  type        = number
  description = <<-EOT
    Persistent disk mounted at /var/lib/grain -- v2/scripts/setup.sh's
    own $GRAIN_DATA_DIR default, so no override is needed for the daemon
    to find it there. Holds the embedded SQLite store
    (pkg/model/sqlite), the secrets database and credential files
    (pkg/secrets), and the sandbox working directories
    orchestrator.HostSandboxes clones each task's repo into. 20 GB is
    generous for a staging deployment working against a handful of test
    repos; grow it if the sandbox directory becomes the bottleneck.
  EOT
  default     = 20
}

variable "data_disk_type" {
  type        = string
  description = "pd-balanced is the sane default for a staging workload; pd-ssd if I/O becomes the bottleneck."
  default     = "pd-balanced"
}

variable "enable_shielded_vm" {
  type        = bool
  description = <<-EOT
    Shielded VM (secure boot, vTPM, integrity monitoring). Unlike
    terraform/gcp's v1 host, this VM has no /dev/kvm dependency to
    conflict with it (see machine_type), so it defaults on here.
  EOT
  default     = true
}

variable "deploy_generation" {
  type        = string
  description = <<-EOT
    Opaque token written to instance metadata. files/config-sync.sh on
    the host watches it and redeploys whenever it changes; CI (or a
    human applying this by hand) sets it to the commit SHA of whatever
    grain_ref names, which is what makes a push roll out.
  EOT
  default     = "manual"
}

# ---------------------------------------------------------------- network --

variable "create_network" {
  type        = bool
  description = "Create a VPC and subnet. Set false to attach to an existing one."
  default     = true
}

variable "network_name" {
  type        = string
  description = "Existing network to attach to when create_network is false."
  default     = ""
}

variable "subnetwork_name" {
  type        = string
  description = "Existing subnet to attach to when create_network is false."
  default     = ""
}

variable "subnet_cidr" {
  type        = string
  description = "CIDR for the created subnet."
  default     = "10.30.0.0/24"
}

variable "assign_external_ip" {
  type        = bool
  description = <<-EOT
    Give the host its own public address. Off by default: the host is
    reached two ways either way -- the IAP-protected HTTPS load balancer
    (dns.tf, iap.tf) for the UI, and an IAP TCP tunnel for SSH
    (ssh_source_ranges) -- neither needs the instance to have an
    external IP of its own. Leave enable_cloud_nat on for its own
    outbound reach (GitHub, the Debian mirror, Docker Hub, the Gemini
    API).
  EOT
  default     = false
}

variable "enable_cloud_nat" {
  type        = bool
  description = "Create a Cloud Router and NAT so the host has egress despite assign_external_ip = false."
  default     = true
}

variable "ssh_source_ranges" {
  type        = list(string)
  description = <<-EOT
    Who may reach port 22 on the host. The default is Google's IAP
    range, so SSH goes through `gcloud compute ssh --tunnel-through-iap`
    and port 22 is never open to the internet.
  EOT
  default     = ["35.235.240.0/20"]
}

variable "enable_os_login" {
  type        = bool
  description = "IAM-driven SSH auth (roles/compute.osLogin) instead of manually managed keys."
  default     = true
}

variable "ui_port" {
  type        = number
  description = <<-EOT
    Port `grain daemon` serves the UI/API on, and the load balancer's
    backend port. Unlike v2/scripts/setup.sh's own GRAIN_UI_ADDR default
    (127.0.0.1:80, loopback-only -- see that script's own header), this
    deployment binds 0.0.0.0:ui_port: an external HTTPS load balancer
    reaches a backend over its *internal* IP, which loopback cannot
    answer on. That is safe here specifically because network.tf's
    firewall rule for this port only admits Google's own health-check
    and load-balancing source ranges (130.211.0.0/22, 35.191.0.0/16),
    not the internet at large -- the actual access control is IAP, on
    the load balancer in front of it (iap.tf), same as it would be for a
    loopback-bound service reached over a tunnel.
  EOT
  default     = 8080
}

# ------------------------------------------------------------------- IAM ---

variable "agent_account_id" {
  type        = string
  description = <<-EOT
    account_id (not email) of the narrow service account the gcp-key and
    gemini-key capabilities (v2/pkg/capability/gcpkey,
    v2/pkg/capability/geminikey) mint task credentials for. Defaults to
    pkg/gcpsetup.DefaultAgentAccountID -- the same name `grain setup gcp`
    would create on a fresh install -- purely so a deployment that only
    ever runs one of these per project needs no extra daemon flag beyond
    what deploy.sh already sets from this module's own output. Override
    it to run more than one v2 deployment (staging and something else)
    in the same GCP project.
  EOT
  default     = "grain-agent"
}

variable "minter_account_id" {
  type        = string
  description = <<-EOT
    account_id of the standing service account that mints and revokes
    the agent account's per-task keys, and (with enable_gemini_key)
    administers API keys project-wide. Defaults to
    pkg/gcpsetup.DefaultMinterAccountID for the same reason
    agent_account_id does.
  EOT
  default     = "grain-gcp-key-minter"
}

variable "enable_gemini_key" {
  type        = bool
  description = <<-EOT
    Grants the minter account roles/serviceusage.apiKeysAdmin on the
    project and enables generativelanguage.googleapis.com and
    apikeys.googleapis.com -- what pkg/capability/geminikey needs to
    mint a short-lived Gemini API key for a task that asks for one. On
    by default: bwsalmon/agents#394 asks this staging deployment to be
    able to hand out Gemini keys.
  EOT
  default     = true
}

variable "agent_can_manage_compute_instances" {
  type        = bool
  description = <<-EOT
    Grants the agent account create/delete/start/stop and SSH access to
    Compute Engine instances -- roles/compute.instanceAdmin.v1,
    roles/compute.osLogin, and (project-wide, see iam.tf's own comment
    on why it cannot be conditioned the same way) roles/iap.tunnelResourceAccessor
    -- everywhere in the project except this deployment's own host VM.
    Also enables compute.googleapis.com. On by default: bwsalmon/agents#394
    asks this staging deployment to be able to create VMs.
  EOT
  default     = true
}

variable "agent_can_manage_gke" {
  type        = bool
  description = <<-EOT
    Grants the agent account roles/container.admin and
    roles/artifactregistry.admin, project-wide, plus
    roles/iam.serviceAccountUser on itself (needed to attach its own
    identity to a GKE node pool -- see iam.tf's own comment,
    bwsalmon/agents#146 in terraform/gcp/iam.tf found this live for v1).
    Also enables container.googleapis.com and
    artifactregistry.googleapis.com. On by default: bwsalmon/agents#394
    asks this staging deployment to be able to create GKE clusters.
  EOT
  default     = true
}

variable "deployer_member" {
  type        = string
  description = <<-EOT
    The identity applying this Terraform (e.g. "user:you@example.com" or
    "serviceAccount:ci@project.iam.gserviceaccount.com"), granted
    roles/iam.serviceAccountKeyAdmin on the minter account so that
    push-secrets.sh -- run separately, after `terraform apply`, by this
    same identity -- can mint the minter's own key without that key ever
    passing through Terraform state (the same reasoning
    terraform/gcp/iam.tf's deployer_manages_agent_keys already applies to
    v1). Required whenever a key needs minting at all: enable_gemini_key
    or agent_can_manage_compute_instances or agent_can_manage_gke, since
    each needs the agent account to work with a real key in place.
  EOT
  default     = ""
}

# ----------------------------------------------------------------- grain ---

variable "grain_repo_url" {
  type        = string
  description = "Clone URL for grain itself."
  default     = "https://github.com/bwsalmon/grain"
}

variable "grain_ref" {
  type        = string
  description = "Branch, tag, or commit of grain to deploy. Pin a tag or SHA for a reproducible staging build."
  default     = "main"
}

variable "test_repos" {
  type        = list(string)
  description = <<-EOT
    owner/name of the handful of test repos this staging deployment
    works against -- what "assume a scoped PAT to a few test repos"
    (bwsalmon/agents#394) means in practice: the PAT itself (pushed by
    push-secrets.sh, never a Terraform input) is a GitHub fine-grained
    token scoped, on GitHub's side, to exactly these repositories, which
    is one enforcement boundary. This is now also the source of the
    daemon's own -target-repos allow-list (v2/pkg/model.Config.TargetRepos,
    bwsalmon/agents#399), the same "allowlist a task naming anything else
    is parked with a comment rather than dispatched against" v1's
    terraform/gcp/variables.tf target_repos documents -- so a task
    naming a repo outside this list is refused by grain itself, not
    just by GitHub declining the PAT's own reach. Empty leaves the
    daemon's own allow-list empty too, i.e. unrestricted, the same
    default_target_repo's own validation below already treats it.
  EOT
  default     = []
}

variable "default_target_repo" {
  type        = string
  description = "owner/name a task with no /repo directive targets. Empty leaves the daemon with no default."
  default     = ""

  validation {
    condition     = var.default_target_repo == "" || contains(var.test_repos, var.default_target_repo)
    error_message = "default_target_repo must be one of test_repos (or left empty)."
  }
}

variable "credential_name" {
  type        = string
  description = "secrets/github credential name the scoped PAT is stored under -- v2/scripts/setup.sh's own GRAIN_GITHUB_CREDENTIAL_NAME."
  default     = "bot"
}

variable "github_host" {
  type        = string
  description = "GitHub API host. Override only to point staging at a mock server."
  default     = "github.com"
}

variable "gemini_model" {
  type        = string
  description = "Gemini model override. Empty uses the daemon's own default (pkg/agent/gemini.DefaultModel)."
  default     = ""
}

variable "slots" {
  type        = string
  description = "Comma-separated concurrency slot names -- v2/scripts/setup.sh's own GRAIN_SLOTS."
  default     = "local"
}

variable "poll_interval" {
  type        = string
  description = "How often the daemon runs a reconcile cycle."
  default     = "30s"
}

# --------------------------------------------------------------------- dns --

variable "dns_managed_zone" {
  type        = string
  description = <<-EOT
    Name of an existing Cloud DNS *public* managed zone to add an A
    record to, pointed at this deployment's reserved static IP
    (dns.tf). Leave empty (the default) and dns.tf instead derives a
    name from the reserved IP itself using sslip.io, a public DNS
    service that resolves "<anything>.<a>.<b>.<c>.<d>.sslip.io" to
    a.b.c.d with no zone or registration needed on this project's part
    -- fine for a staging environment, per bwsalmon/agents#394's own "it
    doesn't matter what it is." Set this to use a real domain you
    control instead.
  EOT
  default     = ""
}

# ------------------------------------------------------------------- IAP ---

variable "iap_members" {
  type        = list(string)
  description = <<-EOT
    Identities granted roles/iap.httpsResourceAccessor on the load
    balancer's backend service (iap.tf) -- who may actually reach the
    UI once they are through Google's own sign-in, in the form IAM
    expects: "user:you@example.com", "group:staging-access@example.com",
    or "domain:example.com". Left empty (the default) grants nobody
    access -- a safe-but-useless default; set this before the first
    apply, or the environment is up but nobody can reach it.
  EOT
  default     = []
}

variable "create_iap_brand" {
  type        = bool
  description = <<-EOT
    Create the project's IAP OAuth brand and an OAuth client for it
    (iap.tf), instead of letting IAP use its own Google-managed client.

    Almost certainly leave this false, and leave iap_client_id and
    iap_client_secret unset with it: IAP uses a Google-managed OAuth
    client for browser access when no client is configured, which is why
    the IAP OAuth Admin API was deprecated in January 2025 and why a
    custom client is no longer part of ordinary setup. The provider made
    the backend service's client fields optional in 6.0; this module
    requires ~> 6.8, so the no-client path is available here.

    Two reasons not to turn this on even when you do want a custom
    client:

    1. A GCP project may have at most one brand, ever, and the API has
       no call to delete one, so this only ever works once per project.
    2. The provider itself warns that `google_iap_brand` "will no longer
       function as intended due to the deprecation of the IAP OAuth
       Admin API" -- `terraform validate` surfaces that warning
       directly.

    So for a custom client, create it by hand once (the GCP Console's
    "Google Auth Platform" page -- check current GCP guidance, this area
    of the product has moved) and pass it as iap_client_id and
    iap_client_secret below, rather than setting this.
  EOT
  default     = false
}

variable "iap_brand_support_email" {
  type        = string
  description = "Support email for the IAP OAuth consent screen. Required when create_iap_brand is true; must be a member of the project's org (or the caller's own address, for a project with no org)."
  default     = ""
}

variable "iap_brand_application_title" {
  type        = string
  description = "Application title shown on the IAP OAuth consent screen."
  default     = "grain v2 (staging)"
}

variable "iap_client_id" {
  type        = string
  description = <<-EOT
    An existing IAP OAuth client id, for a deployment that wants its own
    client rather than the Google-managed one. Optional, and normally
    left empty -- see create_iap_brand above. Set this and
    iap_client_secret together; setting only one is the same as setting
    neither, since the backend service omits both unless both are
    present.
  EOT
  default     = ""
}

variable "iap_client_secret" {
  type        = string
  description = <<-EOT
    The secret for iap_client_id. Optional, and normally left empty --
    see create_iap_brand above. Sensitive: pass via -var or a tfvars
    file kept out of version control, not a committed one.
  EOT
  default     = ""
  sensitive   = true
}
