# Where Terraform keeps its state. Created once by grain's own
# terraform/gcp/bootstrap-gcp.sh -- run from a grain checkout, not from
# this repo -- which prints the exact values to paste here.
#
# The bucket holds the state file, which records every resource this repo
# manages. It records no secret values -- Terraform never sees them -- but
# lock it down anyway: uniform bucket-level access, versioning on, and no
# public access. The bootstrap script does all three.

bucket = "CHANGE-ME-grain-tfstate"
prefix = "grain/prod"
