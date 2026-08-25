# grain config repo (template)

A deployment of [grain](https://github.com/bwsalmon/grain) on GCP,
described entirely by this repository.

This repository does two jobs. It **describes the deployment** — which
machine, how many sandboxes, which repos the agents work on, and the part
worth being careful about, exactly what the VM is allowed to do in your
cloud project — as files you can read, diff, and review; push to `main` and
the running system converges on them. And it **is the task queue**: an
issue filed here and labelled `grain-agent` is what the agents pick up.

```
  pull request ──▶ CI plans, you read the diff
        │
      merge
        │
        ▼
  GitHub Actions ──▶ terraform apply       (the VM, its disks, its identity)
        │        └─▶ instance metadata     (the two runtime credentials,
        │                                   the new commit SHA)
        │                    │
        │                    ▼
        │        grain-config-sync on the host notices the SHA changed
        │                    │
        └───── waits ────────┴──▶ deploy.sh: fetch grain, read the secrets
                                  from its own metadata, run
                                  `grain host bootstrap`
```

CI never logs into the host. There is no SSH key in this repo, no runner on
the VM, and no GCP service in between — the deploy workflow attaches the two
credentials directly to the VM resource, and the host reads them back
locally, needing no credential of its own to do it.

## Use it

Fork this directory into a new repository (private is fine; the host never
reads it), then:

**1. Bootstrap the GCP side.** Once, from a laptop with project-owner
rights, from a clone of [grain](https://github.com/bwsalmon/grain) itself
— the Terraform module and this script both live there, not in your fork:

```sh
git clone https://github.com/bwsalmon/grain
./grain/terraform/gcp/bootstrap-gcp.sh --project my-project --repo my-org/my-config-repo
```

This creates the Terraform state bucket, the service account CI runs as,
and workload identity federation scoped to your repository — so CI
authenticates to GCP with no key anywhere. It prints exactly what to do
next.

**2. Fill in the config.** `config/grain.tfvars` and `config/backend.hcl`
have `CHANGE-ME` in every field that needs you. At minimum: `project_id`
and the state bucket. You do not have to name a task repo — this one is it.

**3. Set four repository secrets.**

| Secret | What it is |
|---|---|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | printed by the bootstrap script |
| `GCP_DEPLOYER_SERVICE_ACCOUNT` | printed by the bootstrap script |
| `GRAIN_GITHUB_TOKEN` | the token grain uses for **this** repo (read issues, move labels, comment) and for every repo in `target_repos` (push a branch, open a PR) |
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

No IAP/SSH access, or none of your accounts have it? The host also ships its
entire systemd journal to Cloud Logging (`google-cloud-ops-agent`, installed
by `startup.sh` -- it's what `vm_service_account_roles`'s default
`logging.logWriter` role is actually for). Filter to just this service in
the [Logs Explorer](https://console.cloud.google.com/logs), or:

```sh
gcloud logging read \
  'resource.type="gce_instance" jsonPayload._SYSTEMD_UNIT="grain-config-sync.service"' \
  --project YOUR_PROJECT --freshness=1d
```

**5. File a task.** Open an issue here, label it `grain-agent`, and name
the repo it is about. That is the whole interface.

## Issues here are the task queue

```
  issue filed here, labelled `grain-agent`
        │
        ▼
  the host polls on its own timer ──▶ claims a free sandbox
        │                                    │
        │                            an agent works in it
        │                                    │
        └──── PR opened in the target repo ◀──┘
              the issue named; this issue closed by reference
```

Once the host is up, file an issue in this repository and label it
`grain-agent`. The next polling pass claims a free sandbox, swaps the
label for `grain-agent-in-progress`, and runs an agent against it. The
deploy workflow creates all three labels the first time it runs, so they
are in the picker before you need them. Nothing about filing a task
involves a deploy: the host polls this repo on its own.

The code being changed is usually somewhere else, named by a directive in
the issue body:

```
The widget service 500s when the cache is cold.

/repo my-org/widget-service
/pr 42            optional: continue that PR instead of a fresh branch
/base develop     optional: PR base; default is the target repo's own
```

Every repo a task may name has to be listed in `target_repos` — that list
becomes the allowlist the git proxy enforces, so a task naming anything
else is parked with a comment rather than dispatched. Set
`default_target_repo` and issues need no `/repo` line at all.

A directive can sit anywhere in the body, and a maintainer can add or
correct one by replying, so a mis-filed task is repaired with a comment
rather than an edit. grain's own README has
[the rest of the workflow](https://github.com/bwsalmon/grain#use-it) —
what happens on the sandbox, how questions come back, how the PR is
opened.

> **If you leave `target_repos` empty, tasks act on this repository** —
> the deployment's own config. That is a real mode, not a mistake: agents
> that maintain their own deployment. Understand what it means before
> choosing it. An agent's PR could edit `config/grain.tfvars`, and merging
> it would widen the VM's IAM roles or point the deployment at a different
> grain ref. Two things stand between that and a bad day, and you should
> want both: grain
> [withholds the `workflow` scope](https://github.com/bwsalmon/grain/blob/main/docs/design.md)
> from its token, so an agent cannot touch `.github/workflows/**` at all;
> and nothing here applies until a human merges to `main`. Put branch
> protection on `main` and a `CODEOWNERS` entry on `config/` if agents
> work in this repo.

## Configuration here, secrets there

The split is the point of this repo, so it is worth stating plainly.

**In the repo, in the clear:** the project, region, machine type, disk
sizes, the grain ref to deploy, the sandbox count, which repos tasks come
from and go to, the network and who may SSH, and the IAM roles the VM
holds. All of it reviewable in a pull request, all of it reverted by
reverting a commit.

**In Actions secrets, never in the repo:** two values that CI attaches
directly to the VM and never uses again, plus the two identifiers that let
CI authenticate to GCP at all.

**On the host:** nothing durable that CI put there. `deploy.sh` reads the
two secrets from its own instance metadata — no GCP credential needed,
just the local metadata server every GCE instance exposes to itself —
writes them to `/run` (tmpfs, `0600`), lets `grain host bootstrap` place
them on the controller's `/data`, and deletes them. Terraform never sees a
secret value, so the state file does not contain one either.

grain's own [`docs/design.md`](https://github.com/bwsalmon/grain/blob/main/docs/design.md)
argues against keeping the *system's* credentials in Actions secrets, and
this template agrees with it: they do not live there. They pass through
once, on their way straight into the instance's own metadata, which is the
alternative that document recommends — it needs no Secret Manager grant at
all, so nothing with broader access to this GCP project (a stray IAM
binding, a misconfigured agent identity) has a role it could use to read
these two credentials back out.

## What the VM is allowed to do

The host runs as its own service account — `grain-host@…` — and holds
exactly the roles you list:

```hcl
vm_service_account_roles = [
  "roles/logging.logWriter",
  "roles/monitoring.metricWriter",
]
```

It holds no Secret Manager grant for its two credentials — they arrive as
instance metadata, which needs no IAM role to read locally — so widening
this list does not accidentally widen access to them.

There is a second, deliberately separate account for **agents**:

```hcl
agent_service_account_roles = ["roles/storage.objectViewer"]
```

Non-empty, and Terraform creates a narrow `grain-agent@…` account with
those roles and lets the host impersonate it. That is the account a
sandboxed agent's ADC is meant to resolve to, via the metadata server the
controller runs per sandbox — the host's own roles stay out of a sandbox's
reach.

Terraform creates that account and the impersonation binding; wiring the
metadata server to it needs no manual step either -- `deploy.yml` mints a
fresh key for the account on every run and pushes it straight to the
host's instance metadata (never a repo secret), and `deploy.sh` passes it
to `grain host bootstrap` automatically, alongside the account's own
email and project id. Leave `agent_service_account_roles` empty (and
`enable_gemini_key` false) and none of this applies -- no account, no key,
nothing pushed.

A short-lived Gemini API key for a task (asked for by labelling the task
issue `grain-gemini-key`, the same "a human decided this" trust tier the
`grain-agent` trigger label itself carries) reuses this same account and
key -- set `enable_gemini_key = true` to also grant it
`roles/serviceusage.apiKeysAdmin` and enable the Generative Language API,
both via Terraform. See grain's `docs/runbook.md`, "Enabling
`grain-gemini-key`".

## Day two

**Change anything** — machine type, sandbox count, a target repo, an IAM
role — by editing `config/grain.tfvars` in a pull request. CI plans it;
merging applies it and redeploys.

**Rotate a credential** by updating the Actions secret and running the
`deploy` workflow manually (`workflow_dispatch`). The workflow pushes the
new value to the host's instance metadata every run, and the manual run
bumps the deploy generation so the host picks it up.

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

This repo holds exactly two things: the deployment described as
configuration, and the workflows that apply it.

```
config/
  grain.tfvars              the deployment, as configuration
  backend.hcl                where Terraform state lives
.github/workflows/
  plan.yml                  pull request: fmt, validate (grain's Terraform, fetched fresh)
  deploy.yml                push to main: apply, push secrets, wait (same fetch)
```

Everything that *does* the applying — the Terraform module, the scripts it
ships into instance metadata (`startup.sh`, `config-sync.sh`, `deploy.sh`),
and the one-time `bootstrap-gcp.sh` — lives in
[grain](https://github.com/bwsalmon/grain)'s `terraform/gcp/`, not here.
Both workflows check that directory out fresh, at the ref
`config/grain.tfvars` names (`grain_ref` — the same ref the on-host deploy
itself fetches grain from), rather than carrying a copy of it that would
drift the moment either repo changed. See `terraform/gcp/` in a grain
checkout for the Terraform and shell source itself:

```
terraform/gcp/
  variables.tf              every knob, with why it exists
  iam.tf                    the host account, the agent account, their roles
  instance.tf               the VM, nested virtualization, the data disk
  network.tf                VPC, one firewall rule, optional Cloud NAT
  outputs.tf                instance name, zone, service accounts, ssh command
  versions.tf               provider/backend requirements
  files/
    startup.sh              boot: mount the data disk, start config-sync
    config-sync.sh          watch metadata, run a deploy when it changes
    deploy.sh               converge the host onto the config
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
