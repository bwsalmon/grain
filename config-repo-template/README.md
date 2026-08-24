# grain config repo (template)

A deployment of [grain](https://github.com/bwsalmon/grain) on GCP,
described entirely by this repository.

Everything about the deployment except two credentials is a file you can
read, diff, and review: which machine, which repos the agents work on, how
many sandboxes, and — the part worth being careful about — exactly what the
VM is allowed to do in your cloud project. Push to `main` and the running
system converges on it.

```
  pull request ──▶ CI plans, you read the diff
        │
      merge
        │
        ▼
  GitHub Actions ──▶ terraform apply       (the VM, its disks, its identity)
        │        └─▶ Secret Manager        (the two runtime credentials)
        │        └─▶ instance metadata     (the new commit SHA)
        │                    │
        │                    ▼
        │        grain-config-sync on the host notices the SHA changed
        │                    │
        └───── waits ────────┴──▶ deploy.sh: fetch grain, read the secrets
                                  with the VM's own identity, run
                                  `grain host bootstrap`
```

CI never logs into the host. There is no SSH key in this repo, no runner on
the VM, and no path from a workflow to a running credential — the host pulls
what it needs, authenticated by being itself.

## Use it

Fork this directory into a new repository (private is fine; the host never
reads it), then:

**1. Bootstrap the GCP side.** Once, from a laptop with project-owner
rights:

```sh
./scripts/bootstrap-gcp.sh --project my-project --repo my-org/my-config-repo
```

This creates the Terraform state bucket, the service account CI runs as,
and workload identity federation scoped to your repository — so CI
authenticates to GCP with no key anywhere. It prints exactly what to do
next.

**2. Fill in the config.** `config/grain.tfvars` and `config/backend.hcl`
have `CHANGE-ME` in every field that needs you. At minimum: `project_id`,
`task_repo`, and the state bucket.

**3. Set four repository secrets.**

| Secret | What it is |
|---|---|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | printed by the bootstrap script |
| `GCP_DEPLOYER_SERVICE_ACCOUNT` | printed by the bootstrap script |
| `GRAIN_GITHUB_TOKEN` | the token grain uses for the repos it works on |
| `GRAIN_CLAUDE_CODE_OAUTH_TOKEN` | output of `claude setup-token` |

(If you cannot use workload identity, leave the first two unset and put a
service account key in `GCP_CREDENTIALS_JSON` instead. It is strictly
worse; the workflows support it so that "no WIF" is not a blocker.)

**4. Push to `main`.** The `deploy` workflow runs, and its last step waits
for the host to report that it converged — so a green check means the
system is actually up, not just that Terraform returned.

A first deploy takes a while: it downloads a Debian cloud image, then boots
the controller and every sandbox. Watch it live with

```sh
gcloud compute ssh grain-host --zone us-central1-a --tunnel-through-iap \
  --command 'sudo journalctl -u grain-config-sync -f'
```

## Configuration here, secrets there

The split is the point of this repo, so it is worth stating plainly.

**In the repo, in the clear:** the project, region, machine type, disk
sizes, the grain ref to deploy, the sandbox count, which repos tasks come
from and go to, the network and who may SSH, and the IAM roles the VM
holds. All of it reviewable in a pull request, all of it reverted by
reverting a commit.

**In Actions secrets, never in the repo:** two values that CI hands to
Secret Manager and never uses again, plus the two identifiers that let CI
authenticate to GCP at all.

**On the host:** nothing durable that CI put there. `deploy.sh` reads the
two secrets from Secret Manager with the instance's own service account,
writes them to `/run` (tmpfs, `0600`), lets `grain host bootstrap` place
them on the controller's `/data`, and deletes them. Terraform never sees a
secret value, so the state file does not contain one.

grain's own [`docs/design.md`](https://github.com/bwsalmon/grain/blob/main/docs/design.md)
argues against keeping the *system's* credentials in Actions secrets, and
this template agrees with it: they do not live there. They pass through
once, on their way to Secret Manager, which is the alternative that
document recommends.

## What the VM is allowed to do

The host runs as its own service account — `grain-host@…` — and holds
exactly the roles you list:

```hcl
vm_service_account_roles = [
  "roles/logging.logWriter",
  "roles/monitoring.metricWriter",
]
```

Read access to its two secrets is granted on the secrets themselves, not
project-wide, so widening that list does not accidentally widen this.

There is a second, deliberately separate account for **agents**:

```hcl
agent_service_account_roles = ["roles/storage.objectViewer"]
```

Non-empty, and Terraform creates a narrow `grain-agent@…` account with
those roles and lets the host impersonate it. That is the account a
sandboxed agent's ADC is meant to resolve to, via the metadata server the
controller runs per sandbox — the host's own roles stay out of a sandbox's
reach.

> **Known gap.** Terraform creates that account and the impersonation
> binding, but grain's metadata server currently reads its impersonation
> source from a key file at `/data/secrets/gcp-service-account.json`
> rather than from the instance's ADC, and the controller is a nested VM
> that does not reach `169.254.169.254` by default. Until
> [roadmap item 4](https://github.com/bwsalmon/grain/blob/main/docs/roadmap.md)
> closes, wiring agents to this account is a manual step on the
> controller. Leave `agent_service_account_roles` empty and none of this
> applies.

## Day two

**Change anything** — machine type, sandbox count, a target repo, an IAM
role — by editing `config/grain.tfvars` in a pull request. CI plans it;
merging applies it and redeploys.

**Rotate a credential** by updating the Actions secret and running the
`deploy` workflow manually (`workflow_dispatch`). The workflow adds a new
Secret Manager version only when the value actually changed, and the
manual run bumps the deploy generation so the host picks it up.

**Deploy a new grain** by moving `grain_ref`. Pin it to a tag or SHA if you
want deployments to be reproducible; leave it on `main` and uncomment the
`schedule:` block in `.github/workflows/deploy.yml` to track the tip.

**When a rollout fails**, CI says so and prints the command to read the
host's log. The host keeps retrying roughly every five minutes on its own,
so a transient failure — an apt mirror, a rate limit, a secret you had not
set yet — heals without another push.

**Destroying it** is deliberately awkward: the data disk carries
`prevent_destroy`, because it holds `/data` — every credential and all
automation state. Remove that block first if you really mean it.

## Layout

```
config/
  grain.tfvars              the deployment, as configuration
  backend.hcl               where Terraform state lives
terraform/
  variables.tf              every knob, with why it exists
  iam.tf                    the host account, the agent account, their roles
  secrets.tf                secret containers; never a secret value
  instance.tf               the VM, nested virtualization, the data disk
  network.tf                VPC, one firewall rule, optional Cloud NAT
  files/
    startup.sh              boot: mount the data disk, start config-sync
    config-sync.sh          watch metadata, run a deploy when it changes
    deploy.sh               converge the host onto the config
.github/workflows/
  plan.yml                  pull request: fmt, validate, plan
  deploy.yml                push to main: apply, push secrets, wait
scripts/
  bootstrap-gcp.sh          one-time: state bucket, deployer SA, WIF
```

## Notes and limits

- **Nested virtualization is required** and is why `machine_type` cannot be
  an E2. grain runs the controller and every sandbox as libvirt guests on
  this host; without `/dev/kvm` the deploy fails immediately and says so.
- **Port 22 is the only inbound port**, open by default only to Google's
  IAP range. Widen `ssh_source_ranges` if you want direct SSH.
- **The guest subnet and the VPC subnet must not overlap.** grain uses
  `10.100.0.0/24` inside the host; `subnet_cidr` defaults to `10.20.0.0/24`
  to stay clear of it.
- **`config-sync` self-updates.** Edits to `config-sync.sh` roll out like
  anything else; the service replaces itself and systemd restarts it.
- **One deployment per state prefix.** Two deployments in one project work
  fine — give them different `name_prefix` values and different
  `backend.hcl` prefixes.
