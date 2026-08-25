# ---------------------------------------------------------------------------
# The whole deployment, as configuration. Edit this file, open a pull
# request, read the plan CI posts on it, merge -- and the host converges.
#
# Nothing secret belongs here. The two credentials grain needs live in this
# repo's Actions secrets and go straight into the host's own instance
# metadata; see the README.
# ---------------------------------------------------------------------------

# -- Where it runs ----------------------------------------------------------

project_id  = "CHANGE-ME-gcp-project"
region      = "us-central1"
zone        = "us-central1-a"
name_prefix = "grain"

# n2-highmem-4 (4 vCPU, 32 GB) is the shape docs/design.md sizes against and
# the one that has been load-tested with two sandboxes. It must be a family
# that supports nested virtualization -- E2 does not.
machine_type   = "n2-highmem-4"
boot_disk_gb   = 50
data_disk_gb   = 200
data_disk_type = "pd-balanced"

# -- What it runs -----------------------------------------------------------

grain_repo_url = "https://github.com/bwsalmon/grain"

# "main" tracks the tip; a tag or a full commit SHA pins the deployment.
# Pinning is the better default once you have something you care about.
grain_ref = "main"

sandbox_count = 2

# Any other field of grain's Cluster inventory, written verbatim into
# /var/lib/grain/cluster.toml. The guest subnet must not overlap
# subnet_cidr below.
cluster_overrides = {
  # subnet          = "10.100.0.0/24"
  # sandbox_mem_mb  = 8192
  # sandbox_cpus    = 2
  # controller_mem_mb = 4096
}

# -- Which repos the agents work on -----------------------------------------

# The queue is *this repository*: file an issue here, label it
# `grain-agent`, and the next polling pass picks it up. CI passes the repo
# name automatically, so there is nothing to set. Uncomment only to point
# the deployment at a separate task repo instead.
#
# task_repo = "CHANGE-ME/agent-tasks"

# Repos a task may dispatch into, named by a /repo directive in the issue:
#
#     Something is broken in the widget service.
#     /repo my-org/widget-service
#
# Leave this empty and there is no such thing as a target elsewhere: tasks
# act on this repository itself, which is the self-managing mode -- read
# the README's note about it before choosing it deliberately.
target_repos = []

# Where a task with no /repo directive goes. It must be one of the
# target_repos above. Empty parks such tasks with a comment rather than
# guessing which repo was meant.
default_target_repo = ""

# credentials.json entry name for the GitHub token.
credential_name = "bot"

# -- What the VM is allowed to do -------------------------------------------

# Roles on the host's own service account. It needs no Secret Manager
# grant -- its two runtime credentials arrive as instance metadata. Add
# to this list to widen what the host may do -- it is a reviewable diff,
# which is the point of keeping it here.
vm_service_account_roles = [
  "roles/logging.logWriter",
  "roles/monitoring.metricWriter",
]

# Roles for the *narrow* account sandboxed agents get tokens for, via the
# controller's metadata server. Leave empty and no such account exists.
# Grant only what an agent genuinely needs -- a compromised sandbox gets
# exactly this list.
agent_service_account_roles = [
  # "roles/storage.objectViewer",
]

# Grants the narrow agent account above roles/serviceusage.apiKeysAdmin and
# enables the Generative Language API, so a task labelled `grain-gemini-key`
# can have a short-lived Gemini API key minted for it. Creates the agent
# account even if agent_service_account_roles above is left empty. See
# grain's docs/runbook.md, "Enabling grain-gemini-key".
enable_gemini_key = false

# -- Network ----------------------------------------------------------------

create_network = true
subnet_cidr    = "10.20.0.0/24"

# The host needs egress (Debian mirror, GitHub, the Anthropic API). With an
# external IP it has it directly; without one, set enable_cloud_nat = true.
assign_external_ip = true
enable_cloud_nat   = false

# Port 22, and only from Google's IAP range by default, so the host is not
# reachable from the internet:
#   gcloud compute ssh grain-host --tunnel-through-iap
# Add your own CIDR here to SSH directly.
ssh_source_ranges = ["35.235.240.0/20"]

# Untested against nested virtualization on this image; see variables.tf.
enable_shielded_vm = false

# Project-wide organization-policy guardrails: no VM in the project may get
# an external IP, and no bucket may be made public. Holds for every
# identity in the project, not just this deployment's own -- a second,
# blunter lock alongside the IAM roles above. Needs assign_external_ip =
# false first (see variables.tf's lock_down_project).
lock_down_project = false

# How long the on-host deploy may run before it reports failure. A first
# deploy downloads a base image and boots the controller plus every sandbox.
deploy_timeout_minutes = 45

labels = {
  managed-by = "terraform"
  system     = "grain"
}
