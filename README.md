# Grain

**A single-node agent cluster.** One machine runs a controller VM and a
small pool of sandbox VMs; label a GitHub issue or pull request, and an
agent picks it up in a sandbox, does the work, and pushes a branch that
the controller turns into a PR.

The point of the design is the credential boundary: **agents hold no
GitHub or GCP credentials.** A sandbox's only routes out are a git proxy
and a metadata server, both on the controller, both allowlist-checked and
audit-logged. The host itself holds no *system* credential — no GitHub
token, no GCP key, no sandbox token — every one of those lives on the
controller's `/data`. It does hold one thing: an admin SSH key, for direct
setup/repair/debugging access to the controller and every sandbox.

```
host (Debian, KVM, no system credentials)
├── controller VM        automation loop · git proxy · one metadata server per sandbox · /data
├── sandbox-0            claude · docker · kind
└── sandbox-1            claude · docker · kind
```

- **[`docs/system-diagram.md`](docs/system-diagram.md)** — the picture:
  every component, VM, port, and secret, and which trust boundary each
  arrow crosses.
- **[`docs/design.md`](docs/design.md)** — the reasoning: why a VM per
  agent, the credential ladder, the threat model, what earlier revisions
  traded away.
- **[`docs/bootstrap.md`](docs/bootstrap.md)** — the design behind
  `grain host bootstrap`, which collapses the old fourteen-step setup into
  one command.
- **[`docs/runbook.md`](docs/runbook.md)** — the operator procedure, in
  more detail than this file, including rotation, stranded sandboxes, and
  the known gaps.
- **[`docs/host-adapter.md`](docs/host-adapter.md)** — the one
  platform-specific module, and what a macOS port would have to replace.
- **[`docs/roadmap.md`](docs/roadmap.md)** — item-by-item status.

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

> **Known gap.** `Cluster.image` (`grain/inventory.py`) defaults to the bare
> string `"debian-12"` and there is no `--image` flag yet, so today you
> either edit that default to the path above or drive `LibvirtAdapter`
> directly from a script the way the integration tests do.

**Off-host** you need a GitHub credential for the target repo (details
under [Configure](#3-configure-the-controller)) and, optionally, a GCP
service account if agents need cloud access.

## Install

There is no package and no entry point — `grain` is invoked as
`python3 -m grain.cli`. Clone it wherever you run it:

```sh
git clone https://github.com/bwsalmon/grain
cd grain
alias grain='python3 -m grain.cli'      # the rest of this file assumes it
```

The same tree is deployed twice: **on the host**, where `grain host …`
drives the hypervisor, and **on the controller** at `/opt/grain`, where
everything else runs. Which machine a command belongs on is not
cosmetic — the controller is the only one with `/data` and the
credentials.

| Command group | Runs on |
|---|---|
| `grain host up/create/start/stop/destroy/recreate/status/rules/egress` | host |
| `grain host cleanup/health` | controller (they reach sandboxes over SSH) |
| `grain automation …`, `grain sessions …`, `grain metadata …`, `grain github audit` | controller |
| `python3 -m grain.proxy.server` | controller |

Two global flags matter and both go **before** the subcommand group:
`--data-dir` (default `/data`), `--sandboxes` (default `2`). So
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
sudo python3 -m grain.cli host bootstrap \
  --repo your-org/your-repo \
  --github-token-file /path/to/token          # or '-' to pipe it in
  # --claude-credentials-file ~/.claude/.credentials.json   # optional
```

`docs/bootstrap.md`'s sequencer (`grain/bootstrap.py`): network up,
controller created and booted, an admin SSH keypair generated if none
exists yet (`/var/lib/grain/admin-ssh{,.pub}` by default — trusted by the
controller *and* every sandbox, see "Admin access" below), the
controller's own key read back automatically, this tree deployed to
`/opt/grain`, `/data` configured, every sandbox created, the git proxy and
automation timer enabled. `--dry-run` previews every command with nothing
touched; every stage checks what's already converged, so a re-run after a
failure resumes rather than redoing completed work. The sections below walk
through what each stage does, for debugging it or doing a step by hand.

### 1. Network

```sh
sudo python3 -m grain.cli host rules      # read it first
sudo python3 -m grain.cli host up
```

Creates the `br-grain` bridge and applies the nftables policy: sandboxes
reach the git proxy and their own metadata server and nothing else,
sandbox↔sandbox is dropped, anti-spoofing rules pin each tap to its
assigned address. Idempotent — re-run it after any inventory change.

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
sudo python3 -m grain.cli host status
```

Use the `controller` target specifically — `create`/`recreate` refuse
`--provision` with `all`, since the controller and sandboxes take
different scripts.

`provision/controller.sh` installs Python, `gce_metadata_server`, the
`grain-metadata` system user, the `/data/{secrets,config,state}` layout,
and the `grain-automation.timer` / `grain-git-proxy.service` units
(**installed but left disabled**). It also generates the controller's own
SSH keypair at `/data/secrets/controller-ssh{,.pub}`, idempotently.

It deliberately does **not** deploy this repo's code and does **not**
enable any service. No secret is ever baked into a provisioning script,
and both of those steps need real data.

**Carry the controller's public key to the host.** The key is generated
*on* the controller, and `LibvirtAdapter` — which runs *on the host* —
needs it to inject as each sandbox's authorized key. Do this before
creating sandboxes; a sandbox created first gets no controller-role
authorized key at all and is unreachable from the automation dispatch path
(though still reachable by the admin key below).

This is scripted now — `grain host wait` blocks until the controller
answers SSH and finishes cloud-init, and reading the key back only needs a
non-interactive SSH hop if the host already holds an **admin** key the
controller trusts (see "Admin access" below):

```sh
sudo python3 -m grain.cli host wait controller
ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  cat /data/secrets/controller-ssh.pub \
  | sudo tee /var/lib/grain/controller-ssh.pub > /dev/null
```

`/var/lib/grain/controller-ssh.pub` is the default
`--controller-ssh-public-key` path; override the flag to keep it elsewhere.
(`host bootstrap` above does all of this automatically.)

### Admin access

Two keys, two purposes (`grain/adapter/libvirt.py`, `LibvirtAdapter.create`):
an **admin** key, trusted by the controller *and* every sandbox, for setup,
repair, and debugging; the **controller**'s own key, trusted by sandboxes
only, for the automation dispatch path. `host bootstrap` generates the
admin key itself if `--admin-ssh-public-key` doesn't exist yet.

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

`provision/sandbox.sh` installs Docker from the official repo, `kind`,
the usual agent toolchain, raises the inotify limits `kind` needs (its
absence fails as opaque `too many open files` errors), pre-pulls the kind
node image, and sets `kernel.yama.ptrace_scope = 2`.

### 4. Deploy the code to the controller

```sh
sudo python3 -m grain.cli host deploy
```

No credential needed — `grain/adapter/deploy.py` pipes a `tar` of this
working tree over the admin SSH path and extracts it as root; `/opt/grain`
is created empty by the provisioning script. Since `grain` has no
third-party dependencies, the source tree is the whole deployment. (The
manual equivalent, `ssh debian@10.100.0.2 sudo git clone <remote>
/opt/grain`, still works if you'd rather deploy from a remote directly.)

## Configure

Everything below lives on the controller, under `/data`. Nothing here is
generated for you — this is the per-deployment data that a provisioning
script has no business holding.

`grain controller configure --repo owner/name --github-token-file PATH`
writes `automation.json`, `repo-allowlist.json`, the token file, and the
`credentials.json` entry pointing at it, over the admin SSH path (stdin,
never argv). `host bootstrap` calls this for you; running it on its own is
for adding a repo, rotating a token, or placing a Claude credential later
without a full bootstrap re-run.

```
/data/
  secrets/
    controller-ssh, controller-ssh.pub   # generated by provision/controller.sh
    sandbox-tokens.json                  # sandbox name -> bearer token
    gcp-service-account.json             # optional; 0640, grain-metadata:grain-metadata
    github/
      credentials.json                   # repo pattern -> credential name
      <name>.token                       # one 0600 file per name above
  config/
    repo-allowlist.json                  # ["owner/repo", ...], default-deny
    automation.json                       # AutomationConfig
  state/
    automation/state.json, audit.log, sessions/
    git-proxy/audit.log
    metadata-server/audit.log
```

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
bypass. `grain github audit` checks this — see [Operate](#operate).

**Repo allowlist**, `/data/config/repo-allowlist.json` — a plain JSON
array, default-deny, hot-reloaded on every request with no restart:

```json
["your-org/your-repo"]
```

A repo must be on this list *and* covered by a `credentials.json` pattern
before the proxy forwards anything for it.

**Sandbox tokens**, `/data/secrets/sandbox-tokens.json` — what a
sandbox's git credential helper presents to the proxy as its HTTP Basic
password:

```sh
python3 -c 'import secrets; print(secrets.token_hex(32))'   # once per sandbox
```

```json
{"sandbox-0": "…", "sandbox-1": "…"}
```

**Automation**, `/data/config/automation.json` — `owner` and `repo` are
the only fields with no default:

```json
{"owner": "your-org", "repo": "your-repo"}
```

The defaults worth knowing: `trigger_label: "grain-agent"`,
`in_progress_label: "grain-agent-in-progress"`, `base_branch: "main"`,
`ssh_user: "debian"`, `ssh_key_path: "/data/secrets/controller-ssh"`,
`runs_per_hour: 10`, `max_runtime_minutes: 120`. Override any of them by
including the key.

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

**Log the agent in, per sandbox.** Claude Code runs *in* the sandbox with
a login credential — this is the one place the "sandboxes hold no
secrets" property is knowingly broken, hardened rather than avoided (see
`docs/design.md`, "Interim choice"). SSH in as `debian` and run the
current `claude` login flow. There is no automation for this, and none is
planned until the controller-side LLM proxy lands.

**GCP (optional).** Place the key at
`/data/secrets/gcp-service-account.json`, `chown` it to
`grain-metadata`, `chmod 0640`, and start the per-sandbox instances:

```sh
grain metadata start          # one gce_metadata_server per sandbox
grain metadata status
```

Each instance is bound to one sandbox's address and impersonates a
narrow second service account. Nothing is needed on the sandbox side:
ADC finds `169.254.169.254`, traffic is DNAT'd to that sandbox's own
instance, and every Google SDK just works.

**Verify before trusting it:**

```sh
grain --data-dir /data automation status     # every sandbox should read `free`
grain --data-dir /data github audit          # no `flagged` verdicts
```

## Use it

**Label an issue `grain-agent`.** The next `run-once` pass picks it up,
moves the label to `grain-agent-in-progress`, and dispatches to a free
sandbox as a transient systemd unit running `claude -p`. The agent works
in `/home/debian/workspace`, cloned through the git proxy, and pushes to
`grain/issue-<N>`. When the unit finishes, the sweeper verifies that
branch exists on GitHub and opens the PR.

The branch name is computed by the controller, never taken from the
agent's own report — the prompt it received came from untrusted issue
content, so nothing the agent says about what it pushed is trusted as an
input to a GitHub write.

**Label an existing PR `grain-agent`** to have an agent address review
feedback, fix CI, or continue work in flight. Same pool, same rate limit;
it pushes more commits to that PR's own branch rather than opening a new
one.

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

Trajectories are captured **on completion**, before a sandbox's slot is
freed — a sandbox is long-lived and the next task's `claude -p` overwrites
the same transcript path, so fetch-on-demand would find the wrong task's
content or none at all.

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
dispatching: a finished unit gets its label moved and its PR opened, a
failed or stranded one gets the issue re-labelled and requeued, and
either way the session's trajectory is captured, between-task cleanup
runs (`kind delete clusters --all`, `docker system prune -af --volumes`),
and a health check follows. A sandbox is clean the moment its slot frees.

What is **not** automatic:

- **A wedged-but-`ACTIVE` sandbox.** The sweeper can't tell "slow" from
  "stuck", so it waits for `max_runtime_minutes`. Stop the unit by hand
  (`sudo systemctl stop grain-task-<sandbox>` on the sandbox) and re-run
  `automation run-once` to let the sweep collect it.
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

### Adding a repo

1. Add `"owner/repo"` to `repo-allowlist.json` (hot-reloaded).
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

The live suites skip themselves cleanly on a machine that can't run them,
so the command above is safe anywhere. They come in when the machine can:

| Suite | Needs |
|---|---|
| `test_net_integration.py` | root and a reachable netfilter |
| `test_vm_integration.py` | `/dev/kvm`, `qemu:///system`, `br-grain` up (it fetches the base image itself if missing) |
| `test_controller_integration.py` | the same, but the base image must already be cached |
| `test_live_issue_to_pr.py` | the same — a full issue→PR run against a mocked GitHub |

```sh
python3 -m tests.loadtest      # boot the real pool and measure it under kind + build load
```

This project holds itself to **verify live, not just unit tests**, and
the reason is written down in `docs/design.md`'s dispatch-mechanism
section: five separate things — the libvirt connection URI, SSH host-key
churn across recreates, `systemd-run` needing `sudo`, successful
transient units self-unloading, and SSH word-splitting an argv it never
really had — looked obviously fine on paper and only surfaced by booting
a guest and running a command on it.

Layout:

```
grain/
  inventory.py        names, addresses, ports, specs — one source of truth
  run.py              command execution behind an interface (Real/DryRun/Fake)
  cli.py              the whole operator surface
  adapter/            the only platform-specific code: libvirt, lima, nftables
  automation/         poll, dispatch, sweep, capture, session history
  proxy/              the git proxy: allowlist, tokens, credentials, audit
  metadata/           per-sandbox gce_metadata_server instances
provision/            controller.sh, sandbox.sh — cloud-init user-data
tests/
  loadtest.py         the harness behind the resource-budget numbers
  test_*.py           unit suites, plus the live suites in the table above
```

Addresses are **assigned, never discovered**: the inventory decides them
and the adapter tells the VM. Asking the hypervisor afterwards would let
the firewall rules and the VMs disagree about who is who, which is
exactly what the anti-spoofing rules exist to prevent.

## Status and limits

Implementation steps 1–10 of `docs/design.md`'s plan are done or mostly
done, most of them verified against real VMs. What is genuinely not
finished:

- **Nothing here has run against a real GitHub repo or a real
  credential.** The full issue→PR pipeline is verified end to end against
  a mocked GitHub; `grain github audit` is verified against scripted
  response shapes, not a live token. Both need a real target repo with
  admin access.
- **No token mint has been verified** against a real GCP project.
- **`git push` through the proxy is implemented but not exercised live** —
  that needs a real writable allow-listed repo. Fetch is verified against
  real `git` and real GitHub.
- **`--image` doesn't exist**, so pointing at a real base image means
  editing `Cluster.image` or driving the adapter from a script.
- Two setup steps are irreducibly manual: copying the controller's public
  key to the host (they are different machines), and deploying this repo
  to `/opt/grain` (it would need a deploy credential in a provisioning
  script).

And what the threat model does **not** defend, stated plainly: sequential
tasks on one long-lived sandbox (task B inherits task A's filesystem);
abuse of legitimate access while a sandbox is compromised; exfiltration
under the default open-egress policy; malicious code in agent output —
human PR review is the control, which is why the no-push-to-`main` rules
are load-bearing; a compromised host, which owns the hypervisor and
therefore every VM; and prompt injection via issue content, where
requiring a human label is a mitigation rather than a guarantee.
