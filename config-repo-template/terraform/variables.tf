# Every value here is *configuration*, not a secret: it is set in
# config/grain.tfvars, committed, and reviewed in a pull request. The two
# secrets this deployment needs (a GitHub token and a Claude Code OAuth
# token) never appear in Terraform -- see secrets.tf.

# ---------------------------------------------------------------- project --

variable "project_id" {
  type        = string
  description = "GCP project that holds the host VM, its disks, and its secrets."
}

variable "region" {
  type        = string
  description = "Region for the subnet, the router, and the Secret Manager replicas."
  default     = "us-central1"
}

variable "zone" {
  type        = string
  description = "Zone for the host VM and its data disk."
  default     = "us-central1-a"
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name, so two deployments can share a project."
  default     = "grain"

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,20}[a-z0-9])?$", var.name_prefix))
    error_message = "name_prefix must be a lowercase RFC1035 label, 22 characters or fewer."
  }
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to the instance, disks, and secrets."
  default     = { managed-by = "terraform", system = "grain" }
}

# ------------------------------------------------------------------- host --

variable "machine_type" {
  type        = string
  description = <<-EOT
    Host machine type. It runs the controller VM plus every sandbox as
    nested guests, so it must be a family that supports nested
    virtualization -- N1, N2, N2D, C2, C3, or M-series. E2 does not.
    docs/design.md sizes the reference deployment at n2-highmem-4.
  EOT
  default = "n2-highmem-4"
}

variable "boot_image" {
  type        = string
  description = "Boot image for the host. grain's provisioning assumes Debian 12."
  default     = "debian-cloud/debian-12"
}

variable "boot_disk_gb" {
  type        = number
  description = "Host boot disk. Holds the OS only; guest disks live on the data disk."
  default     = 50
}

variable "data_disk_gb" {
  type        = number
  description = <<-EOT
    Persistent disk mounted at /var/lib/grain. Holds the base image, guest
    disks, the admin SSH key, and -- inside the controller's disk -- /data
    with every credential and all automation state. Size it for the base
    image plus each guest's disk_gb.
  EOT
  default = 200
}

variable "data_disk_type" {
  type        = string
  description = "pd-balanced is the sane default; pd-ssd if guest I/O is the bottleneck."
  default     = "pd-balanced"
}

variable "enable_shielded_vm" {
  type        = bool
  description = <<-EOT
    Shielded VM (secure boot, vTPM, integrity monitoring). Left off by
    default because it has not been verified against nested
    virtualization on this image; turn it on and confirm /dev/kvm still
    appears before relying on it.
  EOT
  default = false
}

variable "deploy_generation" {
  type        = string
  description = <<-EOT
    Opaque token written to instance metadata. The on-VM config-sync
    service watches it and redeploys whenever it changes; CI sets it to
    the commit SHA, which is what makes a push to this repo roll out.
  EOT
  default = "manual"
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
  description = <<-EOT
    CIDR for the created subnet. Must not overlap the *guest* subnet grain
    runs inside the host (10.100.0.0/24 by default, see cluster.toml).
  EOT
  default = "10.20.0.0/24"
}

variable "assign_external_ip" {
  type        = bool
  description = <<-EOT
    Give the host a public address. It needs outbound reach to GitHub, the
    Debian mirror, and the Anthropic API either way; set this false and
    enable_cloud_nat true to get that without an inbound-reachable address.
  EOT
  default = true
}

variable "enable_cloud_nat" {
  type        = bool
  description = "Create a Cloud Router and NAT so a host with no external IP still has egress."
  default     = false
}

variable "ssh_source_ranges" {
  type        = list(string)
  description = <<-EOT
    Who may reach port 22 on the host. The default is Google's IAP range,
    so SSH goes through `gcloud compute ssh --tunnel-through-iap` and port
    22 is not open to the internet. Add your office CIDR to widen it.
  EOT
  default = ["35.235.240.0/20"]
}

# ------------------------------------------------------------------- IAM ---

variable "vm_service_account_roles" {
  type        = list(string)
  description = <<-EOT
    Project roles granted to the host's own service account -- the whole
    point of giving it one. Keep it to what the host itself needs:
    log/metric writing, and reading the two secrets below (granted
    separately, on the secrets themselves, not project-wide).
    Anything an *agent* should be able to do belongs in
    agent_service_account_roles instead.
  EOT
  default = [
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
  ]
}

variable "agent_service_account_roles" {
  type        = list(string)
  description = <<-EOT
    Project roles for a second, narrow service account that sandboxed
    agents get tokens for, via the controller's metadata server
    impersonating it (docs/design.md, "GCP credentials"). Leave empty and
    no such account is created. Non-empty and the host account is granted
    roles/iam.serviceAccountTokenCreator on it -- but see the README:
    pointing grain's metadata server at it still needs one manual step.
  EOT
  default = []
}

# ----------------------------------------------------------------- grain ---

variable "grain_repo_url" {
  type        = string
  description = "Public clone URL for grain itself. Cloned unauthenticated on the host."
  default     = "https://github.com/bwsalmon/grain"
}

variable "grain_ref" {
  type        = string
  description = "Branch, tag, or commit of grain to deploy. Pin a tag or SHA for a stable deployment."
  default     = "main"
}

variable "debian_image_url" {
  type        = string
  description = "Base qcow2 the guests are provisioned from, fetched once onto the data disk."
  default     = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
}

variable "sandbox_count" {
  type        = number
  description = "How many sandbox VMs. Two fits n2-highmem-4; CPU binds before memory."
  default     = 2
}

variable "cluster_overrides" {
  type        = map(string)
  description = <<-EOT
    Extra keys written verbatim into /var/lib/grain/cluster.toml -- any
    field of grain's Cluster dataclass (subnet, bridge, controller_cpus,
    sandbox_mem_mb, ...). Values are emitted as TOML: bare if they look
    numeric, quoted otherwise.
  EOT
  default = {}
}

variable "task_repo" {
  type        = string
  description = "owner/name of the one repo polled for labelled issues."
}

variable "target_repos" {
  type        = list(string)
  description = <<-EOT
    Repos a task may dispatch into, named by a /repo directive on an
    issue. Leave empty for a single-repo deployment: the task repo
    becomes the only target.
  EOT
  default = []
}

variable "default_target_repo" {
  type        = string
  description = "Target repo for a task with no /repo directive. Empty means park such tasks."
  default     = ""
}

variable "credential_name" {
  type        = string
  description = "credentials.json entry name for the GitHub token."
  default     = "bot"
}

variable "deploy_timeout_minutes" {
  type        = number
  description = "How long the on-VM deploy may run before config-sync gives up and reports failure."
  default     = 45
}
