# Copy to something like staging.tfvars, fill in the CHANGE-ME values,
# and apply with:
#
#   terraform init -backend-config=backend.hcl.example   # copy and fill in too
#   terraform apply -var-file=staging.tfvars
#
# Nothing secret belongs here -- see this directory's README, "Secrets
# never touch Terraform."

project_id = "CHANGE-ME-gcp-project"

# The identity running `terraform apply` -- a human's own account for a
# by-hand deployment, or bootstrap-gcp.sh's own deployer service account
# for CI. Needed so push-secrets.sh (run separately, by this same
# identity) can mint a key for the minter account afterward.
deployer_member = "user:CHANGE-ME@example.com"

# -- Which repos this staging deployment works on ---------------------
#
# The scoped PAT you push with push-secrets.sh should be a GitHub
# fine-grained token limited to exactly these repositories. This is also
# wired into the daemon's own -target-repos allow-list -- see
# variables.tf's own test_repos and this module's README, "Repo
# enforcement."
test_repos = [
  # "CHANGE-ME/test-repo-one",
  # "CHANGE-ME/test-repo-two",
]
default_target_repo = "" # e.g. "CHANGE-ME/test-repo-one"

# -- Who may reach the UI once IAP has authenticated them --------------

iap_members = [
  # "user:CHANGE-ME@example.com",
  # "group:CHANGE-ME@example.com",
]

# See variables.tf's own create_iap_brand: false (the default) needs an
# OAuth client created by hand once; true creates one via Terraform,
# which the provider itself currently warns may not work for a genuinely
# new brand.
create_iap_brand = false
# iap_client_id     = "CHANGE-ME.apps.googleusercontent.com"
# iap_client_secret = "CHANGE-ME"

# Leave empty to get a working https://<something>.sslip.io URL with no
# domain of your own -- see variables.tf's dns_managed_zone.
# dns_managed_zone = "CHANGE-ME-zone"

grain_ref = "main" # pin a tag or SHA for a reproducible staging build

# -- Where this deployment's state lives --------------------------------
#
# The repository grain keeps its database in, as text: settings,
# templates, suites and the encrypted secrets file, imported at startup
# and exported back on a timer. Set it and the host comes up pointed at
# it, with nobody opening the UI's bootstrap pane to say where state
# goes. Left empty (the default) state stays in a local-only repository
# on the host, which a rebuilt VM does not get back.
#
# The secrets key is the one file that repository cannot carry for you --
# it is what decrypts the secrets *in* it. See this directory's README,
# "The secrets key is the one file a redeploy must carry."
# state_repo_url    = "https://github.com/CHANGE-ME/grain-state"
# state_repo_branch = "main"

# -- Storage -----------------------------------------------------------
#
# Everything a sandbox writes -- docker's data root, and so the sandbox
# image and every VM's disk overlay, plus HostSandboxes' own per-run
# checkouts -- lives on a volume of its own, so a task that fills a disk
# does not take the OS, the store and the UI down with it. Grow it for a
# busier deployment; 0 puts all of it back on the boot disk, which then
# has to be sized for it. See variables.tf's own sandbox_disk_gb.
# sandbox_disk_gb = 100

# -- Kontur sandboxing --------------------------------------------------
#
# enable_kontur_sandboxes defaults to true (variables.tf) and needs
# nothing published first: the sandbox image is pulled, and the grain
# image names the one it was built against. The guest is inside that same
# image, so there is no second artifact -- see this directory's README,
# "Kontur sandboxing". The override below runs a sandbox image of your
# own choosing. Or set enable_kontur_sandboxes = false to keep this
# deployment on host-directory sandboxing (bwsalmon/agents#504) instead.
# kontur_oci_image    = "us-central1-docker.pkg.dev/CHANGE-ME-gcp-project/CHANGE-ME-repo/kontur:latest"
# enable_kontur_sandboxes = false
