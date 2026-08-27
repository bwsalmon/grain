# Every value here is *configuration*, not a secret: it is set in
# config/grain.tfvars, committed, and reviewed in a pull request. The two
# secrets this deployment needs (a GitHub token and a Claude Code OAuth
# token) never appear in Terraform -- the deploy workflow pushes them
# straight into instance metadata after `terraform apply` returns, so
# they are never in the plan, the apply, or the state file.

# ---------------------------------------------------------------- project --

variable "project_id" {
  type        = string
  description = "GCP project that holds the host VM, its disks, and its secrets."
}

variable "region" {
  type        = string
  description = "Region for the subnet and the router."
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
  default     = "n2-highmem-4"
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
  default     = 200
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
  default     = false
}

variable "deploy_generation" {
  type        = string
  description = <<-EOT
    Opaque token written to instance metadata. The on-VM config-sync
    service watches it and redeploys whenever it changes; CI sets it to
    the commit SHA, which is what makes a push to this repo roll out.
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
  description = <<-EOT
    CIDR for the created subnet. Must not overlap the *guest* subnet grain
    runs inside the host (10.100.0.0/24 by default, see cluster.toml).
  EOT
  default     = "10.20.0.0/24"
}

variable "assign_external_ip" {
  type        = bool
  description = <<-EOT
    Give the host a public address. It needs outbound reach to GitHub, the
    Debian mirror, and the Anthropic API either way; set this false and
    enable_cloud_nat true to get that without an inbound-reachable address.
  EOT
  default     = true
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
  default     = ["35.235.240.0/20"]
}

variable "enable_os_login" {
  type        = bool
  description = <<-EOT
    IAM-driven SSH auth instead of manually managed keys -- the more
    secure default, but it means every SSH session needs
    roles/compute.osLogin, or roles/compute.osLoginExternalUser (an
    organization-level grant) for an identity outside this project's
    org -- found live: a real operator hit exactly that wall and had no
    way to self-grant it. Set false to fall back to the classic
    SSH-key-based path gcloud compute ssh already uses when OS Login is
    off, no IAM role needed. ssh_source_ranges still gates who can reach
    port 22 either way.
  EOT
  default     = true
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
    Project roles for a second, narrow service account that a sandboxed
    agent's own GCP credentials belong to: the controller mints a
    fresh, short-lived key for it on every dispatch and pushes it into
    the sandbox, revoking it once the task ends (docs/design.md, "GCP
    credentials"; bwsalmon/agents#126). Leave empty and no such account is
    created (unless agent_can_manage_compute_instances is true -- see
    below). Non-empty and the host account is granted
    roles/iam.serviceAccountKeyAdmin on it, so the controller can mint and
    revoke those keys itself -- but see the README: turning this feature
    on for a deployed host still needs one manual step (`grain controller
    configure --gcp-agent-service-account-email ... --gcp-project-id ...`).
  EOT
  default     = []
}

variable "enable_gemini_key" {
  type        = bool
  description = <<-EOT
    Grants the agent account (creating it even if agent_service_account_roles
    is left empty, the same way agent_can_manage_compute_instances already
    does) roles/serviceusage.apiKeysAdmin on the project, and enables the
    Generative Language API (generativelanguage.googleapis.com) -- the two
    things grain/automation/gemini_keys.py needs to mint and revoke a
    short-lived Gemini API key for a task carrying the grain-gemini-key
    label (docs/runbook.md, "Enabling grain-gemini-key"; bwsalmon/agents#47,
    #49).

    This only covers the permissions and account -- it does not by itself
    turn the feature on. That still needs the deploy workflow to have
    minted and placed the agent account's key (it does automatically once
    that account exists, the same way it already does for the metadata
    broker) and grain's own gemini-key.json switch, which the on-host
    deploy writes automatically when this is true (see instance.tf's
    grain_config and terraform/gcp/files/deploy.sh).

    Applying this needs roles/serviceusage.serviceUsageAdmin on the
    deployer running Terraform, to enable the API -- bootstrap-gcp.sh
    grants it.
  EOT
  default     = false
}

variable "agent_can_manage_compute_instances" {
  type        = bool
  description = <<-EOT
    Grants the agent account (creating it even if agent_service_account_roles
    is left empty) create/delete/start/stop and SSH access to Compute
    Engine instances -- roles/compute.instanceAdmin.v1,
    roles/compute.osLogin, and roles/iap.tunnelResourceAccessor.

    The first two exclude the grain host VM itself by IAM condition (see
    iam.tf's agent_compute local): an agent cannot touch its own
    deployment's instance, add an SSH key to it, or provision an OS Login
    account on it. iap.tunnelResourceAccessor cannot be conditioned the
    same way -- GCP does not reliably support excluding one instance from
    a project-level grant of that specific role (confirmed live: doing so
    denied *all* tunnel access rather than excluding just the one
    instance) -- so it is granted project-wide. That role alone only
    opens a network tunnel to an instance's SSH port; it grants no
    authentication capability by itself, and the two excluded roles above
    are what would actually let an agent log in, so the host stays
    unreachable in practice despite the tunnel role being unconditioned.
  EOT
  default     = false
}

variable "agent_can_manage_gke" {
  type        = bool
  description = <<-EOT
    Grants the agent account (creating it even if agent_service_account_roles
    is left empty) the ability to create, resize, and delete GKE clusters
    and node pools, and to create, push to, and delete Artifact Registry
    repositories -- roles/container.admin and roles/artifactregistry.admin,
    both project-wide. Also enables the two APIs those roles need
    (container.googleapis.com, artifactregistry.googleapis.com) --
    otherwise the roles exist but every call fails with API-not-enabled,
    the same trap enable_gemini_key already avoids for
    generativelanguage.googleapis.com.

    Unlike agent_can_manage_compute_instances, this grants no IAM
    condition excluding the grain host: the host is a Compute Engine
    instance, not a GKE cluster or an Artifact Registry repository, so
    there is no equivalent "the deployment's own resource" for an agent
    to be barred from touching here. A cluster an agent creates is a
    project resource like any other Terraform-managed one, indistinguishable
    from one a human made by hand -- keep that in mind when deciding
    whether this project is one agents should be allowed to run
    workloads in.

    roles/container.admin includes full access to whatever Kubernetes API
    objects run inside a cluster it can reach (not just cluster lifecycle),
    since that is how GKE's own predefined roles are scoped -- there is no
    narrower predefined role that covers create/resize/delete of clusters
    without also covering what runs on them.

    Also grants the agent account roles/iam.serviceAccountUser on itself
    (iam.tf's agent_acts_as_self_for_gke_nodes) -- confirmed live,
    bwsalmon/agents#146: container.admin alone is not enough to create a
    cluster. GKE node pools run as some service account, and the caller
    needs iam.serviceAccountUser on whichever one gets attached, or cluster
    creation fails with a 400 naming it. A cluster this account creates
    should pass --service-account=<this account's email> explicitly to use
    this grant, rather than defaulting to the project's Compute Engine
    default service account -- that account often carries broader legacy
    project roles than this one, so using it for nodes would be a
    privilege escalation for anything running as a pod, where using this
    account's own (already fully known) identity is not.

    Applying this needs roles/serviceusage.serviceUsageAdmin on the
    deployer running Terraform, to enable the two APIs -- bootstrap-gcp.sh
    grants it.
  EOT
  default     = false
}

# ------------------------------------------------------------- lockdown ---

variable "lock_down_project" {
  type        = bool
  description = <<-EOT
    Project-wide organization-policy guardrails against external exposure:
    no VM in the project -- this deployment's own host included -- may be
    given an external IP (constraints/compute.vmExternalIpAccess, denied
    for all instances), and no bucket in the project may be made publicly
    readable (constraints/storage.publicAccessPrevention, enforced).

    Unlike vm_service_account_roles or agent_service_account_roles above,
    this is not an IAM grant -- it holds regardless of which identity is
    acting, including a human operator's own gcloud session or an agent
    that somehow escalated past agent_can_manage_compute_instances's
    per-instance IAM condition. That is what makes it worth having
    alongside the IAM roles above rather than instead of reviewing them:
    a second, blunter lock, not a replacement for the first.

    "External IP" and "public bucket" are what GCP's organization-policy
    system actually has a constraint for at the project level -- there is
    no constraint for "no bucket may be created" outright, only for how
    one may be configured once it exists, which for a bucket is almost
    always what "locked down" is really asking for.

    Off by default, and setting this true together with
    assign_external_ip = true fails the plan rather than the apply -- see
    instance.tf's precondition -- because the policy would deny the
    host's own external IP the moment it took effect. Set
    assign_external_ip = false (and enable_cloud_nat = true, for egress)
    first.

    Applying this needs roles/orgpolicy.policyAdmin on the deployer
    running Terraform -- bootstrap-gcp.sh grants it.
  EOT
  default     = false
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
  default     = {}
}

variable "config_repo" {
  type        = string
  description = <<-EOT
    owner/name of *this* repository. CI passes it automatically from
    github.repository; set it in the tfvars only if you run Terraform by
    hand. It is the default task repo -- see task_repo.
  EOT
  default     = ""
}

variable "task_repo" {
  type        = string
  description = <<-EOT
    owner/name of the repo polled for labelled issues. Empty -- the
    default -- means this repository: the config repo is also the queue,
    so an issue filed here and labelled `grain-agent` becomes a task. Set
    it only to point the deployment at a separate task repo.
  EOT
  default     = ""
}

variable "target_repos" {
  type        = list(string)
  description = <<-EOT
    Repos a task may dispatch into, named by a /repo directive on an
    issue. Leave empty for a single-repo deployment: the task repo
    becomes the only target.
  EOT
  default     = []
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

variable "bootstrap_ssh_timeout_seconds" {
  type        = number
  description = "How long grain host bootstrap waits for a freshly created VM (the controller, then each sandbox) to answer SSH at all, before giving up on it -- grain's own --ssh-timeout, default 180s. Nested virtualization on a real cloud VM can take longer than that to boot cold, well within deploy_timeout_minutes's much larger budget."
  default     = 600
}

# ---------------------------------------------------------------- janitor --

variable "enable_janitor" {
  type        = bool
  description = <<-EOT
    Runs a periodic janitor in the controller (bwsalmon/agents#113) that
    deletes GCE instances, their unattached disks, and grain-minted Gemini
    API keys older than janitor_ttl_hours -- cleanup for whatever an agent
    creates in GCP as part of a task and never tears down itself. Skips the
    grain host VM, its data disk (by name), and anything carrying this
    deployment's own labels (default managed-by=terraform) -- see
    grain/automation/janitor.py's own docstring for the full safety model.

    Creates the agent account even if agent_service_account_roles is left
    empty, agent_can_manage_compute_instances is false, and enable_gemini_key
    is false, the same way those already do for each other -- but the
    janitor only has anything to clean up once agent_can_manage_compute_
    instances and/or enable_gemini_key actually grant it the roles to list
    and delete something; turning this on alone is a harmless no-op that
    just logs a listing failure each cycle.
  EOT
  default     = false
}

variable "janitor_ttl_hours" {
  type        = number
  description = <<-EOT
    How old (in hours) a GCE instance, an unattached disk, or a grain-
    minted Gemini API key must be before enable_janitor's janitor deletes
    it. Only meaningful when enable_janitor is true.
  EOT
  default     = 24
}
