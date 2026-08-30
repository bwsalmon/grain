# terraform/gcp-v2 -- a staging environment for v2

Deploys one small VM running v2's `grain daemon` (`v2/README.md`,
"Deploying it"), exposed at a fixed DNS name through an IAP-protected
HTTPS load balancer, with a separate persistent disk for its state so the
VM itself is disposable (bwsalmon/agents#394).

```
  terraform apply ──▶ the VM, its data disk, its network, its load
        │              balancer, its service accounts and IAM grants
        │
        ▼
  push-secrets.sh ──▶ instance metadata (a GitHub PAT, a Gemini API key,
        │              and -- if configured -- a minted GCP minter key)
        │
        ▼
  grain-v2-config-sync.service on the host notices grain-deploy-generation
  changed
        │
        ▼
  deploy.sh: read the non-secret config and the three secrets back out of
  this same instance's metadata, run v2/scripts/setup.sh
```

Unlike `terraform/gcp/` (v1: a controller VM running a pool of nested
libvirt sandbox guests, meant to be forked by many different
organizations via `templates/gcp/`), this module is narrower on purpose:
v2 has no host adapter yet and its daemon already dispatches onto plain
host directories by default (`v2/README.md`, "What this does not have
yet"), so there is no fleet to generalize over here -- just the one VM
`v2/scripts/setup.sh` already knows how to install and update, with the
staging-specific pieces (the data disk split, the load balancer, IAP, the
IAM grants) wrapped around it.

## Use it

**1. Bootstrap the GCP side**, once, from a project-owner account:

```sh
./terraform/gcp-v2/bootstrap-gcp.sh --project my-staging-project
```

This creates the Terraform state bucket and a deployer service account
with exactly the roles this module needs (see that script's own
comments). Pass `--repo owner/name` too if CI, not a human, will run
Terraform -- it also wires up workload identity federation, the same
mechanism `terraform/gcp/bootstrap-gcp.sh` uses for v1, so CI needs no
long-lived key.

**2. Fill in the config.** Copy `example.tfvars` and `backend.hcl.example`
and fill in every `CHANGE-ME`. At minimum: `project_id`,
`deployer_member`, and `iap_members` -- an empty `iap_members` is a valid
plan, but nobody can reach the UI until it names someone.

**3. Get an IAP OAuth client.** `create_iap_brand` defaults to `false`
because, as of this writing, the Terraform provider itself warns that
`google_iap_brand` "will no longer function as intended" for a genuinely
new brand, following the IAP OAuth Admin API's own deprecation --
`terraform validate` against this module surfaces that warning directly.
Create the brand and an OAuth client for it once by hand instead (the GCP
Console's OAuth consent screen / "Google Auth Platform" page -- check
current GCP documentation, since this has changed since this module was
written) and set `iap_client_id`/`iap_client_secret` in your tfvars. See
`variables.tf`'s own `create_iap_brand` if you want to try the
Terraform-managed path anyway.

**4. Apply.**

```sh
terraform init -backend-config=backend.hcl
terraform apply -var-file=staging.tfvars
```

A first apply takes a few minutes for the load balancer and managed SSL
certificate to finish provisioning (Google has to validate the
certificate against `dns_name` before it's usable, which can take up to
around an hour the very first time). The VM itself comes up much faster.

**5. Push the secrets.** Terraform never sees the GitHub PAT, the Gemini
API key, or the minter's own key -- see "Secrets never touch Terraform"
below.

```sh
export PROJECT="$(terraform output -raw project_id)"
export INSTANCE="$(terraform output -raw instance_name)"
export ZONE="$(terraform output -raw zone)"
export MINTER_SERVICE_ACCOUNT="$(terraform output -raw minter_service_account)"
export GRAIN_GITHUB_TOKEN="ghp_..."      # a fine-grained PAT scoped to test_repos
./push-secrets.sh
```

`GRAIN_GEMINI_API_KEY` is optional: with `enable_gemini_key` on (the
default) the host mints the daemon's own key for itself -- see "The
daemon's own Gemini key" below. Export one anyway to use a key of your
own instead.

Watch it converge with:

```sh
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --project "$PROJECT" \
  --tunnel-through-iap --command 'sudo journalctl -u grain-v2-config-sync -f'
```

**6. Open it.** `terraform output url` -- sign in as one of `iap_members`.

## Secrets never touch Terraform

Three values never appear in a `.tf` file, a `tfvars` file, or the
Terraform state:

- **The GitHub PAT** -- a fine-grained token scoped, on GitHub's own side,
  to `test_repos`, which is also wired into the daemon's own
  `-target-repos` allow-list -- see that variable's own doc comment and
  this module's README, "Repo enforcement."
- **The Gemini API key** -- the daemon's own operating key
  (`pkg/agent/gemini`), needed before `grain-daemon.service` will even
  start. Distinct from the gemini-key *capability*
  (`pkg/capability/geminikey`), which mints its own short-lived keys per
  task once this one has the daemon running at all. Optional to supply:
  see "The daemon's own Gemini key" below.
- **The minter's own key** -- what lets `pkg/capability/gcpkey` mint and
  revoke the agent account's per-task keys.

All three go straight into the host instance's own metadata via
`push-secrets.sh`, which any identity with `deployer_manages_minter_keys`
(iam.tf) and edit access to the instance can run -- never through Secret
Manager, so nothing needs a project-wide secret-reading grant, and never
through Terraform, so `instance.tf`'s own `lifecycle.ignore_changes`
keeps a later `terraform apply` from treating them as drift and erasing
them. `files/deploy.sh` reads them back purely locally, over the instance
metadata server, with no GCP credential of the host's own required to do
it.

## The daemon's own Gemini key

With `enable_gemini_key` on (the default here), the minter account
already holds `roles/serviceusage.apiKeysAdmin` project-wide -- that is
what lets the gemini-key *capability* mint a short-lived key per task.
The same grant is all it takes to mint the daemon's own long-lived
operating key, and `push-secrets.sh` has already put the minter's
credential on the host. So the host mints that key for itself:
`v2/scripts/setup.sh`'s `mint_gemini_operating_key` runs
`grain secrets mint-gemini-key` when no key is in place yet, right after
seeding the minter credential it authenticates with.

Nothing about this widens a grant. It is the same credential, the same
API, and the same `generativelanguage.googleapis.com` restriction every
per-task key is minted under -- which also makes a minted operating key
narrower than a hand-made one, since a key created by hand in the
console is unrestricted unless someone remembers to restrict it.

What it removes is a first-deploy footgun: without it, applying and
pushing secrets leaves you with a daemon that installs and then silently
never starts, because the one credential it cannot come up without is
the one nobody has pasted in yet.

Three things worth knowing:

- **It is seed-once.** An existing non-empty key is never overwritten,
  so `config-sync` re-running `setup.sh` on every convergence pass does
  not issue a fresh key each time, and a key you placed by hand always
  wins.
- **Rotating is manual**, the same as the minter key above: delete
  `<data-dir>/secrets/gemini-api-key` on the host, delete the old key in
  GCP, and bump `deploy_generation` so the next pass mints a new one.
- **The reaper never touches it.** `geminikey.DeleteExpired` deletes
  `grain-`prefixed keys older than 24 hours; the operating key carries
  that prefix too and is exempted by exact name
  (`geminikey.OperatingKeyDisplayName`), because it is the credential
  the daemon runs as rather than a per-task lease that leaked. That
  exemption is what keeps the reap from stopping the daemon a day after
  every deploy -- so the constant must keep its prefix, and the
  exemption must keep matching it.

Set `enable_gemini_key = false`, or supply `GRAIN_GEMINI_API_KEY`
yourself, and none of this runs.

## What the deployment is allowed to do

`iam.tf` creates three service accounts and grants each exactly what
its own capability needs -- see that file's own comments for the full
reasoning, and `variables.tf` for the on/off switches:

- **host** -- `roles/logging.logWriter`, `roles/monitoring.metricWriter`.
  No secret grant; its credentials arrive as instance metadata.
- **agent** (`grain-agent` by default, matching
  `pkg/gcpsetup.DefaultAgentAccountID`) -- what a task's short-lived GCP
  credentials belong to. `enable_gemini_key`, `agent_can_manage_compute_instances`,
  and `agent_can_manage_gke` (all on by default for this module, per
  bwsalmon/agents#394's own ask) grant it, respectively: minting a Gemini
  API key per task; creating/managing Compute Engine instances everywhere
  in the project except this deployment's own host VM; and
  creating/managing GKE clusters and Artifact Registry repositories,
  project-wide.
- **minter** (`grain-gcp-key-minter` by default) -- mints and revokes the
  agent account's keys, and (with `enable_gemini_key`) administers API
  keys project-wide. Its own key never touches Terraform; see
  "Secrets never touch Terraform" above.

## Repo enforcement

`test_repos` is now wired into the daemon's own `-target-repos`
allow-list (`v2/pkg/model.Config.TargetRepos`, bwsalmon/agents#399): a
task naming a repo outside it is filed as asked but parked awaiting
reply rather than dispatched, the same "the allowlist the git proxy
enforces, so a task naming anything else is parked with a comment rather
than dispatched" v1's `terraform/gcp/variables.tf` `target_repos`
documents. `default_target_repo`'s own validation, above, already
requires it be a member of `test_repos` (or empty), so the two variables
stay consistent with each other by construction.

### What the PAT needs

A **fine-grained** token, with repository access limited to exactly
`test_repos`, and three repository permissions:

| Permission | Level | What needs it |
|---|---|---|
| Metadata | Read | mandatory for every fine-grained token; also `GET /repos/{owner}/{repo}`, behind `DefaultBranch` |
| Contents | Read and write | the git proxy's fetch and push, plus creating and updating refs (`CreateBranch`, `UpdateBranch`, `BranchExists`, `GetBranchHead`) |
| Pull requests | Read and write | `CreatePullRequest`, `FindOpenPullRequestForBranch`, `GetPullRequest`, `MergePullRequest` |

**Do not grant `Workflows`.** `pkg/gitproxy` authorizes by repository and
by push-versus-fetch, never by what a commit touches -- nothing in v2
stops an agent editing `.github/workflows/**`, so the token not holding
the permission is the whole of the enforcement, exactly as v1 withholds
the classic `workflow` scope. GitHub rejects such a push server-side.

**`Issues` is not needed either**, unlike v1, where the GitHub issue list
*was* the task queue. v2 reads no issue, writes no comment and moves no
label: tasks arrive by being written to the store (`pkg/ui`), and the
agent's own escape-hatch tools become effects on the task, not GitHub
API calls. `pkg/github` still implements that surface -- it is built, not
wired -- so grant `Issues` only if something later routes those effects
back to GitHub.

**There is no `Checks` permission to grant.** GitHub offered one for
fine-grained tokens initially and withdrew it; the Checks API now takes
GitHub App installation tokens only. So `ListCheckRuns` returns 403 to
this deployment permanently, and `pkg/orchestrator`'s `checkRunsFor`
treats that one status as "health unknown" rather than an error -- see
its own doc comment. The cost is auto-merge: a task with `AutoMerge`
never merges, because a PR whose checks cannot be read is never `PrClean`
(reading it as clean is how you merge a PR with CI red). Dispatch, the
push, and opening the pull request are a separate reconciler and are
unaffected. A deployment that genuinely needs auto-merge needs an App
installation token for the REST client, which is a code change, not
configuration.

"A scoped PAT to a few test repos" -- the PAT itself being a GitHub
fine-grained token limited, on GitHub's own side, to `test_repos` -- is
still a second, independent enforcement boundary underneath this one:
even a `grain settings` change widening `TargetRepos` past what the PAT
can reach would still fail at the git proxy. Nothing here keeps the two
lists in sync automatically; an operator who changes one by hand (via
`grain settings`, bypassing Terraform) should update the other too.

## Notes and limits

- **The load balancer and managed SSL certificate cost more to run than
  the VM does.** This module is sized for a staging environment working
  against a handful of test repos, not for scaling traffic.
- **`create_iap_brand`'s Terraform-managed path may not work at all** --
  see "Get an IAP OAuth client" above. Check current GCP guidance before
  relying on it.
- **The DNS name defaults to sslip.io**, a third-party public service
  neither this module nor Google controls, resolving purely by encoding
  the reserved IP in the hostname. Set `dns_managed_zone` to use a domain
  you actually control instead.
- **Rotating the minter key needs a manual step on the host.**
  `push-secrets.sh` mints a fresh minter key and prunes old ones on every
  run, but `v2/scripts/setup.sh`'s own `seed_gcp_minter_key` only ever
  seeds the host's local secrets database once -- it never overwrites an
  existing `gcp-key-minter` entry. Delete that entry by hand
  (`grain secrets delete gcp-key-minter key.json`, over
  `gcloud compute ssh --tunnel-through-iap`) and bump `deploy_generation`
  if a rotated key genuinely needs to take effect.
- **Convergence is polling, not instant**, but close to it:
  `grain-v2-config-sync.service` hangs on the metadata server's
  `wait_for_change` endpoint (`files/config-sync.sh`, ported from v1's
  own mechanism unchanged) rather than truly polling, so a
  `deploy_generation` change or a `push-secrets.sh` run is picked up
  within moments, not minutes.
- **One deployment per state prefix.** Running more than one of these in
  the same project needs a different `name_prefix`, a different
  `backend.hcl` prefix, and (since `agent_account_id`/`minter_account_id`
  default to fixed names) distinct values for those two as well.
- **Destroying it is deliberately awkward.** The data disk carries
  `prevent_destroy` (`instance.tf`) -- it holds the SQLite store, the
  secrets database, and the sandbox working directories. Remove that
  block first if you really mean to lose it.

## Layout

```
versions.tf     provider/backend requirements
variables.tf    every knob, with why it exists
network.tf      VPC, two firewall rules (IAP-tunnel SSH, load-balancer-to-UI), optional Cloud NAT
iam.tf          the host/agent/minter service accounts and their roles
instance.tf     the VM, the data disk, non-secret config as instance metadata
dns.tf          the reserved static IP and the fixed DNS name
iap.tf          the load balancer chain and IAP itself
outputs.tf      instance name, zone, service accounts, URL, ssh command
files/
  startup.sh      boot: mount the data disk, start config-sync
  config-sync.sh  watch metadata, run a deploy when it changes
  deploy.sh       translate this deployment's config into a v2/scripts/setup.sh call
bootstrap-gcp.sh   one-time: state bucket, deployer service account, optional WIF
push-secrets.sh    post-apply: push the GitHub PAT, the Gemini key, and a minted minter key
example.tfvars, backend.hcl.example
```
