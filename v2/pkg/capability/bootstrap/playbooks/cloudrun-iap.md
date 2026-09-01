# Bootstrap: CloudRun-based IAP access

Goal: stand up the IAP-protected path to this deployment's UI described
in `terraform/gcp-v2/iap.tf` and `terraform/gcp-v2/cloudrun-proxy.tf` --
a global HTTPS load balancer, Identity-Aware Proxy in front of it, and
(when `use_cloudrun_iap_proxy` is on) a Cloud Run service that forwards
to the hand-created VM's internal IP over a Serverless VPC Access
connector, rather than the load balancer talking to the VM's own
instance group directly. There is no non-Terraform path for this one --
unlike GCP service accounts (see gcp-capabilities.md), nothing in this
repo builds IAP/Cloud Run resources through a plain SDK call, so this
playbook's Terraform apply is not a choice, it's the only mechanism.

Read this whole playbook before running anything. Every shell command
below is something you (the agent) run through the `run_host_command`
tool, one at a time -- each one is posted into this chat and waits for a
human "approve" before it actually runs.

## 1. Do this after gcp-capabilities.md, not before

If this deployment hasn't bootstrapped its GCP service accounts yet,
read and run that playbook first: this one shares the same Terraform
working directory, backend, and master-credential pattern, and layering
IAP on top of a project with no agent/minter accounts yet is not useful
on its own.

## 2. Ask before touching anything

Confirm with whoever you're talking to:

- who should be able to reach the UI -- their Google identities or
  group, for `iap_members` (an empty list is a valid plan, but nobody
  can sign in until it names someone)
- the DNS name this should be reachable at (`ui_domain`), if
  `expose_ui_publicly` is wanted at all -- the alternative is IAP's own
  TCP tunnel (`gcloud compute start-iap-tunnel`, or -- with the Cloud
  Run proxy -- the equivalent through `gcloud run services proxy`),
  which needs no public DNS or load balancer at all
- whether they want the Cloud Run proxy (`use_cloudrun_iap_proxy`) or
  the load balancer backed by the VM's own instance group directly

If they only want the tunnel, tell them they don't need this playbook --
IAP's tunnel command works once `iap.tf`'s IAM member grant exists,
which is a much smaller change than the whole load balancer / Cloud Run
stack below.

## 3. Master credential and local state -- same pattern as gcp-capabilities.md

Reuse that playbook's steps 2 and 3 verbatim: a master credential placed
on the host out of band (never pasted into this chat), activated with
`gcloud auth activate-service-account`, and the same
`terraform/gcp-v2` working directory initialized against
`backend-local.hcl` with state at
`$GRAIN_DATA_DIR/terraform-state/gcp-v2.tfstate`. If that playbook
already ran in this conversation (or a previous one), the state file
and `terraform init` are already done -- skip straight to step 4.

The identity this needs is broader here than for gcp-capabilities.md:
IAP and Cloud Run both need their own project-level roles
(`roles/iap.admin` or equivalent, `roles/run.admin`,
`roles/vpcaccess.admin`) alongside the IAM-admin role the accounts
playbook already needed. If reusing the same master credential from an
earlier step in this conversation, confirm with the human that it
actually holds these too before spending a `terraform apply` finding
out it doesn't.

## 4. Plan and apply, targeted at IAP/Cloud Run resources only

Fill in `iap_members`, `ui_domain` (if `expose_ui_publicly`), and
`use_cloudrun_iap_proxy` in `terraform.tfvars` (reuse the same file
gcp-capabilities.md created, or start one the same way). Then:

```
terraform plan -backend-config=backend-local.hcl \
  -target=google_project_service.run \
  -target=google_project_service.vpcaccess \
  -target=google_compute_backend_service.ui \
  -target=google_iap_web_backend_service_iam_member.members \
  -target=google_cloud_run_v2_service.proxy \
  -target=google_compute_global_address.lb \
  -target=google_compute_managed_ssl_certificate.lb
```

(Check `iap.tf`, `cloudrun-proxy.tf`, and `dns.tf` for the exact
resource addresses this version of the module declares, and drop any
`-target` for a resource `count`/`for_each`s down to zero given this
deployment's own variables -- e.g. `google_cloud_run_v2_service.proxy`
only exists when `use_cloudrun_iap_proxy` is on.) As in
gcp-capabilities.md: **stop and ask the human** before applying if the
plan proposes touching `google_compute_instance.host`, its disk, or
`network.tf`'s resources -- this playbook should never need to.

```
terraform apply -backend-config=backend-local.hcl <same -target flags>
```

## 5. Verify and hand back the access paths

```
terraform output url
terraform output tunnel_command
terraform output cloudrun_proxy_service
```

Report these back to the human. `url` is null unless
`expose_ui_publicly` is on; `tunnel_command` always works once the IAP
IAM member grant applied, regardless. Tell them the DNS record and
managed certificate (if `expose_ui_publicly`) can take several minutes
to become reachable even after `apply` returns clean -- a `curl` that
fails right after apply isn't necessarily a problem.

## 6. Drop the master credential

Same as gcp-capabilities.md step 7: revoke the service account and
delete the key file from the host, and confirm with the human that it's
gone before considering this playbook finished.
