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
