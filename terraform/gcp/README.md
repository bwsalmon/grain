# terraform/gcp -- a grain deployment on GCP

Deploys one small VM running `grain daemon` (`README.md`,
"Deploying it"), exposed at a fixed DNS name through an IAP-protected
HTTPS load balancer, with a separate persistent disk for its state so the
VM itself is disposable (bwsalmon/agents#394) and another for everything
its sandboxes write, so a task that fills a disk does not take the host
down with it ("Sandboxes get a disk of their own").

```
  terraform apply ──▶ the VM, its data and sandbox disks, its network,
        │              its load balancer, its service accounts and IAM
        │              grants
        │
        ▼
  push-secrets.sh ──▶ instance metadata (a GitHub PAT, a Gemini API key,
        │              and -- if configured -- a minted GCP minter key)
        │
        ▼
  grain-config-sync.service on the host notices grain-deploy-generation
  changed
        │
        ▼
  deploy.sh: read the non-secret config and the three secrets back out of
  this same instance's metadata, run scripts/setup.sh
```

Unlike v1's module (a controller VM running a pool of nested libvirt
sandbox guests, meant to be forked by many different organizations from
a template), this module is narrower on purpose:
grain has no host adapter yet and its daemon already dispatches onto plain
host directories by default (`README.md`, "What this does not have
yet"), so there is no fleet to generalize over here -- just the one VM
`scripts/setup.sh` already knows how to install and update, with the
staging-specific pieces (the disk split, the load balancer, IAP, the
IAM grants) wrapped around it.

## Use it

**1. Bootstrap the GCP side**, once, from a project-owner account:

```sh
./terraform/gcp/bootstrap-gcp.sh --project my-staging-project
```

This creates the Terraform state bucket and a deployer service account
with exactly the roles this module needs (see that script's own
comments). Pass `--repo owner/name` too if CI, not a human, will run
Terraform -- it also wires up workload identity federation, the same
mechanism v1's own bootstrap script used, so CI needs no long-lived
key.

The state bucket is the one piece `deploy/terraform-apply.sh` will also
create for itself, when a deploy finds it missing -- with the same
uniform bucket-level access, versioning and public-access prevention
this script applies. It only ever creates the name this script would
have chosen, `<project_id>-<name_prefix>-tfstate`; a `backend.hcl`
naming anything else is treated as a deliberate choice (or a typo) and
fails rather than being created, since a mistyped bucket would otherwise
give a deployment an empty state and have it build a second copy of
itself. Everything else here still has to be run once by hand: the
deployer account and the workload identity pool cannot bootstrap
themselves from a job that authenticates as them.

**2. Fill in the config.** Copy `example.tfvars` and `backend.hcl.example`
and fill in every `CHANGE-ME`. At minimum: `project_id`,
`deployer_member`, and `iap_members` -- an empty `iap_members` is a valid
plan, but nobody can reach the UI until it names someone.

**3. No OAuth client needed.** IAP uses a Google-managed OAuth client
when none is configured, which is why the IAP OAuth Admin API was
deprecated in January 2025 -- there is normally no client to create any
more. Leave `create_iap_brand`, `iap_client_id` and `iap_client_secret`
alone and go straight to the apply.

Set `iap_client_id`/`iap_client_secret` only for a client of your own,
created by hand once (the GCP Console's "Google Auth Platform" page --
check current GCP guidance, this area of the product has moved). Do not
set `create_iap_brand` even then: a project may have at most one brand,
ever, and the provider warns `google_iap_brand` no longer functions as
intended for a new one.

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
export GRAIN_GITHUB_TOKEN="github_pat_..."   # a fine-grained PAT scoped to test_repos
./deploy/push-secrets.sh
```

`GRAIN_GEMINI_API_KEY` is optional: with `enable_gemini_key` on (the
default) the host mints the daemon's own key for itself -- see "The
daemon's own Gemini key" below. Export one anyway to use a key of your
own instead.

`GRAIN_GITHUB_APP_ID`, `GRAIN_GITHUB_APP_INSTALLATION_ID` and
`GRAIN_GITHUB_APP_PRIVATE_KEY` are an alternative to `GRAIN_GITHUB_TOKEN`,
stored under the same `credential_name`: set all three (never a subset)
instead of the PAT to back this deployment with a GitHub App installation
token rather than a bare PAT -- see "There is no Checks permission to
grant" below for when that is needed and how to obtain them. Registering
the App itself is still a manual, browser-based step on GitHub's own
side; only pushing the resulting three values here is automated.

Watch it converge with:

```sh
gcloud compute ssh "$INSTANCE" --zone "$ZONE" --project "$PROJECT" \
  --tunnel-through-iap --command 'sudo journalctl -u grain-config-sync -f'
```

**6. Open it.** `terraform output url` -- sign in as one of
`iap_members`. With `expose_ui_publicly = false` there is no URL: run
`terraform output tunnel_command`, which prints the
`gcloud compute start-iap-tunnel` line to forward the UI to
`http://localhost:8080`.

## Bootstrapping the rest of a hand-created VM

Everything above assumes Terraform creates the VM itself, applied from
outside it, state in the GCS bucket `bootstrap-gcp.sh` creates. That's
not the only starting point: bwsalmon/agents#620 added a second one, for
a VM created by hand (the Console, a one-off `gcloud compute instances
create`, ...) that still wants this module's service accounts, IAM
grants, and IAP/Cloud Run access set up afterward -- run from the VM
itself, against `-target`ed subsets of this same module, with state kept
on the VM's own data disk (`backend-local.hcl.example`) instead of GCS.

That path is written up as playbooks grain's own configuration agent can
read and act on -- `pkg/capability/bootstrap/playbooks/
gcp-capabilities.md` and `cloudrun-iap.md` -- rather than repeated here;
open the configuration agent from the UI and ask it to bootstrap GCP
capabilities or CloudRun IAP access, or read those two files directly.
The GitHub-side setup (`github-connections.md`, `github-test-repos.md`
in the same directory) has no Terraform involved at all -- see those
playbooks for why.

## What actually runs on the host

`grain-daemon.service` runs a container image, not a binary this host
built (bwsalmon/agents#645): grain plus every binary it shells out to,
published to GHCR by `.github/workflows/build-artifacts.yml` on every
commit to grain (`Dockerfile`). Three variables decide which one:

- **`grain_ref`** names the branch. It is what the on-host checkout
  tracks -- `files/deploy.sh` clones and updates it, and that checkout
  is where this deploy finds `scripts/setup.sh` -- and, by default, also
  which image tag runs: that branch's name with `/` replaced by `-`,
  which is how CI tags it. Nothing else on the host reads that checkout:
  `setup.sh` keeps none of its own, and `scripts/kontur`'s guest build
  runs out of the source unpacked from the deployment image.
- **`grain_image_tag`** overrides that. Set it to `sha-<short sha>` --
  published for every commit -- to pin this deployment to one immutable
  build. A rollback is that variable plus a `terraform apply`: no
  rebuild anywhere, and the old image is still in the registry.
- **`grain_image`** is the repository, for a mirror or a private copy.
  A registry needing credentials takes `grain_image_pull_user` here and
  a `GRAIN_IMAGE_PULL_TOKEN` through `push-secrets.sh`;
  `ghcr.io/bwsalmon/grain`'s own package is public and pulls
  anonymously.

Two consequences worth knowing before a first deploy. A deploy no longer
compiles anything on this host, so `grain_ref` pointing at a tag or a
commit SHA -- fine for the checkout -- has no image published under that
name, and needs `grain_image_tag` set alongside it. And the host now
needs to reach the registry: an egress policy that allowed GitHub and a
Debian mirror also needs `ghcr.io`.

For the container's own shape -- what it is given, what it deliberately
is not, and the two systemd path units that let it reboot the host and
restart itself from inside a container -- see grain's own
`README.md`, "The deployment is a container".

## Deploying it from CI

Everything above is the by-hand path. To have a config repo's GitHub
Actions apply it instead, pass `--repo owner/name` to the bootstrap
script and wire a workflow to the step scripts in this module's
`deploy/`:

```
deploy/terraform-apply.sh    init, validate, apply (with stock-out retries)
deploy/read-outputs.sh       Terraform outputs -> Actions step outputs
deploy/push-secrets.sh       the same script the by-hand path runs, called with env
deploy/wait-for-host.sh      block until the host reports it converged
deploy/write-summary.sh      the job summary
```

The step bodies live in grain, and the config repo's workflow only wires
them up -- supplying which secrets, which config directory, which
generation. That is the same split v1's own config-repo template
argues for: a workflow file is forked and then owned by the config repo,
so anything written *there* is something nobody re-syncs, while a fix
here reaches every deployment on its next `grain_ref` bump. See
`bwsalmon/agents`'s `.github/workflows/deploy-v2-staging.yml` for a
worked example.

These five paths are the interface a config repo pins, and all five
moved into it: four from grain's old top-level `ci/`, and
`push-secrets.sh` from the module root beside this README, so one
deploy's steps are no longer split across two directories for no
reason. A workflow still naming the old paths breaks on the
`grain_ref` bump that first picks this layout up, so repoint it in the
same change. Two names also shortened, now that the directory says the
rest: `read-terraform-outputs.sh` is `read-outputs.sh`, and
`write-deploy-summary.sh` is `write-summary.sh`.

Two things differ from a by-hand deploy:

- **`deployer_member` must be the CI service account**, not a human --
  `push-secrets.sh` mints the minter's key, which needs
  `deployer_manages_minter_keys` (iam.tf) on whoever runs it.
- **The workflow supplies `deploy_generation`**, so a manual re-run
  redeploys rather than no-oping. `wait-for-host` then blocks on the host
  reporting *that* generation, and CI goes green only once the rollout
  actually landed.

### Sharing a project with another grain deployment

Supported -- and now the normal case, since `bwsalmon/agents` runs both a
main and a staging deployment of this module in one project. It is the
reason `bootstrap-gcp.sh` derives its workload identity pool and provider
from `name_prefix`. A pool is a project-level resource, and the script
once hardcoded "github" for it: on that name, bootstrapping a second
deployment into a project that already had one took the *update* branch
on the first's provider and rewrote its attribute condition to name
whatever `--repo` was passed the second time -- a no-op while both name
the same repo, and a deploy that can no longer authenticate at all the
moment they differ. The script prefixes now, so two deployments never
touch. `--pool`/`--provider` override it for a pool shared deliberately.

Everything else that collides is a *project-level* name. Per-host names
do not: the guest attribute namespace (`grain`), the
`grain-config-sync.service` unit, and the `grain-data`/`grain-sandbox`
disk devices all live on one instance, and `gcloud compute instances
get-guest-attributes` reads the instance you name, so two deployments
never see each other's status through them.

What has to differ, per deployment:

| Setting | Why |
|---|---|
| the state bucket/prefix (`backend.hcl`) | two deployments on one prefix each see the other's resources as drift to destroy |
| `name_prefix` | the instance, network, deployer account, and WIF pool/provider are all derived from it |
| `agent_account_id` | a project-level service account; see below |
| `minter_account_id` | likewise, when both deployments enable `enable_gemini_key` |

**`agent_account_id` has to be set on at least one of them.** It defaults
to `grain-agent`, matching `pkg/gcpsetup.DefaultAgentAccountID`, so a
second deployment left at the default fails with

```
Error 409: Service account grain-agent already exists within project ...
  with google_service_account.agent[0], on iam.tf line 69
```

Set something like `agent_account_id = "grain-staging-agent"`. Do not
point it at the other deployment's existing account to make the error go
away: that account carries the other deployment's grants, so sharing it
hands a staging agent whatever the main deployment was trusted with,
which is the whole thing sharing a project is already uncomfortably close
to.

Nothing on the host assumes the default name -- the account's email
reaches the daemon as `gcp_agent_service_account` in instance metadata,
straight from this module's own output -- so changing it costs nothing.

What is *not* separated by any of this is the agent account's reach.
`agent_can_manage_gke` grants `roles/container.admin` project-wide with
no exclusion, and `agent_can_manage_compute_instances`'s exclusion names
only this deployment's own host -- so in a shared project a staging
agent can reach the *other* deployment's host VM and any cluster in the
project. Set both to `false` while sharing, or give each deployment its
own project.

## Secrets never touch Terraform

Four kinds of value never appear in a `.tf` file, a `tfvars` file, or any
plan this module is applied from:

- **The GitHub PAT** -- a fine-grained token scoped, on GitHub's own side,
  to `test_repos`, which is also wired into the daemon's own
  `-target-repos` allow-list -- see that variable's own doc comment and
  this module's README, "Repo enforcement."
- **The Gemini API key** -- the daemon's own operating key
  (`pkg/agent/gemini`). Distinct from the gemini-key *capability*
  (`pkg/capability/geminikey`), which mints its own short-lived keys per
  task. Optional to supply: see "The daemon's own Gemini key" below.
- **The Claude Code OAuth token** and **the OpenAI API key** -- the same
  thing for the other two agent frameworks (`pkg/agent/claude`,
  `pkg/agent/codex`), pushed as `GRAIN_CLAUDE_CODE_OAUTH_TOKEN` and
  `GRAIN_OPENAI_API_KEY`. Also optional, and for a reason that now
  applies to the Gemini key too: any of them can be pasted into the UI
  instead (Settings -> Agent frameworks), which stores it in the host's
  own secrets database and takes precedence over whatever metadata
  carries. The daemon starts and serves the UI with none set; only a
  *dispatch* needs one, and a run whose framework has none fails saying
  exactly that. Push one to have a deployment come up already able to
  dispatch, without anyone opening the UI first. Which framework a run
  is driven by is a store-backed setting, overridable per task, so a
  deployment that might use more than one wants each of their
  credentials.
- **The minter's own key** -- what lets `pkg/capability/gcpkey` mint and
  revoke the agent account's per-task keys.

All of them go straight into the host instance's own metadata via
`push-secrets.sh`, which any identity with `deployer_manages_minter_keys`
(iam.tf) and edit access to the instance can run -- never through Secret
Manager, so nothing needs a project-wide secret-reading grant, and never
through Terraform, so `instance.tf`'s own `lifecycle.ignore_changes`
keeps a later `terraform apply` from treating them as drift and erasing
them. `files/deploy.sh` reads them back purely locally, over the instance
metadata server, with no GCP credential of the host's own required to do
it.

They do, however, reach the Terraform **state**, which is worth being
exact about because believing otherwise leaked one of them. Nothing here
writes them, but `terraform refresh` reads the instance's real metadata
back on every run, so the state file in the state bucket holds each of
these values. Two consequences:

- **The state bucket is as sensitive as the secrets themselves.**
  `bootstrap-gcp.sh` already gives it uniform bucket-level access and
  public access prevention; treat read access to it as read access to the
  PAT, the OAuth token and the minter's key.
- **A replacement of the host prints them.** `lifecycle.ignore_changes`
  (instance.tf) suppresses in-place diffs, not the prior state a destroy
  is rendered from -- so an apply that replaces the instance (a bigger
  `boot_disk_gb`, a new `boot_image`, a new `machine_type`) renders every
  one of these values in full. In bwsalmon/agents#653 that put the minter
  account's private key in a deploy workflow's log. `deploy/terraform-apply.sh`
  now filters the apply's output for exactly this, so the plan stays
  readable and the values do not; a key exposed before that filter existed
  needs revoking, not just redacting.

Neither of these is a reason to route the secrets through Terraform
instead. Keeping them out of configuration is what makes them absent from
diffs, pull requests and this repository; the state and the destroy diff
are the residue, and are handled where they occur.

## The daemon's own Gemini key

With `enable_gemini_key` on (the default here), the minter account
already holds `roles/serviceusage.apiKeysAdmin` project-wide -- that is
what lets the gemini-key *capability* mint a short-lived key per task.
The same grant is all it takes to mint the daemon's own long-lived
operating key, and `push-secrets.sh` has already put the minter's
credential on the host. So the host mints that key for itself:
`scripts/setup.sh`'s `mint_gemini_operating_key` runs
`grain secrets mint-gemini-key` when no key is in place yet, right after
seeding the minter credential it authenticates with.

Nothing about this widens a grant. It is the same credential, the same
API, and the same `generativelanguage.googleapis.com` restriction every
per-task key is minted under -- which also makes a minted operating key
narrower than a hand-made one, since a key created by hand in the
console is unrestricted unless someone remembers to restrict it.

What it removes is a first-deploy footgun: without it, applying and
pushing secrets leaves you with a deployment whose every dispatched run
fails at setup, because the credential its agent framework runs as is
the one nobody has pasted in yet. (The daemon itself comes up either
way now, and its Settings pane is the other way to fix that -- see
README.md, "Two agent frameworks, either per task".)

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

## Kontur sandboxing

`enable_kontur_sandboxes` (on by default, `variables.tf`) dispatches onto
real `bwsalmon/kontur`-managed VMs, one per slot, over SSH
(`orchestrator.KonturSandboxes`) instead of plain host directories
(`orchestrator.HostSandboxes`) -- the same nested cloud-hypervisor guest
`scripts/kontur/README.md` documents building, now actually wired through
this deployment shape (bwsalmon/agents#504) rather than only configurable
by hand-editing the systemd unit afterward.

Nothing has to be built or published by hand first, and nothing is built
on the host. `scripts/setup.sh`'s own `ensure_kontur_images`/
`ensure_kontur_kvm_access` pull the sandbox image and grant `$GRAIN_USER`
`/dev/kvm` and `docker` group access, before `write_systemd_units` wires
up `grain daemon`'s own `-kontur-*` flags.

Which image is not a decision this deployment makes: CI publishes one per
commit, and the grain image a host runs carries the reference of the one
built from its own commit (`grain sandbox-image`), so the two are always
one commit's worth of each other -- including after a rollback, which asks
for its own older sandbox rather than whatever is newest
(bwsalmon/agents#645). The guest a task actually runs in travels inside
that same image, which is why there is one artifact here and not two.

That is a change worth knowing if you deployed an earlier version. A host
used to *build* its guest disk on first use -- debootstrap plus the whole
package set `guest-setup.sh` installs, several minutes against a real
Debian mirror, on every host and every deploy generation, with a
content-hash cache to avoid repeating it. It did that because
`guest-setup.sh` baked that deployment's own SSH public key into the
image, so no generic published disk could exist. kontur generates that
keypair per VM boot now, so the guest is derived once in CI
(`scripts/kontur/build-guest.sh`) and pulled here like anything else.
`kontur_image_bucket`, which existed for an operator who wanted to build
it once centrally and share it across a fleet, is gone with the build it
was an alternative to.

`kontur_oci_image` overrides the pulled reference with one of your own --
a mirror, a private copy, or a sandbox pinned apart from grain's. It is
pulled either way, and built on the host in no case. (Pointing it at an
Artifact Registry repository in this project works; create the repository
yourself first.)

A pull failing leaves `enable_kontur_sandboxes`'s intent unmet *for that
run only*: the host still comes up dispatching into host directories,
with a line in `journalctl -u grain-daemon -f`'s own deploy log and
`setup.sh`'s own readiness report naming which prerequisite was not
ready, rather than failing the whole install. Re-running `setup.sh` (or
waiting for the next `config-sync` pass) picks it back up once it is.

Set `enable_kontur_sandboxes = false` to keep a deployment on
host-directory sandboxing indefinitely -- nothing above is required then,
and `enable_nested_virtualization` can come off with it if nothing else
on this host needs `/dev/kvm`.

Each VM's root filesystem is writable, not read-only:
`write_systemd_units` passes `-disk-mode=overlay`, so the guest writes
into a thin qcow2 created inside its own container and backed by the
disk the sandbox image carries, which is only ever read
(bwsalmon/kontur#37). Creating it costs a fixed few hundred KiB whatever
the disk's size, so booting a VM never copies a multi-gigabyte image and
several VMs on a host share one copy of it. `konturctl`'s own default is
read-only, which a dispatched task cannot use. The host directories this
used to need -- one for the shared image, one for the overlays -- are
gone with it.

Which is also why the sandbox volume is docker's data root rather than a
directory of its own: with the overlay inside the VM's container, "how
much disk can a task use" is a question about docker's storage, and
`sandbox_disk_gb` is the answer to it (see "Sandboxes get a disk of their
own").

A freshly created VM's container/pod getting an IP is not the same moment
as its nested guest actually accepting SSH connections -- confirmed by
hand, a docker-backed VM's container is reachable well before the guest
has finished booting to sshd. `orchestrator.KonturSandboxes.resolveEndpoint`
now waits out that gap too (bwsalmon/agents#504: a plain TCP dial against
the resolved host:port, polled the same way it already polled for the
container IP itself), bounded by the same `ReadyTimeout` (2 minutes by
default) as the IP wait -- so the first dispatched tool call against a
brand new slot no longer races a guest that is still booting. A guest
that genuinely takes longer than that to boot (a cold local disk, a busy
host) still fails clearly, naming the address it could not reach, rather
than the ambiguous "connection refused" a bare SSH client would have
given no hint about.

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

## Creating a VM as the agent

`agent_can_manage_compute_instances` grants the roles, but a bare
`gcloud compute instances create my-vm --zone=...` run with a
`gcp-key` credential still fails, twice over, for reasons that name no
role this module sets:

- **The default service account.** `create` with no
  `--service-account` attaches the project's default Compute Engine
  account, and GCP refuses unless the caller holds
  `iam.serviceAccounts.actAs` on *that* account. Nothing here grants
  that, deliberately -- the default account usually carries broad legacy
  roles, so an instance running as it is a wider identity than the agent
  itself. `iam.tf`'s `agent_acts_as_self` grants `actAs` on the agent
  account only, so pass `--service-account=<agent email>` (or
  `--no-service-account --no-scopes` for an instance that needs no GCP
  identity).
- **Port 22.** This VPC opens SSH to Google's IAP range only, and only
  for tagged instances. `network.tf`'s `agent_iap_ssh` covers the
  `<name_prefix>-agent-vm` tag, so an instance created without
  `--tags=<name_prefix>-agent-vm` cannot be reached even though the agent
  holds `roles/iap.tunnelResourceAccessor` -- and it cannot fix that
  itself, since `compute.instanceAdmin.v1` can read firewall rules but
  not create them.

The agent's SSH grant is `roles/compute.osLogin`, so create the instance
with `--metadata=enable-oslogin=TRUE` (or set that project-wide) to keep
OS Login the path in. Left off, `gcloud compute ssh` falls back to
pushing a key into project-wide metadata instead, which is both a wider
write than this needs and a different permission from the one granted
here.

The `agent_vm_create_flags` output prints the whole set for this
deployment. The full lifecycle is then:

```
gcloud auth activate-service-account --key-file=$GOOGLE_APPLICATION_CREDENTIALS
gcloud config set project <project_id>

gcloud compute instances create my-vm $(terraform output -raw agent_vm_create_flags) \
  --machine-type=e2-medium --image-family=debian-12 --image-project=debian-cloud \
  --metadata=enable-oslogin=TRUE
gcloud compute ssh my-vm --zone <zone> --tunnel-through-iap --command 'hostname'
gcloud compute instances delete my-vm --zone <zone> --quiet
```

`--tunnel-through-iap` is not optional: `--no-address` above leaves the
instance with no external IP, which is what lets `enable_cloud_nat` give
it egress without giving the internet a path in.

Two things this deployment does *not* let a task do to itself:
`agent_compute`'s IAM condition excludes this deployment's own host VM
from both instance management and SSH, and `agent_iap_ssh` is scoped to
its own tag rather than the network, so a task cannot tag its way onto
the host's rules either.

## Repo enforcement

`test_repos` is now wired into the daemon's own `-target-repos`
allow-list (`pkg/model.Config.TargetRepos`, bwsalmon/agents#399): a
task naming a repo outside it is filed as asked but parked awaiting
reply rather than dispatched, the same "the allowlist the git proxy
enforces, so a task naming anything else is parked with a comment rather
than dispatched" v1's own `target_repos` documented. `default_target_repo`'s own validation, above, already
requires it be a member of `test_repos` (or empty), so the two variables
stay consistent with each other by construction.

### What the PAT needs

A **fine-grained** token, with repository access limited to exactly
`test_repos`, and four repository permissions:

| Permission | Level | What needs it |
|---|---|---|
| Metadata | Read | mandatory for every fine-grained token; also `GET /repos/{owner}/{repo}`, behind `DefaultBranch` |
| Contents | Read and write | the git proxy's fetch and push, plus creating and updating refs (`CreateBranch`, `UpdateBranch`, `BranchExists`, `GetBranchHead`) |
| Pull requests | Read and write | `CreatePullRequest`, `FindOpenPullRequestForBranch`, `GetPullRequest`, `MergePullRequest` |
| Actions | Read | `ListWorkflowRuns` -- the CI read auto-merge gates on, since a fine-grained token cannot reach the Checks API. Omit it and auto-merge never fires; see below |

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

**There is no `Checks` permission to grant, which is why `Actions` is on
the table above.** GitHub offered a `Checks` permission for fine-grained
tokens initially and withdrew it "due to some edge cases", and has said
only GitHub Apps may use that API in the meantime -- it is still listed
in GitHub's own permissions reference, so it looks grantable when it is
not. `ListCheckRuns` therefore returns 403 to this deployment
permanently.

`pkg/orchestrator`'s `checkRunsFor` answers that by falling back to
`ListWorkflowRuns`, which reads GitHub Actions workflow runs from the
Actions API and sits behind the `Actions` read permission a fine-grained
token *can* hold. Auto-merge works on this PAT with that permission
granted. What the fallback cannot see is CI reported through the Checks
API by anything other than Actions -- a third-party provider like
Buildkite or CircleCI, or a review bot -- so a deployment whose
`test_repos` use one of those needs a credential that can read checks
properly, below.

Grant neither and there is no CI signal at all: a task with `AutoMerge`
never merges, because a PR whose checks cannot be read is never
`PrClean` (reading it as clean is how you merge a PR with CI red).
`checkRunsFor` treats that as "health unknown" rather than an error, and
`/api/config` reports `autoMergeDegraded` so the UI can say so. Dispatch,
the push, and opening the pull request are a separate reconciler and are
unaffected either way.

Note that a **classic** PAT reads the Checks API fine with the `repo`
scope -- the limitation is specific to fine-grained tokens. `repo` is far
coarser than the four permissions above, though, so prefer the
fine-grained token plus `Actions`, or the App below.

**A deployment needing checks from a non-Actions provider needs a GitHub
App installation token instead of this PAT** (bwsalmon/agents#491) --
`pkg/gitproxy` now has that credential kind, so this is configuration
after all rather than the code change it used to be. Create the App on
`test_repos`' own org with Contents, Pull requests and Checks read
(-and-write for the first two, the same levels the PAT table above
grants), and install it on `test_repos`. That is a manual, browser-based
step on GitHub's own side with no API this module or `push-secrets.sh`
could drive even in principle (see `docs/design.md`, "Auth model") --
there is no way around a human clicking through it once.

Getting the resulting App ID, installation ID and downloaded private key
onto the host is configuration, though, and `push-secrets.sh` now does
it the same way it already does for `GRAIN_GITHUB_TOKEN`: export
`GRAIN_GITHUB_APP_ID`, `GRAIN_GITHUB_APP_INSTALLATION_ID` and
`GRAIN_GITHUB_APP_PRIVATE_KEY` (all three, never a subset) instead of
`GRAIN_GITHUB_TOKEN` before running it -- see "Push the secrets" above.
`scripts/setup.sh` seeds them, once, as a `<credential_name>.app.json`
file next to this deployment's `*.token` files under `secrets/github/`
-- `{"app_id": "...", "installation_id": "...", "private_key":
"-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----\n"}`
-- under the same `credentials.json` entry a bare PAT would otherwise
occupy, so switching a deployment from one to the other needs no
`credentials.json` edit, just which of the two env vars this run of
`push-secrets.sh` was given. `pkg/gitproxy.CredentialSet` mints and
refreshes an installation token from that file itself (`apptoken.go`);
nothing else about the ladder or the proxy changes. Placing the file by
hand (or editing `credentials.json` to name a different credential
entirely) still works exactly as before -- `push-secrets.sh` is a
convenience over that, not a replacement for it -- and either path needs
`deploy_generation` bumped (this module runs the git proxy and the REST
client in the same `grain-daemon.service`, unlike a deployment shape with
a standalone git-proxy process), or `grain-daemon.service` restarted by
hand, the same way rotating any other credential here already requires.

"A scoped PAT to a few test repos" -- the PAT itself being a GitHub
fine-grained token limited, on GitHub's own side, to `test_repos` -- is
still a second, independent enforcement boundary underneath this one:
even a `grain settings` change widening `TargetRepos` past what the PAT
can reach would still fail at the git proxy. Nothing here keeps the two
lists in sync automatically; an operator who changes one by hand (via
`grain settings -target-repos`, `grain repo add`, or the repos page --
all three write the same field, bypassing Terraform) should update the
other too.

## Notes and limits

- **The host runs nested guests by default.**
  `enable_nested_virtualization` is on, so `machine_type` must be a
  family that supports it -- N1, N2, N2D, C2, C3 or M-series, never E2 --
  and `on_host_maintenance` is `TERMINATE` in consequence. It has to stay
  on for `enable_kontur_sandboxes` (also on by default -- "Kontur
  sandboxing" above) to work at all; a deployment dispatching only into
  host directories can turn both off together and get MIGRATE, E2, and a
  daemon that survives host maintenance.
- **The load balancer and managed SSL certificate cost more to run than
  the VM does**, and are most of what this module spends. Set
  `expose_ui_publicly = false` and none of it is created: no load
  balancer, no reserved address, no managed certificate, no DNS name.
  The UI is then reached by forwarding `ui_port` over IAP's TCP tunnel
  (`terraform output tunnel_command`), which is the same IAP, checking
  `roles/iap.tunnelResourceAccessor` instead of
  `roles/iap.httpsResourceAccessor`. It also drops the sslip.io
  dependency and the certificate-provisioning wait entirely.
- **`create_iap_brand` is not needed and may not work.** IAP falls back
  to a Google-managed OAuth client when none is configured, which is the
  normal path -- see "No OAuth client needed" above.
- **The DNS name defaults to sslip.io**, a third-party public service
  neither this module nor Google controls, resolving purely by encoding
  the reserved IP in the hostname. Set `dns_managed_zone` to use a domain
  you actually control instead.
- **Rotating the minter key needs a manual step on the host.**
  `push-secrets.sh` mints a fresh minter key and prunes old ones on every
  run, but `scripts/setup.sh`'s own `seed_gcp_minter_key` only ever
  seeds the host's local secrets database once -- it never overwrites an
  existing `gcp-key-minter` entry. Delete that entry by hand
  (`grain secrets delete gcp-key-minter key.json`, over
  `gcloud compute ssh --tunnel-through-iap`) and bump `deploy_generation`
  if a rotated key genuinely needs to take effect.
- **The host's journal reaches Cloud Logging.** `files/deploy.sh`
  installs `google-cloud-ops-agent` early, before anything that can
  fail, which is what the host account's `roles/logging.logWriter`
  grant is for. So a failed rollout is readable without a shell on the
  box -- filter to this deployment in the Logs Explorer with
  `jsonPayload._SYSTEMD_UNIT="grain-config-sync.service"`, or:

  ```sh
  gcloud logging read \
    'resource.type="gce_instance" jsonPayload._SYSTEMD_UNIT="grain-config-sync.service"' \
    --project YOUR_PROJECT --freshness=1h
  ```

  `config-sync` also publishes the tail of a failed deploy as a guest
  attribute, which is what CI prints when a rollout fails.
- **Convergence is polling, not instant**, but close to it:
  `grain-config-sync.service` hangs on the metadata server's
  `wait_for_change` endpoint (`files/config-sync.sh`, ported from v1's
  own mechanism unchanged) rather than truly polling, so a
  `deploy_generation` change or a `push-secrets.sh` run is picked up
  within moments, not minutes.
- **One deployment per state prefix.** Running more than one of these in
  the same project needs a different `name_prefix`, a different
  `backend.hcl` prefix, and (since `agent_account_id`/`minter_account_id`
  default to fixed names) distinct values for those two as well. The
  state bucket, the deployer account, and the workload identity pool and
  provider all derive from `name_prefix` already.
- **Destroying it is deliberately awkward.** The data disk carries
  `prevent_destroy` (`instance.tf`) -- it holds the SQLite store and the
  secrets database, the state a wipe or redeploy must not lose. Remove
  that block first if you really mean to lose it. The sandbox disk
  carries no such block and needs none: everything on it is a re-pullable
  image or a finished run's leftovers (see "Sandboxes get a disk of their
  own"), so it goes with the rest of the host on a wipe.
- **Sandboxes get a disk of their own.** `sandbox_disk_gb` (100 GB by
  default) is attached as `grain-sandbox` and mounted by `files/startup.sh`
  at `/mnt/grain-sandbox`, which then holds both of the things that grow
  with the work this host does:

  - **docker's data root**, moved there with an `/etc/docker/daemon.json`
    the startup script writes. That is where the sandbox image lives and,
    less obviously, where every kontur VM's writable root lives too: a VM
    gets its disk as a qcow2 overlay created *inside* its own container
    (bwsalmon/kontur#37), so a guest's writes are docker's storage, not a
    host directory anything else could have mounted.
  - **the per-run checkouts** `orchestrator.HostSandboxes` makes when
    `enable_kontur_sandboxes` is off -- `/mnt/grain-sandbox/sandboxes`,
    bind-mounted onto `scripts/setup.sh`'s own `$GRAIN_SANDBOX_DIR`
    (`/var/lib/grain-sandbox`) so the daemon needs no override to find
    them there.

  The boot disk keeps the OS, the journal and the grain checkout, and
  the data disk keeps the store and secrets. So a task that fills its
  disk -- a runaway build, a clone of something enormous -- costs the
  runs in flight and nothing else: the host stays up, `config-sync` still
  deploys, and the UI is still there to say what happened. A
  `docker.service` drop-in carries `RequiresMountsFor=/mnt/grain-sandbox`,
  so `dockerd` waits for the volume rather than racing it and quietly
  filling the boot disk under a mount point.

  The UI's own host status (Debugging → Sandbox health) reports this
  volume as well as the data disk, one row per filesystem: the daemon
  reads `-data-dir`, `-sandbox-dir` and docker's data root and folds
  together whichever turn out to be the same disk, which here means the
  sandbox row covers both of the things listed above. It showed only the
  data disk's figure until then, so the 20 GB volume read as healthy
  however full the 100 GB one beside it got. Docker's data root
  (`/mnt/grain-sandbox/docker`) is not itself mounted into the daemon's
  container, so the daemon logs one line about it at startup and reports
  the volume through `$GRAIN_SANDBOX_DIR`'s bind mount instead -- the
  same filesystem, and the same number.

  On an existing deployment the volume arrives at the host's next boot,
  since the startup script is what mounts it (or run it by hand:
  `sudo google_metadata_script_runner startup`). That boot moves docker's
  data root, so the host re-pulls its images -- the startup script clears
  `config-sync`'s deploy state to force exactly that -- and leaves the
  old `/var/lib/docker` behind, unused, for you to delete once satisfied.
  `sandbox_disk_gb = 0` puts everything back on the boot disk and undoes
  the docker configuration that went with the volume; size `boot_disk_gb`
  accordingly if you do that.
- **The load balancer can front a Cloud Run proxy instead of the VM
  directly.** Set `use_cloudrun_iap_proxy = true` (needs
  `expose_ui_publicly = true`) and `google_compute_backend_service.ui`
  (`iap.tf`) points at a small Cloud Run service (`cloudrun-proxy.tf`)
  that blind-forwards to the VM's internal IP over a Serverless VPC
  Access connector, instead of at the VM's own instance group directly.
  IAP, the DNS name, and the managed certificate are unchanged -- only
  what backs the load balancer changes. Off by default: the
  instance-group path is one fewer moving part and is what this module
  has always done.

## Layout

```
versions.tf     provider/backend requirements
variables.tf    every knob, with why it exists
network.tf      VPC, firewall rules (IAP-tunnel SSH, load-balancer/connector-to-UI), optional Cloud NAT
iam.tf          the host/agent/minter service accounts and their roles
instance.tf     the VM, the data and sandbox disks, non-secret config as instance metadata
dns.tf          the reserved static IP and the fixed DNS name
iap.tf          the load balancer chain and IAP itself
cloudrun-proxy.tf  optional Cloud Run proxy backing the load balancer instead of the VM directly (use_cloudrun_iap_proxy)
outputs.tf      instance name, zone, service accounts, URL, ssh command
files/
  startup.sh      boot: mount the data and sandbox disks, put docker's data root on the latter, start config-sync
  config-sync.sh  watch metadata, run a deploy when it changes
  deploy.sh       translate this deployment's config into a scripts/setup.sh call
bootstrap-gcp.sh   one-time: state bucket, deployer service account, optional WIF
deploy/
  push-secrets.sh     post-apply: push the GitHub PAT, the Gemini key, the kontur SSH key, and a minted minter key
  terraform-apply.sh  init, validate, apply -- creates the state bucket if it is missing
  read-outputs.sh     Terraform outputs -> Actions step outputs
  wait-for-host.sh    block until the host reports it converged on this generation
  write-summary.sh    the deploy's job summary
example.tfvars, backend.hcl.example
```
