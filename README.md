# Grain

**A single-node agent cluster.** One machine runs a controller VM and a
small pool of sandbox VMs; label a GitHub issue or pull request, and an
agent picks it up, does the work in a sandbox, and pushes a branch that the
controller turns into a PR.

The point of the design is the credential boundary: **the untrusted
execution environment holds nothing worth taking.** The agent *process* —
`claude -p` — runs on the **controller**, with its entire native tool
roster disabled and replaced by five narrow MCP tools, four of which reach
the assigned sandbox over SSH; the fifth (`ask_question`) reaches a human
instead, by posting to the GitHub issue/PR thread. The sandbox is where the
work actually happens
— the checkout, the builds, the `kind` clusters — and it holds no GitHub
token and no Claude credential. Its only route out to GitHub is a git
proxy on the controller, allowlist-checked and audit-logged. It *can* hold
a GCP credential, if the deployment mints one: a real, short-lived
service-account key, minted fresh per dispatch and revoked once the task
ends (or after 24 hours regardless) — never anything capable of minting
another.

The host holds no *system* credential — no GitHub token, no GCP key, no
Claude login; every one of those lives on the controller's `/data`. It
holds one thing: an admin SSH key, for direct setup/repair/debugging access
to the controller and every sandbox.

```
host (Debian, KVM — admin SSH key only, no system credentials)
├── controller VM   automation loop · claude -p (as grain-agent) · git proxy
│                   · mints/revokes a GCP key per dispatch · /data (every credential)
│                       │
│                       │  SSH, five MCP tools, nothing else
│                       ▼
├── sandbox-0       docker · kind · the workspace checkout — no GitHub/Claude
│                   credential; a short-lived GCP key only if one was minted
└── sandbox-1       (same)
```

## Where the agent runs, and why it moved

`claude -p` used to run *in* the sandbox, with a real Claude Code login
sitting there, and Claude Code's own sandbox/permission settings tried to
contain it. A full live-debugging session found that fundamentally broken:
the credential leaks into any unsandboxed Bash subprocess's environment
trivially (confirmed live with a plain `env`), the agent readily discovers
`dangerouslyDisableSandbox: true` on its own to get there, and Landlock —
kernel-level, immune to that flag — can protect a *file* but has no concept
of environment variables at all. No amount of tuning closes that gap.

So the credential left the untrusted environment entirely. Today
`grain/automation/dispatch.py` starts `claude -p` on the controller, as a
dedicated unprivileged `grain-agent` account, with:

- `--tools ""` — the entire native tool roster emptied. Confirmed live:
  `--allowedTools` alone does **not** do this (it is a permission hint, not
  a roster filter); both flags together do, and the advertised tool list in
  the `system/init` event shrinks to exactly what is named.
- `--mcp-config <per-dispatch file> --strict-mcp-config` — pointing at
  `grain/automation/mcp_server.py`, which exposes exactly six tools:
  `run_command`, `read_file`, `edit_file`, `write_file` all resolve against
  the *assigned* sandbox's workspace, over SSH — the sandbox's address,
  user, and key are baked into the MCP server's argv at dispatch time,
  never into a tool call's own arguments. `ask_question` and
  `comment_on_issue` are different: neither ever touches the sandbox at
  all, and both only ever write to a local file on the controller for the
  orchestrator to relay as a GitHub comment (see "Asking the human a
  question" and "Analysis-only tasks" below) — the agent still gets no
  GitHub API access of its own.
- `TodoWrite` and `Task` also allowed — a `Task`-spawned subagent inherits
  the same empty roster (confirmed live by an explicit system denial, not
  self-report), so delegation is safe to leave on.

A sandbox therefore holds exactly one secret: its own git-proxy bearer
token, which buys nothing but proxied access to allow-listed repos and is
revocable per sandbox.

## Asking the human a question

An agent that's genuinely blocked — ambiguous requirements, a decision only
a human can make — can call the `ask_question` MCP tool instead of guessing
or grinding to a timeout. That ends its turn: `dispatch.py` resets a fixed
per-unit file before every dispatch, the tool call writes the question
there, and once the unit finishes, `core.py`'s sweep reads it back, posts
it as a `🤖`-signed comment on the task issue, and swaps the in-progress label
for `grain-agent-awaiting-reply` — **without** re-adding the trigger label,
so the task doesn't immediately redispatch and re-ask the same question in
a loop.

The same machinery covers a task whose `/repo` directive is missing,
malformed, or names a repo that isn't allow-listed: the comment says which
of those it is, and the task waits in exactly the same state.

The issue then sits idle until someone with write access to the task repo
(GitHub's own `author_association`: owner, member, or collaborator) replies
in the thread — every `run_once` checks each open question's comments for
exactly that, and re-applies the trigger label on its own the moment one
shows up, so the very next dispatch picks it back up with the reply already
in its prompt (`_dispatch` always fetches the current comment thread). A
trusted reply can also carry a `/repo`, `/pr` or `/base` directive, which
is how a parked task gets repaired without editing the original body. A
reply from anyone *without* write access is ignored: treating any comment
as a redispatch trigger would let a random public commenter drive the agent
with content of their choosing, on a public repo, which is exactly the
prompt-injection gate the trigger label exists to close. Re-applying the
label by hand still works too, as a fallback.

The agent still never gets GitHub API access of its own — `core.py` is the
only thing that posts the comment, and only from this one path
(docs/roadmap.md items 12–13).

## Analysis-only tasks

Not every task is a code change. One filed only as a question, an
investigation, or a request for a recommendation can end with a call to the
`comment_on_issue` MCP tool instead of a `git push` (bwsalmon/agents#50,
reworked by bwsalmon/agents#89). That works like `ask_question`'s file
handoff — `dispatch.py` resets a fixed per-unit file before every dispatch,
the tool call writes the comment there, and once the unit finishes,
`core.py`'s sweep reads it back — but what happens with it depends on the
branch, which is always checked first: if the agent never pushed anything,
the comment is posted as a `🤖`-signed comment on the task issue instead of
a pull request, and the task is tagged `grain-agent-completed` without the
issue itself being closed. If the agent *did* push commits, a pull request
opens for them exactly as it would without any comment at all — calling
`comment_on_issue` never suppresses a pull request the branch actually
earned, which is what `complete_analysis` (its predecessor) used to get
wrong: it skipped the branch check outright whenever the agent called it,
so an agent confused about which tool to call at the end of a task could
push real commits and still lose the PR. Nothing is left pending
afterwards, unlike a question — there is no reply to wait for.

## Documentation

- **[`docs/system-diagram.md`](docs/system-diagram.md)** — the picture:
  every component, VM, port, and secret, and which trust boundary each
  arrow crosses.
- **[`docs/design.md`](docs/design.md)** — the reasoning: why a VM per
  agent, the credential ladder, the threat model, what earlier revisions
  traded away.
- **[`docs/data-model.md`](docs/data-model.md)** — the task model: what a
  task, a repo role, a folder, a capability, a pull request, a sub-task
  and a run are, which of them GitHub owns and which grain does, how a
  task reads more repos than it writes, and how review threads become
  tasks. A design; no code implements it yet.
- **[`docs/bootstrap.md`](docs/bootstrap.md)** — the design behind
  `grain host bootstrap`, which collapses the old fourteen-step setup into
  one command.
- **[`docs/runbook.md`](docs/runbook.md)** — the operator procedure, in
  more detail than this file, including rotation, stranded sandboxes, and
  the known gaps.
- **[`docs/host-adapter.md`](docs/host-adapter.md)** — the one
  platform-specific module, and what a macOS port would have to replace.
- **[`docs/beads-incus-feasibility.md`](docs/beads-incus-feasibility.md)** —
  whether to rebuild on incus (a third host-adapter driver) and beads (a
  graph issue tracker): what each buys, what incus costs the macOS plan,
  and why beads' source of truth decides it.
- **[`docs/roadmap.md`](docs/roadmap.md)** — item-by-item status.
- **[`docs/next-session.md`](docs/next-session.md)** — what is left before
  a first real run, in the order worth doing it. Start here.

> **`design.md`, `system-diagram.md`, `roadmap.md`, and `runbook.md`
> predate the move described above** — they still describe `claude -p`
> running on the sandbox with a login credential there. They are accurate
> on everything else. The code, `provision/`, `next-session.md`, and this
> file are current; for the live findings behind the change, read
> `grain/automation/dispatch.py`'s and `mcp_server.py`'s module
> docstrings.

Read the ["Status and limits"](#status-and-limits) section before pointing
this at a repo you care about.

---

## Requirements

**The host** is a Debian machine with nested virtualization — the design
targets a GCP `n2-highmem-4` (4 vCPU, 32 GB), and that shape has been
load-tested with two sandboxes plus the controller running real `kind`
clusters and real from-source builds concurrently. CPU binds before
memory; see [`docs/design.md`](docs/design.md)'s resource budget.

```sh
ls /dev/kvm                       # must exist — nested virt is off by default on GCP
sudo apt-get install -y \
  qemu-system-x86 qemu-utils libvirt-daemon-system cloud-image-utils \
  nftables python3 git openssh-client curl
```

`grain` itself is **stdlib-only Python 3.11+** — see `pyproject.toml`,
`dependencies = []`. There is nothing to `pip install`, on the host or on
the controller. The tools above are what the libvirt driver shells out to:
`virsh` (pinned to `qemu:///system`), `qemu-img`, `cloud-localds`, `nft`,
`ssh`.

**The guests** are provisioned from a stock Debian cloud image by the two
scripts in `provision/`. Fetch the image once:

```sh
sudo mkdir -p /var/lib/grain/images
curl -fsSL -o /var/lib/grain/images/debian-12.qcow2 \
  https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
```

`Cluster.image` (`grain/inventory.py`) still defaults to the bare string
`"debian-12"`, so name the real path — either per-invocation with the
global `--image` flag, or once in `/var/lib/grain/cluster.toml`:

```toml
sandbox_count = 2
image = "/var/lib/grain/images/debian-12.qcow2"
# subnet, bridge, and the per-role sizes keep their defaults unless set
```

**Off-host** you need a GitHub credential for the target repo (details
under [Configure](#configure)) and a Claude Code login for the pool — one,
not one per sandbox. A GCP service account is optional, for agents that
need cloud access.

## Install

There is no package and no entry point — `grain` is invoked as
`python3 -m grain.cli`, wrapped by `bin/grain` (`exec python3 -m grain.cli
"$@"`, resolved against its own real path so it works from anywhere it's
symlinked onto `PATH`, not just from inside the checkout). Clone it
wherever you run it, then put that wrapper on `PATH`:

```sh
git clone https://github.com/bwsalmon/grain
cd grain
sudo ln -sf "$(pwd)/bin/grain" /usr/local/bin/grain   # the rest of this file assumes it
```

The same tree is deployed twice: **on the host**, where `grain host …`
drives the hypervisor, and **on the controller** at `/opt/grain`, where
everything else runs. Which machine a command belongs on is not
cosmetic — the controller is the only one with `/data` and the
credentials. `bin/grain` is symlinked onto the controller's `PATH`
automatically (`provision/controller.sh`, and `terraform/gcp/files/deploy.sh`
for the GCP host that plays the same role there) — this manual step is only
needed for a host you set up yourself.

| Command group | Runs on |
|---|---|
| `grain host up/create/start/stop/destroy/recreate/status/rules/egress` | host |
| `grain host bootstrap/wait/deploy`, `grain controller configure`, `grain sandbox login` | host (they use the admin SSH key) |
| `grain host cleanup/health` | controller (they reach sandboxes over the controller's own key) |
| `grain automation …`, `grain sessions …`, `grain metadata …`, `grain github audit` | controller |
| `python3 -m grain.proxy.server` | controller |

Global flags go **before** the subcommand group: `--data-dir` (default
`/data`), `--sandboxes`, `--image`, `--cluster-file` (default
`/var/lib/grain/cluster.toml`), `--config-dir`, `--admin-ssh-public-key`,
`--controller-ssh-public-key`, `--dry-run`. So
`grain --data-dir /data automation run-once`, never
`grain automation run-once --data-dir /data`.

### Read before you apply

This program rewrites the firewall of a machine you may only be able to
reach through that firewall. Both escape hatches are first-class:

```sh
grain host rules                  # print the ruleset, apply nothing
grain --dry-run host up           # print every command, run none
```

## Bring it up

### The one-command path

```sh
sudo python3 -m grain.cli \
  --image /var/lib/grain/images/debian-12.qcow2 \
  host bootstrap \
    --task-repo your-org/agent-tasks \
    --target-repo your-org/your-repo \
    --github-token-file /path/to/token \
    --claude-credentials-file ~/.claude/.credentials.json
```

`--github-token-file -` reads the token from stdin instead. Both
credential flags are optional on a re-run: a bare re-run does not clobber
what is already in place.

`grain/bootstrap.py` sequences eleven stages: preflight, an admin SSH
keypair generated if none exists yet (`/var/lib/grain/admin-ssh{,.pub}` by
default — trusted by the controller *and* every sandbox), network up,
controller created and booted, wait for SSH and cloud-init, the
controller's own key read back automatically, this tree deployed to
`/opt/grain`, `/data` configured (repo config, GitHub token, Claude
credential), every sandbox created and given a git-proxy token, the git
proxy and automation timer enabled, and a verify pass.

**No state file** — every stage converges from observed reality, so a
re-run after a failure resumes rather than redoing completed work.
`--dry-run` previews every command with nothing touched. Stage order is not
reorderable: the controller's key has to be read back *before* any sandbox
is created, since sandbox creation is what embeds it as an authorized key.

The sections below walk through what each stage does, for debugging it or
doing a step by hand.

### On GCP, from a config repo

[`templates/gcp/`](templates/gcp/) is a repository template
that does everything above on GCP with nobody SSHing anywhere. Fork it and
it holds only the deployment's configuration and its two workflows — the
Terraform module and the scripts it ships into instance metadata live in
this repo's own [`terraform/gcp/`](terraform/gcp/), and the deploy steps
themselves in [`ci/`](ci/), so the workflow a fork owns is wiring rather
than logic. All of it is pulled fresh by both workflows, pinned to the
same `grain_ref` the deployment already uses to fetch grain onto the host,
so a fork never carries a copy of any of it that could drift. Terraform creates the host — nested
virtualization on, a persistent disk for `/var/lib/grain`, and a service
account whose roles are one committed list — GitHub Actions applies it on
every push to `main`, and a small service on the host watches instance
metadata and re-runs `host bootstrap` whenever the config changes.

That repo is also the task repo: an issue filed there and labelled
`grain-agent` is what the agents pick up, so the queue and the deployment
that serves it are one thing to set up, not two.

The GitHub token and the Claude Code token live in its Actions secrets,
which the deploy workflow pushes straight into the host's own instance
metadata; the host reads them back locally, with no GCP credential of its
own, so no separate GCP service ever holds them and no runner or SSH
session ever touches the host — the arrangement
[`docs/design.md`](docs/design.md#where-credentials-should-live) prefers.
Everything else — machine type, sandbox count, target repos, IAM roles —
is a diff you review before it ships.

### 1. Network

```sh
sudo python3 -m grain.cli host rules      # read it first
sudo python3 -m grain.cli host up
```

Creates the `br-grain` bridge and applies the nftables policy: sandboxes
reach the git proxy and nothing else, sandbox↔sandbox is dropped,
anti-spoofing rules pin each tap to its assigned address. Idempotent —
re-run it after any inventory change.

Egress from sandboxes is **open by default**, because agents need the
internet for dependencies. `grain host egress allowlist` is the opt-in
tightening; be honest with yourself that open egress means a compromised
sandbox can exfiltrate whatever it can read.

The host's own INPUT chain is deliberately *not* managed —
`grain host rules --input-chain` renders one for hosts that need it, and
nothing applies it automatically.

### 2. Controller VM

```sh
sudo python3 -m grain.cli host create controller --provision provision/controller.sh
sudo python3 -m grain.cli host wait controller
sudo python3 -m grain.cli host status
```

Use the `controller` target specifically — `create`/`recreate` refuse
`--provision` with `all`, since the controller and sandboxes take
different scripts.

`provision/controller.sh` installs Python, `gcloud` (for
`gemini_keys.py`/`gcp_keys.py`'s own gcloud calls), the Claude Code CLI, a
system user (`grain-agent`, for `claude -p` and the MCP server it spawns),
the `/data/{secrets,config,state}` layout, and the `grain-automation.timer`
/ `grain-git-proxy.service` units (**installed but left disabled**). It also
generates the controller's own SSH keypair at
`/data/secrets/controller-ssh{,.pub}`, idempotently — group-readable by
`grain-agent`, which needs it to reach the sandbox it was dispatched
against.

It deliberately does **not** deploy this repo's code and does **not**
enable any service. No secret is ever baked into a provisioning script,
and both of those steps need real data.

**Carry the controller's public key to the host.** The key is generated
*on* the controller, and `LibvirtAdapter` — which runs *on the host* —
needs it to inject as each sandbox's authorized key. Do this before
creating sandboxes; a sandbox created first gets no controller-role
authorized key at all and is unreachable from the automation dispatch path
(though still reachable by the admin key below).

```sh
ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  cat /data/secrets/controller-ssh.pub \
  | sudo tee /var/lib/grain/controller-ssh.pub > /dev/null
```

`/var/lib/grain/controller-ssh.pub` is the default
`--controller-ssh-public-key` path; override the flag to keep it elsewhere.
(`host bootstrap` does all of this automatically, including detecting a
*changed* controller key and repairing existing sandboxes.)

### Admin access

Two keys, two purposes (`grain/adapter/libvirt.py`, `LibvirtAdapter.create`):
an **admin** key, trusted by the controller *and* every sandbox, for setup,
repair, and debugging; the **controller**'s own key, trusted by sandboxes
only, for the automation dispatch path and the MCP server's tool calls.
`host bootstrap` generates the admin key itself if
`--admin-ssh-public-key` doesn't exist yet.

```sh
sudo python3 -m grain.cli sandbox login sandbox-0     # or 'controller'
```

Direct, interactive SSH using the admin key — no hop through the
controller first. For a stuck `kind` cluster, a wedged docker daemon,
anything `grain host health`/`cleanup`/`sessions browse` doesn't give
enough visibility into.

### 3. Sandbox VMs

```sh
sudo python3 -m grain.cli host create sandboxes --provision provision/sandbox.sh
sudo python3 -m grain.cli host status
```

`provision/sandbox.sh` installs Docker from the official repo, `kind`, and
the usual agent toolchain, raises the inotify limits `kind` needs (their
absence fails as opaque `too many open files` errors), and pre-pulls the
kind node image. It installs **no** Claude Code and places **no**
credential — there is nothing on a sandbox to harden anymore.

### 4. Deploy the code to the controller

```sh
sudo python3 -m grain.cli host deploy
```

No credential needed — `grain/adapter/deploy.py` pipes a `tar` of this
working tree over the admin SSH path and extracts it as root; `/opt/grain`
is created empty by the provisioning script. Since `grain` has no
third-party dependencies, the source tree is the whole deployment.

## Configure

Everything below lives on the controller, under `/data`. This is the
per-deployment data that a provisioning script has no business holding.

```sh
grain controller configure --task-repo owner/agent-tasks \
  --target-repo owner/service-a --target-repo owner/service-b \
  --github-token-file PATH \
  --claude-credentials-file PATH
```

writes `automation.json` (the task repo), `repo-allowlist.json` (the
target repos), the token file, the `credentials.json` entries pointing
every one of those repos at it, and both copies of the Claude
credential — over the admin SSH path, stdin, never argv. `host bootstrap`
calls this for you; running it on its own is for adding a repo, rotating a
token, or placing a Claude credential later without a full bootstrap
re-run.

```
/data/
  secrets/
    controller-ssh, controller-ssh.pub   # generated by provision/controller.sh
    claude-credentials.json              # the pool's one Claude Code login
    sandbox-tokens.json                  # sandbox name -> git-proxy bearer token
    gcp-service-account.json             # optional; 0600 -- gemini_keys.py's own primary key
    github/
      credentials.json                   # repo pattern -> credential name
      <name>.token                       # one 0600 file per name above
  config/
    repo-allowlist.json                  # ["owner/repo", ...], default-deny
    automation.json                      # AutomationConfig
    gcp-key.json                         # optional; agent SA email + project id (bwsalmon/agents#126)
    sandbox-github-key.json              # sandbox name -> named credential override, if any
  state/
    automation/state.json, audit.log, sessions/
    automation/units/grain-task-<sandbox>/
      prompt.md, mcp-config.json, transcript.jsonl   # one dir per dispatch
    git-proxy/audit.log
```

**The Claude credential.** One login for the whole pool, on the controller
only. `configure_claude_credentials` writes two copies: a root-owned
reference copy at `/data/secrets/claude-credentials.json`, and the live
copy `claude -p` actually reads at
`/home/grain-agent/.claude/.credentials.json`, owned by `grain-agent`.
Nothing places a Claude credential on a sandbox, and nothing should.

**GitHub credentials.** `credentials.json` maps a repo pattern to a
credential name, narrowest match wins, and the proxy records which one
served each request:

```json
{"your-org/your-repo": "bot", "your-org/*": "bot", "*": "personal"}
```

Every value other than the literal `"anonymous"` needs a matching
`<name>.token` file beside it — a `0600` file holding the raw token.
Prefer a dedicated machine account invited as a collaborator (that needs
repo admin, not org admin) over a personal token.

**Withhold `workflow`, `delete_repo`, `write:org`, and every `admin:*`
scope.** The `workflow` one is the non-obvious privilege escalation: an
agent that can edit `.github/workflows/**` can make CI run code of its
choosing with whatever secrets that workflow holds. Withholding the scope
makes *GitHub* reject the push, which is a control your bugs cannot
bypass. `grain github audit` checks this — see [Operate](#operate). A task
that genuinely needs such a scope names a separate, narrowly-provisioned
credential instead of widening the default one — see "Label an issue
`grain-github-<name>`" below; `grain github audit` will (correctly) flag
that credential too, since the scope really is there, deliberately.

**Repo allowlist**, `/data/config/repo-allowlist.json` — the *target*
repos this deployment may work in. Enforced twice against one file: by the
git proxy on every fetch and push, and by the orchestrator when it resolves
a task's `/repo` directive, so a task naming an off-list repo is parked
with an explanation instead of failing later as an opaque clone error. The
task repo does not belong here — no sandbox ever clones it. A plain JSON
array, default-deny, hot-reloaded on every request with no restart:

```json
["your-org/your-repo"]
```

A repo must be on this list *and* covered by a `credentials.json` pattern
before the proxy forwards anything for it.

**Sandbox tokens**, `/data/secrets/sandbox-tokens.json` — what a
sandbox's git credential helper presents to the proxy as its HTTP Basic
password:

```json
{"sandbox-0": "…", "sandbox-1": "…"}
```

`host bootstrap` mints one per sandbox before the proxy first starts, which
matters: the proxy loads this file once at startup, so a token minted only
lazily on first dispatch would make that very first dispatch fail
authentication (a live-found bug, fixed by `ensure_sandbox_tokens`). To add
one by hand, `python3 -c 'import secrets; print(secrets.token_hex(32))'`
and restart `grain-git-proxy.service`.

**Automation**, `/data/config/automation.json` — `task_owner` and
`task_repo` name the *task* repo (the polled queue) and are the only fields
with no default. Which repos tasks may dispatch *into* is not configured
here: that is `repo-allowlist.json`, the same list the git proxy enforces.

```json
{"task_owner": "your-org", "task_repo": "agent-tasks",
 "default_target_repo": null}
```

`default_target_repo` is the target for a task carrying no `/repo`
directive. `null` (the default) makes the directive mandatory: a task
without one is parked with a comment rather than dispatched at a guess.
A single-repo deployment sets it to its own repo and writes no directives
at all — which is what `grain controller configure --task-repo X` with no
`--target-repo` produces.

The defaults worth knowing: `trigger_label: "grain-agent"`,
`in_progress_label: "grain-agent-in-progress"`,
`awaiting_reply_label: "grain-agent-awaiting-reply"`,
`gemini_key_label: "grain-gemini-key"`,
`ssh_user: "debian"`, `ssh_key_path: "/data/secrets/controller-ssh"`,
`runs_per_hour: 60`, `max_runtime_minutes: 120`. Also
`github_host: "api.github.com"`, `git_forward_host: "github.com"`, and
`github_use_tls: true` — right for every real deployment, and set
otherwise only to point a live test at a mock GitHub
(`--github-host`/`--git-forward-host`/`--github-insecure-http`).

**Branch protection on the target repo.** Not scriptable from here — it
needs admin on that repo — and it is load-bearing rather than optional:
no direct pushes to the default branch from the agent credential, no
force-push, no deletion, PRs required. The design deliberately enforces
write safety at GitHub instead of by parsing pack files, because getting
that wrong fails open.

**Start the services**, now that `/opt/grain` holds code and `/data`
holds credentials:

```sh
sudo systemctl enable --now grain-git-proxy.service
sudo systemctl enable --now grain-automation.timer     # two-minute cadence
```

`systemctl cat grain-automation.timer` shows the exact unit; edit
`/etc/systemd/system/*` and `daemon-reload` for a different cadence.

**GCP (optional, bwsalmon/agents#126).** No key file to place by hand for
this one — the controller mints its own, on demand, as the host's own
attached identity:

```sh
grain controller configure \
  --gcp-agent-service-account-email grain-agent@my-project.iam.gserviceaccount.com \
  --gcp-project-id my-project
```

This writes `/data/config/gcp-key.json`. From the next dispatch on, every
sandbox gets a freshly minted, short-lived key for that account pushed
into it at `~/.gcp-service-account.json`, revoked once the task's slot
frees (or after 24 hours regardless — see `grain/automation/gcp_keys.py`).
The agent's own prompt tells it the path and how to use it; nothing is
baked into the sandbox image.

**Verify before trusting it:**

```sh
grain --data-dir /data automation status     # every sandbox should read `free`
grain --data-dir /data github audit          # no `flagged` verdicts
grain host health                            # every sandbox healthy
```

## Use it

**File the task in the task repo, and label it `grain-agent`.** One repo
is the agent set's queue: it is the only repo polled, labelled, or
commented on. The code being changed is a *target* repo, named by the task
itself:

```
Something is broken in the widget service.

/repo acme/widget-service
/pr 42            (optional: continue that PR instead of a fresh branch)
/base develop     (optional: PR base; default is the target repo's own)
```

A directive can sit anywhere in the body, and a maintainer can add or
correct one by replying to the issue — replies count as directives, from
the same people who could have applied the label. `default_target_repo` in
`automation.json` covers a deployment whose task repo *is* its code: set
it and no task needs a `/repo` line at all.

A target repo has to be on `/data/config/repo-allowlist.json`, the same
list the git proxy enforces. A task naming anything else — or naming
nothing, with no default configured — is **parked**: the orchestrator
comments saying exactly what is wrong, swaps the trigger label for
`grain-agent-awaiting-reply`, and picks the task back up once a maintainer
replies. Nothing dispatches on a guess about which repo was meant.

The next `run-once` pass picks a labelled task up, moves the label to
`grain-agent-in-progress`, and claims a free sandbox. It also applies a
second label naming *which* sandbox took it — `grain-agent-working-0`,
`grain-agent-working-1`, and so on — re-applied every `run-once` cycle for
as long as the task stays in progress and removed the moment it stops,
however it stops (bwsalmon/agents#95, bwsalmon/agents#101), so it never
sits stale once the work has moved on or a human has knocked it off by
hand. Dispatch is two-sided:

- On the **sandbox**: the workspace at `/home/debian/workspace` is cloned
  (first task) or fetched-and-reset (every task after) through the git
  proxy, and a git credential helper is pointed at that sandbox's proxy
  token — delivered over stdin, never argv, so the token never lands in a
  clone URL, in `ps`, or in command logs.
- On the **controller**: `claude -p` starts as the transient unit
  `grain-task-<sandbox>`, running as `grain-agent`, with the prompt on
  stdin and the four MCP tools pointed at that sandbox. Untrusted issue
  content never becomes a shell-interpolated argument anywhere in this
  path.

The agent works in the sandbox through those tools and pushes to
`grain/issue-<N>` — `<N>` being the *task* issue's number. When the unit
finishes, the sweeper verifies that branch exists in the target repo and
opens the PR there, closing the task issue by a fully qualified
`Closes owner/tasks#N` reference.

The branch name is computed by the controller, never taken from the
agent's own report — the prompt it received came from untrusted issue
content, so nothing the agent says about what it pushed is trusted as an
input to a GitHub write.

**Add `/pr 42` to a task** to have an agent address review feedback, fix
CI, or continue work in flight on an existing pull request in the target
repo. Same pool, same rate limit; the workspace lands on the PR's own
branch with its existing history, the prompt carries the PR's review
comments, and it pushes more commits to that branch rather than opening a
new one. The labels and the conversation still live on the task issue — no
label of ours is ever applied in a target repo.

**Add `/review true` alongside `/pr 42`** to have an agent *read* that
pull request instead of continuing the work on it. The workspace lands on
the same branch `/pr` alone would use, but the agent is told not to push
anything — it leaves feedback with a dedicated tool instead, optionally
attached to a specific file and line. Once the run finishes, everything it
left is posted as a single **draft** review on the pull request: GitHub
leaves a review with no `event` in the request `PENDING`, visible only to
the credential that created it, until a human opens it on github.com and
submits it themselves — an agent never approves, requests changes on, or
even plain-comments its own (or anyone else's) code. `/review` with no
`/pr` alongside it is refused and the task is parked, the same way an
unlisted `/repo` is: a review needs a pull request to know which branch to
read and which PR to post its draft against.

**A completed task's PR is watched, too** (bwsalmon/agents#83): each
`run-once` pass checks every still-open PR against a definite conflict
(GitHub's own `mergeable` field reading `false`) or a definite failing
check, and the first time either shows up for a given PR, grain files a
*new* task suggesting the fix — never a second one for the same PR. That
task is filed with `grain-agent-needs-approval`, not the trigger label, so
it sits visibly in the queue without being picked up on its own; a comment
on the original task links to it. Apply the trigger label to it, the same
action that starts every other task, or comment `/lgtm` on it
(bwsalmon/agents#136) — either one lets the agent set attempt the fix on
a fresh branch built on top of the original PR's own branch — a PR stacked
on that PR, not a second one against the target repo's default branch.
That new task also carries `/auto-merge`, so once its own PR reads clean
(no conflict, no pending or failing check) grain merges it straight into
the original PR's branch itself — no second round of human review for the
fix. A stuck fix (one that itself ends up with a conflict or a failing
check) is left open rather than chained into another suggestion.

**Label a task `grain-gemini-key`** to have a short-lived Gemini API key
minted for that task, placed in its sandbox (the prompt tells the agent
exactly where), and revoked automatically once the task's slot frees —
success, failure, or stranded, whichever comes first. A label, not a body
directive (bwsalmon/agents#49): the same "a human decided this" trust
tier the trigger label itself carries, applied at any point before or
during the run rather than parsed out of the issue's own untrusted text.
Off by default: a deployment enables it once with `grain controller
configure --gemini-project-id <project>` (see `docs/runbook.md`, "Enabling
`grain-gemini-key`"); a task carrying the label before that's done is
parked with a comment, the same as an unlisted `/repo`. The raw key never
rides in the prompt file, only its path in the sandbox — see
`grain/automation/gemini_keys.py` for why this is minted on the
controller's own account rather than the sandbox-facing metadata broker.

**Label a task `grain-scratch-repo`** (bwsalmon/agents#159) to dispatch it
into a repo dedicated to testing grain itself, one per sandbox slot
(`owner/grain-scratch-<sandbox>`), instead of anywhere named by `/repo` —
the label overrides any `/repo` line entirely, since which of the scratch
repos applies isn't known until a sandbox is actually picked. Off by
default: a deployment enables it once with `grain controller configure
--scratch-repo-owner <owner>`; a task carrying the label before that's
done is parked with a comment, the same as an unlisted `/repo`.
Authentication is nothing custom (bwsalmon/agents#186): a personal access
token with elevated permissions (Contents, Issues, Pull requests, and
Checks — nothing else grain's own GitHub client ever exercises), placed
like any other named credential (`grain controller configure --github-key
scratch=PATH`), reaches every scratch repo through an `owner/*`
`credentials.json` pattern an operator adds by hand — see
`grain/automation/scratch_repo.py` and docs/runbook.md, "Enabling
`grain-scratch-repo`", for the full setup. `--scratch-repo-owner` itself
is plain, non-secret config: it only names which repo a given sandbox's
scratch task should land in, and carries no credential of its own.

**Label a task `grain-self-debug`** (bwsalmon/agents#62, #86) to give the
agent four extra tools, all strictly read-only, for triaging a bug in
grain itself rather than the target repo's own code:

- `read_grain_logs`: recent `journalctl` entries for grain's own
  controller services, `grain-automation.service` and
  `grain-git-proxy.service` — or, via a third `unit` value `grain-task`,
  this dispatch's own controller-side `claude -p` unit. `claude -p`'s
  stdout is redirected to the transcript file `capture.py` reads, but its
  stderr never is, so `grain-task` is the only way to see why a run
  crashed before writing anything (bwsalmon/agents#97).
- `check_grain_health`: the same ssh/systemd/docker/disk checks `grain
  host health` reports to an operator, against either the task's assigned
  sandbox or the controller itself.
- `read_grain_config`: one of grain's own non-secret config files under
  `/data/config` on the controller (`automation.json`,
  `repo-allowlist.json`, `gemini-key.json`, `gcp-key.json`,
  `scratch-repo.json`, `sandbox-github-key.json`) — every credential and
  token this deployment holds lives under `/data/secrets` instead, which
  none of these tools can reach.
- `read_automation_audit_log`: recent entries from the dispatch/sweep
  audit log — one line per state-machine decision the orchestrator made
  (a task dispatched, skipped, succeeded, failed, or stranded, and why).

Same trust tier as the other two labels above, and on unconditionally at
the account level (`provision/controller.sh` grants `grain-agent`
read-only `systemd-journal` group membership regardless of whether any
task ever uses it), so unlike `grain-gemini-key` there is no separate
`controller configure` step — the label alone turns these tools on for
that task. Strictly read-only throughout: `read_grain_logs` can only read
the journal for one of those three fixed units, `check_grain_health` never
mutates anything it checks, `read_grain_config` is checked against a
fixed allowlist of non-secret file names rather than a raw path, and
`read_automation_audit_log` only ever reads the one audit log file — none
of the four can write to anything or reach any other file, unit, or
credential on the controller.

**Label a task `grain-self-repair`** (bwsalmon/agents#99) to give the
agent four more tools — the mutating counterpart to `grain-self-debug`
above, kept behind its own label since none of these are read-only:

- `restart_grain_service`: `systemctl restart` on
  `grain-automation.service` or `grain-git-proxy.service`, for a wedged
  service short of rebooting the whole controller.
- `reboot_sandbox`: reboot the task's own assigned sandbox VM.
- `reformat_sandbox`: run the same between-task hygiene (`kind delete
  clusters --all`, `docker system prune -af --volumes`) grain already
  runs automatically once a task finishes, callable mid-task instead of
  only between tasks.
- `reboot_controller`: reboot the controller VM this task's own session is
  running on — a last resort that ends the task's turn immediately and
  interrupts any other task running concurrently on the same controller,
  recovered automatically by grain's own stranded-work sweep once the
  controller is back.

Same "on unconditionally, no separate `controller configure` step" shape
as `grain-self-debug` — `provision/controller.sh` grants `grain-agent` a
narrow, unconditional sudo rule covering exactly the command lines
`restart_grain_service`/`reboot_controller` need, nothing else. **What it
deliberately doesn't cover**: rebuilding a sandbox from scratch or
re-running `grain host bootstrap`, both `HostAdapter` operations against
the *host* machine's hypervisor that this deployment's controller has no
credential or network path to reach at all (see `docs/runbook.md`,
"Enabling `grain-self-repair`," for the gap spelled out in full) — a
genuinely bad VM still needs an operator's `grain host recreate`.

**Label an issue `grain-github-<name>`** to have that task's git pushes use
a named credential instead of the deployment's default one — for a task
that genuinely needs a scope the default deliberately withholds (the
`workflow` scope, most notably: see "Withhold `workflow`..." above). An
operator provisions the credential first with `grain controller configure
--github-key <name>=PATH` (or `grain host bootstrap --github-key
<name>=PATH` on a first-time deploy — see `docs/runbook.md`, "Adding a
named GitHub key," which also covers threading one through a Terraform/GCP
deployment's own config repo via `GRAIN_GITHUB_KEYS`). Either way, it
writes only `/data/secrets/github/<name>.token` — deliberately not a
`credentials.json` entry, so it never becomes any repo's *default*
credential, only a task-selected override. A label naming a credential
that was never provisioned parks the task with a comment, same as an
unlisted `/repo`; more than one `grain-github-*` label on the same issue
does too, since which one applies would otherwise be a guess. The override
applies for exactly that task's lifetime — set right before dispatch,
cleared the moment its sandbox's slot frees — and, like the trigger label
itself, only someone who can apply a label can ask for it.

**Requiring a human to apply the label is the prompt-injection gate.**
Anyone who can file an issue can put text in front of the agent; the
label is what makes a person decide it runs.

Run a pass by hand, or let the timer do it:

```sh
grain automation run-once        # sweep stranded work, then poll and dispatch
grain automation status          # current sandbox -> issue/PR assignments
```

Agents get git transport only. There is no GitHub API from a sandbox and
no `gh pr create` — all API work happens on the controller, which is the
machine that already holds the credential.

### Reading what an agent did

```sh
grain sessions list                          # trigger, sandbox, outcome, transcript?
grain sessions list --kind pr --outcome failed
grain sessions browse                        # curses UI; needs a real terminal
```

`claude -p` is run with `--output-format stream-json --verbose`, redirected
to that dispatch's `transcript.jsonl`, and `--no-session-persistence` so
Claude Code's own session store doesn't accumulate forever under the shared
`grain-agent` account. Trajectories are captured **on completion**, before
a sandbox's slot is freed — the unit name (and therefore the transcript
path) is fixed per sandbox, so the next task overwrites it and
fetch-on-demand would find the wrong task's content or none at all.

Every dispatch and sweep decision is one JSON object per line in
`/data/state/automation/audit.log`, with the outcome (`dispatched`,
`succeeded`, `failed`, `stranded`, or a `skipped: …` reason). It is the
first place to look when a run surprises you.

## Operate

```sh
grain host status                    # VM states and addresses          (host)
grain host health [name]             # SSH/docker/systemd/disk          (controller)
grain host cleanup [name]            # kind delete + docker prune       (controller)
grain host recreate <name> --provision provision/sandbox.sh            # (host)
grain github audit                   # withheld-scope check             (controller)
```

`health` and `audit` both exit nonzero on a problem, so they drop
straight into a cron job.

**The sweeper handles most of it already.** Each `run-once` pass, before
dispatching: it reads each tracked unit's state on the controller, a
finished unit gets its label moved and its PR opened, a failed or stranded
one gets the issue re-labelled and requeued, and either way the session's
trajectory is captured, between-task cleanup runs on the sandbox (`kind
delete clusters --all`, `docker system prune -af --volumes`), and a health
check follows. A sandbox is clean the moment its slot frees.

What is **not** automatic:

- **A wedged-but-`ACTIVE` unit.** The sweeper can't tell "slow" from
  "stuck", so it waits for `max_runtime_minutes`. Stop the unit by hand
  (`sudo systemctl stop grain-task-<sandbox>` — on the **controller** now,
  not the sandbox) and re-run `automation run-once` to let the sweep
  collect it.
- **Acting on a health warning.** Both `grain host health` and the
  sweeper's own check *report*; neither quarantines a degraded sandbox or
  stops dispatching to it. `grain host recreate <name>` is the usual fix.
- **Recreating on a cadence.** Recreate is the deploy path for image
  updates and the fix for a filling disk, and weekly is reasonable —
  nothing schedules it. It is the routine operation most likely to be
  forgotten until something breaks.
- **Token rotation on recreate.** Despite what the design describes,
  `recreate()` does not touch `sandbox-tokens.json` today. Rotate as a
  separate step.

**Rotation is uniformly "replace the file, restart the one service that
reads it."** `config/` is watched; `secrets/` is not. The git proxy and
the automation process both load credentials once at construction, so a
running process keeps using the old token until restarted.

**Backup is `/data`** — a provider snapshot is enough, nothing else in
the system is stateful. And since the design has no inbound dependency
(cron polling, no webhooks), **stopping the instance when idle is
supported and cuts the bill roughly threefold.**

### Adding a target repo

Adding a repo tasks may dispatch into — the task repo itself is configured
once, in `automation.json`, and adding another one means a second
deployment.

1. Add `"owner/repo"` to `repo-allowlist.json` (hot-reloaded). Tasks can
   then name it with `/repo owner/repo`; until it is on the list, one that
   does is parked with a comment saying so.
2. Add a `credentials.json` entry pointing at a credential with a
   matching token file.
3. Apply branch protection on that repo — GitHub-side, manual, needed.
4. `grain github audit`, and confirm the covering credential isn't
   `flagged`.
5. If using a machine account, invite it as a collaborator.

## Development

```sh
python3 -m pytest              # unit tests: no hypervisor, no network, no root
```

705 unit tests pass on a bare machine; the live suites skip themselves
cleanly there, so the command above is safe anywhere. They come in when the
machine can run them:

| Suite | Needs |
|---|---|
| `test_net_integration.py` | root and a reachable netfilter |
| `test_vm_integration.py` | `/dev/kvm`, `qemu:///system`, `br-grain` up (it fetches the base image itself if missing) — includes an MCP-server-over-real-SSH round trip |
| `test_controller_integration.py` | the same, but the base image must already be cached |
| `test_bootstrap_integration.py` | the same — the two-key sandbox and the deploy/configure verbs |
| `test_live_issue_to_pr.py` | the same — a full issue→PR run against a mocked GitHub |

`.github/workflows/tests.yml` runs that same unscoped `python3 -m pytest`
on every pull request and every push to `main`, against Python 3.11 and
3.13 — the floor `pyproject.toml` declares and the stock interpreter on
the Debian the host runs. A hosted runner has no `/dev/kvm` and no
netfilter, so the live suites skip there exactly as they do on a bare
machine, and the job holds no credential of any kind: it runs PR-branch
code before a human has read it.

```sh
python3 -m tests.loadtest      # boot the real pool and measure it under kind + build load
```

> The live suites default to the same VM names a real deployment uses
> (`controller`, `sandbox-0`), so an unscoped `pytest` on a host with a
> live cluster up will destroy it. Scope it to specific files, or stop the
> cluster first.

This project holds itself to **verify live, not just unit tests**, and the
record is worth the cost: the credential leak that moved `claude -p` to the
controller, `--allowedTools` not actually emptying the tool roster, a
root-owned unit directory blocking the agent's own transcript redirect,
`claude` missing from a non-login shell's `PATH`, `git http-backend`
denying push even with `GIT_HTTP_EXPORT_ALL=1`, a sandbox's first dispatch
always failing proxy auth, transient units self-unloading on success — none
of these were visible on paper. All surfaced by booting a guest and running
the real thing on it.

Layout:

```
grain/
  inventory.py        names, addresses, ports, specs — one source of truth
  run.py              command execution behind an interface (Real/DryRun/Fake)
  cli.py              the whole operator surface
  bootstrap.py        the eleven-stage `host bootstrap` sequencer
  adapter/            the only platform-specific code: libvirt, lima, nftables
  automation/         poll, dispatch, sweep, capture, session history
    dispatch.py       starts claude -p on the controller, per sandbox
    mcp_server.py     the four tools that are the agent's only reach
    gcp_keys.py       mints/revokes a GCP service-account key per dispatch
  proxy/              the git proxy: allowlist, tokens, credentials, audit
provision/            controller.sh, sandbox.sh — cloud-init user-data
ci/                   the deploy steps a config repo's own CI runs
terraform/gcp/        the GCP module, and the scripts it ships to the host
templates/gcp/        the config-repo template: configuration and workflows
tests/
  loadtest.py         the harness behind the resource-budget numbers
  test_*.py           unit suites, plus the live suites in the table above
```

Addresses are **assigned, never discovered**: the inventory decides them
and the adapter tells the VM. Asking the hypervisor afterwards would let
the firewall rules and the VMs disagree about who is who, which is
exactly what the anti-spoofing rules exist to prevent.

## Status and limits

The whole mechanical pipeline works, and most of it has been verified
against real VMs — including a real `grain host bootstrap` run driving a
real deployed `grain-automation.timer`, and a real `claude -p` dispatch,
both against a mock GitHub server. What is genuinely not finished:

- **Nothing here has run against a real GitHub repo or a real
  credential.** Issue→PR is verified end to end against a mocked GitHub;
  `grain github audit` is verified against scripted response shapes, not a
  live token. Both need a real target repo with admin access.
- **The controller-side agent has not completed a real issue→PR run.** The
  mechanism is live-verified — the MCP tools over real SSH against a real
  sandbox (including timeout enforcement), and all three sweep scenarios
  (happy path, nonzero exit, exit-zero-no-push) end to end against real
  infrastructure with a scripted stand-in for `claude`. What hasn't
  happened since the move is a real logged-in agent working a real issue
  through those four tools and pushing.
- **No token mint has been verified** against a real GCP project.
- **`git push` through the proxy is exercised live**, by the fake agent in
  `test_live_issue_to_pr.py` against a real `git http-backend` behind a
  real `GitProxy` — but never against real GitHub.
- **Hardening (`docs/roadmap.md` item 7) is half done**: the tooling is
  built and unit-tested; moving a real repo down the credential ladder and
  applying branch protection need a real repo.

And what the threat model does **not** defend, stated plainly: sequential
tasks on one long-lived sandbox (task B inherits task A's filesystem);
abuse of legitimate access while a sandbox is compromised — including the
sandbox's own git-proxy token, which is scoped to allow-listed repos but is
real; exfiltration under the default open-egress policy; malicious code in
agent output — human PR review is the control, which is why the
no-push-to-`main` rules are load-bearing; a compromised controller, which
now runs the agent process *and* holds every credential; a compromised
host, which owns the hypervisor and therefore every VM; and prompt
injection via issue content, where requiring a human label is a mitigation
rather than a guarantee.
