# Operator runbook

Procedure, not reasoning — for *why*, see `docs/design.md`. This document
covers only what is actually built today, cross-checked against the code in
this repo rather than against what the design says should eventually exist.
Where something the design describes isn't wired up yet, that's called out
explicitly rather than glossed over — see especially
["Gaps: what this runbook can't yet tell you to
automate"](#gaps-what-this-runbook-cant-yet-tell-you-to-automate) at the
end.

All commands below assume you're running as the repo's `python3 -m
grain.cli ...` (shortened to `grain ...` below — there is no installed
entry point yet, so substitute the full invocation, or `alias grain='python3
-m grain.cli'`). Unless noted, run these **on the controller VM**, not the
host — the controller is where `/data` and every credential live.

## System map

| Command | What it does | Runs on |
|---|---|---|
| `grain host bootstrap --task-repo owner/name [--target-repo owner/name ...]` | One command: network, controller, deploy, configure, sandboxes, enable (docs/bootstrap.md) — replaces steps 1–12 below | host |
| `grain host up` | Creates the private bridge/network and applies the firewall policy | host |
| `grain host create/start/stop/destroy/recreate <name>` | VM lifecycle | host |
| `grain host wait <name>` | Blocks until VM(s) answer SSH and finish cloud-init | host |
| `grain host deploy [controller]` | Pushes this working tree to `/opt/grain` on the controller, no credential needed | host |
| `grain host status` | Lists VM state + assigned address | host |
| `grain host rules [--dry-run]` | Prints the firewall ruleset without applying it | host |
| `grain controller configure --task-repo owner/name [--target-repo owner/name ...]` | Writes `automation.json` (the task repo) / `repo-allowlist.json` (the target repos) and, optionally, GitHub/Claude credentials to `/data` | host (over SSH) |
| `grain sandbox login <name>` | Direct interactive admin SSH to one sandbox or the controller — for debugging | host |
| `grain automation run-once` | Sweep stranded work, then poll GitHub and dispatch | controller |
| `grain automation status` | Show current sandbox↔issue assignments | controller |
| `grain host cleanup [name]` | Between-task hygiene over SSH (`kind delete clusters --all`, `docker system prune -af --volumes`); also runs automatically after every sweep-freed sandbox | controller |
| `grain host health [name]` | SSH/systemd/docker/disk-watermark check over SSH; nonzero exit if unhealthy | controller |
| `grain sessions list [--kind/--outcome/--trigger]` | List past dispatch sessions (trigger, sandbox, outcome, whether a trajectory was captured) | controller |
| `grain sessions browse` | Interactive text UI (curses) to browse sessions and read a captured trajectory, over SSH | controller |
| `grain github audit` | Check every credential under `secrets/github/` for withheld scopes | controller |
| `python3 -m grain.proxy.server` | Runs the git proxy (not wired into `grain` yet — its own entry point) | controller |

`--data-dir` (default `/data`), `--sandboxes` (default `2`, or
`--cluster-file`'s `sandbox_count`), `--image` (a real qcow2 path,
overriding `--cluster-file`), and `--admin-ssh-public-key`/
`--controller-ssh-public-key` (defaults `/var/lib/grain/admin-ssh.pub` /
`/var/lib/grain/controller-ssh.pub`, both **host**-local — see "Key roles"
below) are **global flags on `grain` itself, before the subcommand group**
— e.g. `grain --data-dir /data automation run-once`, not `grain automation
run-once --data-dir /data`.

## First-time setup: one command

```sh
sudo python3 -m grain.cli host bootstrap \
  --task-repo your-org/agent-tasks \
  --target-repo your-org/your-repo \
  --github-token-file /path/to/token   # or '-' to pipe it in on stdin
  # --claude-token-file /path/to/token   # optional, see below
```

This is `docs/bootstrap.md`'s sequencer (`grain/bootstrap.py`) — it replaces
every step below with one idempotent command: brings the network up,
generates an admin keypair if none exists, creates and boots the controller,
reads its own SSH key back (only possible because the admin key it just
generated is trusted by the controller too — see "Key roles"), deploys this
tree to `/opt/grain`, writes `automation.json` (the task repo) and
`repo-allowlist.json` (the target repos) and, if
given, the GitHub token and Claude credential, creates and boots every
sandbox, and enables the git proxy and automation timer. `--dry-run` prints
every command it would run without touching anything. Safe to re-run: every
stage checks what's actually there before acting, so a re-run after a
failure resumes rather than redoing completed work.

**What it does not do**: place a GitHub token or Claude credential if you
don't pass one (add them with `grain controller configure` later, or a
second `host bootstrap` run), and log in to Claude Code for you — see
"Claude Code credential" below, still the one genuinely manual step.

**If it stops at "stage 5/11: wait for the controller"**: the stage prints
its own diagnostics before it raises — the domain's state, whether the guest
ever ARPed or answered a ping, and the tail of its serial console log
(`/var/lib/grain/instances/controller-console.log`) when it never answered
SSH; `cloud-init status --long` and the tail of the guest's
`/var/log/cloud-init-output.log` when it answered but provisioning failed.
On GCP all of that is in Cloud Logging with the rest of the deploy output.
See docs/bootstrap.md, "When the wait fails".

**Verify**: `grain --data-dir /data automation status` should list every
sandbox as `free`, `grain --data-dir /data github audit` should print no
`flagged` verdicts, and `grain host health` should report every sandbox
healthy.

### Key roles

Two keys, two purposes (`grain/adapter/libvirt.py`, `LibvirtAdapter.create`):

| Key | Default path (host-local) | Trusted by | Purpose |
|---|---|---|---|
| **admin** | `/var/lib/grain/admin-ssh.pub` | controller *and* every sandbox | setup, repair, and admin debugging access (`grain sandbox login`) |
| **controller** | `/var/lib/grain/controller-ssh.pub` | sandboxes only | the automation dispatch path — generated *on* the controller at first boot |

`host bootstrap` generates the admin keypair itself if `--admin-ssh-public-key`
doesn't exist yet (announced on stdout; back the private half up somewhere —
it's the only way in if the controller's own key is ever lost) and reads the
controller's key back automatically. Supplying your own admin key ahead of
time (`--admin-ssh-public-key ~/.ssh/id_ed25519.pub`) is the better habit for
a real deployment.

### Admin access: `grain sandbox login`

```sh
sudo python3 -m grain.cli sandbox login sandbox-0     # or 'controller'
```

Direct, interactive SSH using the admin key — no hop through the controller
first. For debugging: a stuck `kind` cluster, a wedged docker daemon,
anything `grain host health`/`cleanup`/`sessions browse` doesn't give enough
visibility into. This works because of the key-roles split above: the admin
key is embedded as an authorized key on every VM at create time, not just
informally reachable through whoever holds the controller's own dispatch
key. Holding the admin *private* key is what gates this, the same way
holding any other SSH key gates any other login.

No SSH access at all (a VM that fails to boot, or a controller whose own
git proxy/automation loop is wedged)? The controller's serial console is
also captured to a plain file on the **host**, `virsh console` needs no
active session for:

```sh
sudo tail -f /var/lib/grain/instances/controller-console.log
```

`provision/controller.sh` forwards the controller's own journal
(`grain-automation.service`, `grain-git-proxy.service`) to that console;
`LibvirtAdapter`'s domain XML (`grain/adapter/libvirt.py`) is what gives
the console a `<log file=...>` sink on the host in the first place. On GCP
this same file is tailed into Cloud Logging by the host's ops-agent,
configured by `terraform/gcp/files/deploy.sh` (not `startup.sh` -- deploy.sh
is what re-converges on every config-repo push, so a change here reaches an
already-running host without a reboot), so it's also readable from the
Cloud Console without SSH/IAP access to the host at all.

### Claude Code credential

`claude -p` runs on the controller now, as a dedicated `grain-agent`
account — never on a sandbox, and never using an operator's own personal
login (`docs/design.md`, "Final choice: no credential in the sandbox at
all"). One token for the whole pool, not one per sandbox: generate a
dedicated one with `claude setup-token` (deliberately not your own `claude
login` session, so this deployment's dispatch traffic never rides on a
personal credential), then pass it to `host bootstrap --claude-token-file
<path>` (or `grain controller configure --claude-token-file <path>` on its
own). It is placed at `/data/secrets/claude-oauth-token` on the controller
(a root-owned reference copy) and again at `grain-agent`'s own
`~/.claude-oauth-token` (the live copy `dispatch.py`'s own unit script
reads into `CLAUDE_CODE_OAUTH_TOKEN` at runtime) — nothing is ever placed
on a sandbox.

## First-time setup checklist (manual, step by step)

The bootstrap command above is a sequencer over exactly these steps — read
this section when something needs doing by hand (a step failed, you want to
understand what a stage actually does, or you're intentionally stopping
short of giving the host a GitHub/Claude credential — see
`docs/bootstrap.md`, "What must not break" for that variant).

1. **Host baseline.** Debian, nested virtualization enabled, `/dev/kvm`
   present. Confirm with `ls /dev/kvm`.
2. **Bring the host network up**:
   ```sh
   sudo python3 -m grain.cli host rules            # read the policy first
   sudo python3 -m grain.cli host up --persist
   ```
   This creates the `br-grain` bridge and applies the default-open-egress
   nftables policy. Idempotent — safe to re-run after any inventory change.
   `--persist` also installs and enables `grain-network.service`, a oneshot
   unit that reruns this exact command at every boot: without it, a host
   reboot leaves the sandboxes (which come back on their own via libvirt's
   autostart) running with *no* firewall at all — sandbox-to-sandbox
   isolation and the metadata anycast DNAT both silently gone, while SSH
   and a direct connection to the controller keep working, so nothing looks
   wrong until something inside a sandbox tries to reach
   `169.254.169.254` (bwsalmon/agents#111). Re-run `host up --persist`
   after any change to `--egress` too — the unit bakes in whichever mode
   was active when it was last installed, not whatever is current.
3. **Fetch a base image**, shared by the controller and every sandbox, and
   point `--image` at it (or set it in `--cluster-file`'s TOML — see
   `docs/bootstrap.md` Phase 2):
   ```sh
   sudo mkdir -p /var/lib/grain/images
   curl -fsSL -o /var/lib/grain/images/debian-12.qcow2 \
     https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
   sudo python3 -m grain.cli --image /var/lib/grain/images/debian-12.qcow2 host create ...
   ```
   `LibvirtAdapter.create()` passes `Cluster.image` straight to `qemu-img
   create -b <image>` as a backing file — it must be a **local path to a
   qcow2 image**, not a name Lima or libvirt resolves for you.
   `Cluster.image`'s own default is still the bare string `"debian-12"`,
   which is not a real path — always pass `--image` (or set it in the
   cluster file) before creating anything.
4. **Create the controller VM**, provisioned by `provision/controller.sh`
   (run this **on the host**):
   ```sh
   sudo python3 -m grain.cli host create controller --provision provision/controller.sh
   ```
   Use the `controller` target specifically, not `all` — `grain host
   create`/`recreate` now refuses `--provision` combined with `all`, since
   the controller and the sandboxes need different scripts. The script
   installs Python 3.11+ and `gcloud` (for `grain/automation/gemini_keys.py`
   and `grain/automation/gcp_keys.py`'s own gcloud calls), the
   `/data/{secrets,config,state}` layout every module in `grain/automation`
   and `grain/proxy` expects, the systemd units for the automation timer
   and the git proxy (installed but not enabled), and **generates the
   controller's own SSH keypair**,
   `/data/secrets/controller-ssh{,.pub}`, idempotently, on the controller
   itself. It does not deploy this repo's own code, and it does not enable
   any service; both need real data that a provisioning script has no
   business holding. See the script's own comments, or
   `/etc/grain-tools/README` on the controller once it's booted.
5. **Wait for it, then copy the controller's SSH public key to the host.**
   The host (running `grain host create`) and the controller (running
   everything else) are different machines (`docs/design.md`, "One host
   machine runs everything"), so a file generated *on* the controller has
   to be carried across that gap before `LibvirtAdapter` (which runs *on
   the host*) can embed it into a sandbox's cloud-init as an authorized
   key. This only works non-interactively if the host already holds an
   **admin** key the controller trusts (see "Key roles" above) — with one
   present:
   ```sh
   sudo python3 -m grain.cli host wait controller
   ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
     cat /data/secrets/controller-ssh.pub \
     | sudo tee /var/lib/grain/controller-ssh.pub > /dev/null
   ```
   It is a public key, not a secret, so this copy needs no special handling
   beyond getting the bytes across correctly. A sandbox created *before*
   this file exists on the host gets no controller-role authorized key at
   all (`render_meta_data` treats a missing file as "no key to inject"
   rather than erroring), so do this before the next step.
6. **Create the sandbox VMs**, provisioned by `provision/sandbox.sh` (also
   on the host):
   ```sh
   sudo python3 -m grain.cli host create sandboxes --provision provision/sandbox.sh
   sudo python3 -m grain.cli host wait sandboxes
   sudo python3 -m grain.cli host status
   ```
7. **Deploy this repo's code to the controller**, at `/opt/grain` (created,
   empty, by `provision/controller.sh`). `grain` has no third-party
   dependencies (`pyproject.toml` — stdlib only), so this is the only thing
   missing before `python3 -m grain.cli` works there:
   ```sh
   sudo python3 -m grain.cli host deploy
   ```
   No credential needed — `grain/adapter/deploy.py` pipes a `tar` of this
   working tree over the same admin SSH path, and extracts it as root. (The
   manual equivalent, `ssh debian@10.100.0.2 sudo git clone <remote>
   /opt/grain`, still works if you'd rather deploy from a remote directly.)
8. **GitHub credential files**, under `/data/secrets/github/`:
   ```
   /data/secrets/github/
     credentials.json     # {"owner/repo": "name", "owner/*": "name", "*": "name"}
     bot.token             # e.g. the machine-account PAT
     personal.token         # last resort
   ```
   `grain controller configure --task-repo owner/name [--target-repo
   owner/name ...] --github-token-file PATH` writes both, with one
   exact-repo pattern per repo it was given (the task repo, so the
   orchestrator can read issues and move labels, plus every target repo, so
   it can check a branch and open a PR), over the admin SSH path (stdin, never argv — see
   `grain/automation/configure.py`). Doing it by hand: every value in
   `credentials.json` other than the literal string `"anonymous"` must have
   a matching `<name>.token` file next to it (`grain/proxy/credentials.py`)
   — a `0600` file holding the raw token, trailing whitespace stripped.
   **Do not grant `workflow`, `delete_repo`, `write:org`, or any `admin:*`
   scope to any of these** — see [Credential audit](#credential-audit)
   below.
9. **The repo allowlist**, `/data/config/repo-allowlist.json` — the
   *target* repos this deployment may work in: a plain JSON array of
   `"owner/repo"` strings, default-deny, hot-reloaded
   (`grain/proxy/allowlist.py` re-reads it on every request, no restart
   needed). Written by `grain controller configure` from its
   `--target-repo` values, alongside `automation.json` (step 11). Enforced
   in two places against this one file: the git proxy on every fetch and
   push, and the orchestrator when it resolves a task's `/repo` directive
   (a task naming an off-list repo is parked with a comment rather than
   dispatched). The **task** repo does not belong here — no sandbox ever
   clones it; the orchestrator reads it over the API, which
   `credentials.json` covers. A repo must be on this list *and* covered by
   a `credentials.json` pattern before the proxy will forward anything for
   it.
10. **Sandbox tokens**, `/data/secrets/sandbox-tokens.json` — already
    unnecessary: `SandboxTokenStore.ensure_token()`
    (`grain/proxy/tokens.py`) mints and records one per sandbox,
    idempotently, on first dispatch. Nothing to do here.
11. **`automation.json`**, `/data/config/automation.json` — names the
    *task* repo, the one queue polled for labelled issues
    (`AutomationConfig`, `grain/automation/config.py`); `task_owner` and
    `task_repo` are its only fields with no default:
    ```json
    {"task_owner": "your-org", "task_repo": "agent-tasks",
     "default_target_repo": null}
    ```
    Written by `grain controller configure --task-repo owner/name` (step
    8). `default_target_repo` is the target for a task carrying no `/repo`
    directive; `null` makes the directive mandatory, and a task without one
    is parked with a comment. Passing no `--target-repo` at all produces
    the single-repo shape instead: the task repo as the sole allow-listed
    target *and* the default, so no task needs a directive.
    Everything else has a default worth knowing: `trigger_label:
    "grain-agent"`, `in_progress_label: "grain-agent-in-progress"`,
    `awaiting_reply_label: "grain-agent-awaiting-reply"`,
    `ssh_user: "debian"`, `ssh_key_path: "/data/secrets/controller-ssh"`,
    `runs_per_hour: 60`, `max_runtime_minutes: 120`. Override any of them by
    editing the file directly.
12. **Enable the git proxy and the automation timer**, now that `/opt/grain`
    holds real code and `/data` holds real credentials (SSH to the
    controller for this — `provision/controller.sh` installed these units
    but deliberately left them disabled):
    ```sh
    sudo systemctl enable --now grain-git-proxy.service
    sudo systemctl enable --now grain-automation.timer
    ```
13. **Claude Code token.** See "Claude Code credential" above — one
    dedicated `claude setup-token` value, placed once via `grain controller
    configure --claude-token-file`/`host bootstrap`, never a sandbox-side
    ritual.
14. **Verify before trusting it**: `grain --data-dir /data automation
    status` should list every configured sandbox as `free`, and `grain
    --data-dir /data github audit` should print one line per credential
    file with no `flagged` verdicts (see below).

## Running automation

`grain automation run-once` does one pass: sweep stranded/finished work,
poll the **task repo** for open issues carrying `trigger_label`, resolve
each one's target repo from its own `/repo` directive, and dispatch to any
free sandbox within the rate limit. A fresh task opens a new PR in its
target repo once its branch shows up; a task carrying `/pr N`
(docs/roadmap.md items 9 and 15 — continue an *existing* PR to address
review feedback, fix CI, or finish work in flight) just pushes more commits
to that PR's own branch, already checked out — no new PR to open. Both
share the same pool and the same budget, since it's the same finite set of
sandboxes either way; labels and comments stay on the task issue in both
cases, and a task whose directive can't be honoured is parked with a
comment rather than retried blindly. It is meant to be **invoked
periodically by something else, not run as a daemon** — `docs/design.md`
says "invoked by a systemd timer."

**That timer is now shipped**: `provision/controller.sh` installs
`grain-automation.service`/`.timer` (a 30-second `OnUnitActiveSec`, same
shape this section used to hand-draft) and `grain-git-proxy.service`, all
disabled until step 12 of the first-time setup checklist enables them —
they can't do anything useful before `/opt/grain` holds real code and
`/data` holds real credentials. `systemctl cat grain-automation.timer` on
the controller shows the exact unit; edit `/etc/systemd/system/*` directly
and `systemctl daemon-reload` for a different cadence.

Live-verified (`tests/test_controller_integration.py`, boots a real
controller VM): the units land on disk with the right paths and stay
disabled after provisioning. **Not** verified: the units actually *running*
`automation run-once`/the proxy successfully — that needs `/opt/grain`
populated and real credentials in `/data`, neither of which a provisioning
script can supply (see step 12).

`grain automation status` reads `/data/state/automation/state.json` and
prints each sandbox's assignment (or `free`). Safe to run any time; it makes
no GitHub or SSH calls.

Every dispatch/sweep decision is appended to
`/data/state/automation/audit.log` (one JSON object per line —
`grain/automation/audit.py`): which sandbox, which issue, and the outcome
(`dispatched`, `succeeded`, `failed`, `stranded`, or a `skipped: ...`
reason). This is the first place to look when a run behaves unexpectedly.

## Browsing past sessions (docs/roadmap.md item 10)

`AutomationState`/`audit.log` above are both about the *live* pool and a
one-line-per-decision log — neither lets an operator go back and read what
an agent actually did on a past run. `grain sessions list` and `grain
sessions browse` do, keyed by the trigger (the issue or PR number) that
started each session:

```
$ grain --data-dir /data sessions list
2026-01-01 12:00  issue#42   succeeded  sandbox-0    transcript=yes grain-task-sandbox-0
2026-01-01 11:40  pr#7       failed     sandbox-1    transcript=yes grain-task-sandbox-1
2026-01-01 10:05  issue#41   stranded   sandbox-0    transcript=no  grain-task-sandbox-0
$ grain --data-dir /data sessions list --kind pr --outcome failed
$ grain --data-dir /data sessions browse   # interactive: list, filter, select, read
```

`sessions browse` is a `curses` text UI — list sessions, cycle the kind/
outcome filter (`k`/`o`), select one (arrow keys, `enter`) to read its
captured trajectory, `q` to go back or quit. It needs a real terminal (an
SSH session is one); `sessions list` is the same data for a script or a
plain non-interactive shell.

**Where the data comes from**: `sweeper.py`'s release path — the same place
`grain/automation/cleanup.py` and `health.py` already hook into (see
"Stranded sandboxes" below) — reads the finished session's trajectory
*before* the sandbox's slot is freed for reuse, and records it into
`/data/state/automation/sessions/`. `claude -p` runs on the controller now
(`docs/design.md`, "Final choice"), so this is a plain local file read, not
an SSH pull, though the load-bearing timing is identical either way: a
sandbox is long-lived and its next task's `claude -p` run overwrites the
same fixed transcript path, so a later "fetch it when
someone wants to browse" would find nothing or the wrong task's content.
`claude -p` is run with `--output-format stream-json --verbose`, redirected
to that fixed path (`grain/automation/dispatch.py`'s `transcript_path()`) —
see `grain/automation/capture.py`'s module docstring for what was actually
checked (not assumed) about the real format Claude Code's session
persistence uses on disk, and why this project captures a redirected stream
at a location it controls rather than depending on that internal,
undocumented file-naming scheme.

A session with no captured trajectory (`transcript=no` above) means the
unit never got far enough to write one — most commonly a genuinely stranded
run (never started, or the sandbox was recreated out from under it); the
session record itself is still kept, just without transcript content.

## Credential file layout and rotation

```
/data/
  secrets/
    controller-ssh, controller-ssh.pub   # controller -> sandbox SSH identity
    sandbox-tokens.json                  # sandbox name -> bearer token (git proxy auth)
    gcp-service-account.json             # primary GCP key gemini_keys.py authenticates with (optional)
    github/
      credentials.json                   # owner/repo pattern -> credential name
      <name>.token                       # one file per credential named in credentials.json
  config/
    repo-allowlist.json                  # ["owner/repo", ...], default-deny
    automation.json                      # AutomationConfig
    cluster.toml                         # sandbox_count, subnet -- the controller's own copy; see below
    gemini-key.json                      # GeminiKeyConfig (optional, see below)
    gcp-key.json                         # GcpKeyConfig (optional, bwsalmon/agents#126, see below)
    sandbox-github-key.json              # sandbox name -> named credential override, if any (bwsalmon/agents#52)
  state/
    automation/state.json, audit.log
    automation/sessions/<key>.json, <key>.jsonl   # session history + captured trajectories
    git-proxy/audit.log
```

Rotation is uniformly **"replace the file, restart the one service that
reads it"** (`docs/design.md`, "Operations") — nothing here watches
`secrets/` for changes the way the allowlist watches `config/`:

- **A GitHub credential** (`bot.token`, `personal.token`, a named
  `grain-github-<name>` key, ...): overwrite the file with the new token,
  `chmod 0600` it, restart the git proxy (`python3 -m grain.proxy.server`)
  and, if `automation.json`'s `owner`/`repo` resolve to that credential,
  the process invoking `automation run-once` too — `CredentialSet` and
  `build_orchestrator` both load once at construction
  (`grain/proxy/credentials.py`'s own docstring is explicit about this), so
  a running process keeps using the old token until restarted. (The
  *selection* of which sandbox uses which named key,
  `/data/config/sandbox-github-key.json`, is a different file, lives under
  `config/` rather than `secrets/`, and *is* hot-reloaded — see "Adding a
  named GitHub key" below.)
- **A sandbox token**: generate a new one, update its entry in
  `sandbox-tokens.json`, restart the git proxy. **The old token stays valid
  everywhere else that reads the same file until the proxy restarts** —
  there's no in-place invalidation. Note also that `HostAdapter.recreate()`
  (`grain/adapter/base.py`) does **not** touch `sandbox-tokens.json` at
  all, despite `docs/design.md` describing rotation as "folded into
  recreate" — today that's aspirational, not implemented. Recreating a
  sandbox does *not* rotate its token; do that as a separate, manual step.
- **`cluster.toml`**: written by `configure_cluster`
  (`grain/automation/configure.py`) on every `grain host bootstrap` run, not
  just the first — the host's own `--cluster-file` never leaves the host,
  so without this copy `grain-automation.service`'s `--cluster-file
  /data/config/cluster.toml` (`provision/controller.sh`) would resolve to
  nothing and `Cluster.load` would silently fall back to its bare defaults
  (`sandbox_count=2`) forever, regardless of what the real deployment's
  `cluster.toml`/`cluster_overrides` said. Only `sandbox_count` and
  `subnet` are written — the only two fields `sandbox_names`/`address_of`
  depend on; everything else in `Cluster` (VM sizing, image, bridge) is a
  host-side-only concern. Takes effect on the next `automation run-once`
  tick (every 30s); no restart needed, since it's a oneshot invoked fresh
  each time, not a long-lived process holding a stale copy in memory.
- **The controller SSH key**: generated once, on the controller, by
  `provision/controller.sh` (idempotently — it will not touch an existing
  `/data/secrets/controller-ssh`). `grain host recreate controller
  --i-know-this-deletes-data` — the flag is required because `/data` has no
  disk of its own yet and lives on the controller's own qcow2, so this also
  destroys every credential and all automation state, not just the SSH key
  — followed by `grain host bootstrap --task-repo owner/name` (no need to repeat the
  GitHub/Claude credential flags — see below) now handles rotation without a
  full sandbox recreation cycle: stage 6 reads the *new* key back over the
  admin SSH path (only possible because of the "key roles" split — see
  above) and, since it differs from what's on file, stage 9 repairs every
  already-existing sandbox by appending the new key to `authorized_keys`
  over that same admin path, rather than baking it in at creation time as
  before (`docs/bootstrap.md`, "Repairing a recreated controller"). Doing it
  by hand instead: delete both files on the controller, re-run the keygen
  line from `provision/controller.sh`, copy the new `.pub` to the host's
  `--controller-ssh-public-key` path, then either re-run `host bootstrap` or
  manually `ssh -i <admin key> ... "echo <new pubkey> >> ~/.ssh/authorized_keys"`
  against each sandbox.

## Credential audit

`grain github audit` (`grain/automation/credential_audit.py`) walks every
`*.token` file directly under `secrets/github/` and, for each, tries to
determine whether it carries a scope `docs/design.md`'s "Scopes to
withhold" section says must never be granted: `workflow`, `delete_repo`,
`write:org`, or anything `admin:*`.

```
$ grain --data-dir /data github audit
bot          classic PAT                                   ok           scopes: repo, read:org
personal     classic PAT                                   flagged      carries withheld scope(s): workflow (full grant: repo, workflow)
ci-app       fine-grained PAT                               unverifiable  fine-grained PAT: GitHub exposes no scopes/permissions header for this token type via the API; check its permissions by hand at github.com's token settings (or, for a GitHub App, its installation permissions page).
```

Exit code is **1 if any credential is `flagged`**, `0` otherwise (including
when results are `unverifiable` — that's a "go check by hand," not a
confirmed pass).

What it can and can't tell you, and why (verified against GitHub's own
docs, not assumed — see the module's docstring for citations):

- **Classic PATs and OAuth tokens** (`ghp_…`, `gho_…`, or the un-prefixed
  40-hex-character format that predates GitHub's 2021 token-format change)
  return their granted scopes on *any* authenticated API call, in a
  `X-OAuth-Scopes: repo, user` response header. This is the only case the
  tool actually checks — it makes one `GET /user` call per such credential.
- **Fine-grained PATs** (`github_pat_…`) and **GitHub App tokens**
  (`ghu_…`/`ghs_…`) expose no such header, and GitHub currently has **no
  API to introspect a fine-grained PAT's permissions at all**. For these
  the tool reports `unverifiable` and makes no network call — it does not
  guess. Check a fine-grained PAT's permissions at
  `github.com/settings/personal-access-tokens`; check an App's at its
  installation settings page.

This has **not been run against a real credential** — this environment has
no file under `/data/secrets/github/` and no network path to
`api.github.com`. It is fully unit-tested against `FakeTransport` scripted
with GitHub's real response shapes
(`tests/test_automation_credential_audit.py`), which is a meaningfully
different claim from "verified live." Run it for real the first time a real
credential is configured, per `docs/roadmap.md` items 7 and 8.

## Stranded sandboxes: automatic vs. manual

`grain automation run-once` sweeps before it dispatches
(`grain/automation/sweeper.py`), and handles these cases **automatically**,
with no operator action:

- The dispatched unit finished successfully → label moved back off
  in-progress, sandbox freed, PR opened once the pushed branch is verified
  to exist.
- The unit finished with a failure → issue re-labelled with the trigger
  label (put back in the queue), sandbox freed.
- The unit is missing entirely (never started, or the sandbox was recreated
  out from under it) or has run past `max_runtime_minutes` → treated as
  stranded, issue re-labelled, sandbox freed.
- **Every one of the three releases above also captures the session's
  trajectory** (`docs/roadmap.md` item 10, `grain/automation/capture.py` —
  see ["Browsing past sessions"](#browsing-past-sessions-docsroadmapmd-item-10)
  above) **, runs between-task cleanup**
  (`kind delete clusters --all`, `docker system prune -af --volumes` — not
  a clone-directory wipe, see below) **and a post-cleanup health check**
  (`docs/roadmap.md` item 5, `grain/automation/cleanup.py` and
  `grain/automation/health.py`). A sandbox is guaranteed clean the moment
  its slot is freed for reuse — no separate cron job, no manual step. A
  health problem found this way (SSH unreachable, `docker info` failing,
  a degraded `systemctl is-system-running`, or disk at/above the 85%
  watermark) is written to `/data/state/automation/audit.log` as a
  `"health warning: ..."` line, **but does not remove the sandbox from the
  dispatch pool** — this is visibility, not gating; see
  `grain/automation/sweeper.py`'s docstring for the reasoning. Note that
  cleanup deliberately does **not** clear the workspace directory —
  `dispatch.py`'s `ensure_workspace()` already resets it to a known-clean
  state on every dispatch, so wiping it here would only force a slower
  full re-clone next time with no correctness benefit.
- **`grain host cleanup [name]`** and **`grain host health [name]`** run the
  same two checks standalone, for a sandbox that isn't mid-sweep at all (a
  free sandbox, or before waiting on a cron cycle). `health`'s exit code is
  nonzero if any sandbox comes back less than fully healthy, matching
  `grain github audit`'s convention. Both default to `--ssh-user debian
  --ssh-key /data/secrets/controller-ssh` (`AutomationConfig`'s own
  defaults) and don't require `automation.json` to exist, unlike
  `grain automation run-once`.

What is **not automatic**:

- **A wedged sandbox that still reports `ACTIVE`** — e.g. `claude -p` is
  hung but hasn't exceeded `max_runtime_minutes` yet. The sweeper leaves it
  alone by design (it can't distinguish "slow" from "stuck"). `claude -p`
  runs on the **controller** now (`docs/design.md`, "Final choice"), so the
  unit to check is there too, not on the sandbox: `grain automation status`
  plus, on the controller itself, `systemctl status
  grain-task-<sandbox>`. Either wait it out or manually stop the unit
  (`sudo systemctl stop grain-task-<sandbox>`, on the controller) and
  re-run `automation run-once` to let the sweep pick it up as stranded.
- **Acting on an unhealthy reading.** `grain host health` and the sweeper's
  own post-cleanup check both *report* a problem; neither one recreates,
  quarantines, or stops dispatching to a degraded sandbox. If
  `grain host health` (or an audit-log `"health warning"` line) flags one,
  the operator decides: `grain host recreate <name>` is the usual fix for
  anything short of "watch it."
- **Base-image updates** — recreate is the deploy path
  (`grain host recreate <name> --provision provision/sandbox.sh`), but
  nothing schedules it; `docs/design.md` suggests weekly.

## Adding or reconfiguring a target repo

A *target* repo is one that tasks may dispatch into. The **task** repo (the
polled queue) is set once, in `automation.json`; pointing a deployment at a
different one is a `grain controller configure --task-repo ...` run, not
this procedure.

1. Add `"owner/repo"` to `/data/config/repo-allowlist.json` (hot-reloaded —
   no restart). Until it is on that list, a task naming it with `/repo
   owner/repo` is parked with a comment saying exactly that, rather than
   dispatched.
2. Add a `credentials.json` entry (exact repo, `owner/*`, or leave it
   covered by the existing `*` fallback) pointing at a credential name with
   a matching `<name>.token` file.
3. Apply branch protection / a repository ruleset on that repo: no direct
   pushes to the default branch from the agent credential, no force-push,
   no deletion, PRs required (`docs/design.md`, "Write safety"). **This is
   a GitHub-side, per-repo action this runbook cannot script** — it needs
   admin on the target repo and is not something any code in this repo
   does today.
4. Run `grain github audit` and confirm the credential covering the new
   repo isn't `flagged`.
5. If using the machine-account pattern, invite `grain-agent-bot` (or
   whatever account the token belongs to) as a collaborator on the repo.

## Enabling `grain-gemini-key` (optional, bwsalmon/agents#47, #49)

A task issue carrying the `grain-gemini-key` label gets a short-lived
Gemini API key minted for it, placed in its sandbox, and revoked once the
task's slot frees (success, failure, or stranded — see
`grain/automation/sweeper.py`'s docstring). A label, not a body directive
— `grain/automation/directives.py`'s module docstring has the reasoning.
Off by default; nothing above requires it. Terraform-managed deployments
(`terraform/gcp/`) can turn the IAM side of this on declaratively with
`enable_gemini_key = true` in `grain.tfvars` instead of steps 1-2 below —
see `terraform/gcp/variables.tf`.

1. Complete step 8's GCP service account setup first
   (`--gcp-service-account-key-file`/`--gcp-agent-service-account-email`/
   `--gcp-project-id`) — `grain/automation/gemini_keys.py` authenticates
   `gcloud` with that same primary key rather than asking for a second one.
2. Grant that service account `roles/serviceusage.apiKeysAdmin` (or an
   equivalent narrower role covering `apikeys.keys.create`/`.delete`/
   `.get`) on the project, and make sure the Generative Language API
   (`generativelanguage.googleapis.com`) is enabled there.
3. Run `grain controller configure --gemini-project-id <project>` (any
   other `controller configure` flags in the same invocation still apply
   normally — this one is additive). This writes
   `/data/config/gemini-key.json`, read fresh on every `automation
   run-once` invocation, same as `repo-allowlist.json`. A Terraform-managed
   deployment does this automatically as part of `host bootstrap` instead
   — see `terraform/gcp/files/deploy.sh`.
4. A task now enables it by carrying the `grain-gemini-key` label — see
   the README's directives section. Until step 3 is done, that label parks
   the task with a comment explaining why, the same as an unlisted
   `/repo`.

To disable it again: delete `/data/config/gemini-key.json`. Any key already
minted for a task still in flight is unaffected (it still gets revoked
normally when that task's slot frees) — this only stops new ones from
being minted.

## Enabling GCP access in sandboxes (optional, bwsalmon/agents#126)

Every dispatched sandbox gets a freshly minted, short-lived GCP service-
account key pushed into it, revoked once the task's slot frees (success,
failure, or stranded — same checkpoint as the Gemini key above), or after
24 hours regardless, whichever comes first (see
`grain/automation/gcp_keys.py`'s docstring for why GCP itself enforces no
such expiry and grain has to). Unlike the Gemini key, this is *not* gated
on a task label — it mirrors the old per-sandbox metadata-server broker's
"every sandbox, every dispatch" behaviour. Off by default; nothing above
requires it. Terraform-managed deployments (`terraform/gcp/`) wire the IAM
side of this up automatically whenever `agent_service_account_roles` (or
`agent_can_manage_compute_instances`) creates the agent account at all —
see `terraform/gcp/iam.tf`'s `host_manages_agent_keys`.

1. Have an agent service account (`agent_service_account_roles` or
   `agent_can_manage_compute_instances` in `grain.tfvars`, or one created
   by hand) that the host account has `roles/iam.serviceAccountKeyAdmin`
   on — Terraform-managed deployments get this automatically; by hand,
   grant it yourself.
2. Run `grain controller configure --gcp-agent-service-account-email
   <email> --gcp-project-id <project>` (any other `controller configure`
   flags in the same invocation still apply normally — this one is
   additive, and needs neither `--gcp-service-account-key-file` nor a
   Gemini key setup, unrelated features that happen to share this same
   command). This writes `/data/config/gcp-key.json`, read fresh on every
   `automation run-once` invocation. A Terraform-managed deployment does
   this automatically as part of `host bootstrap` instead — see
   `terraform/gcp/files/deploy.sh`.
3. Every dispatch from here on mints a key and places it in the sandbox at
   `~/.gcp-service-account.json`; the agent's own prompt tells it the path
   and how to use it (`gcloud auth activate-service-account --key-file=...`,
   or `export GOOGLE_APPLICATION_CREDENTIALS=...` for client libraries).

To disable it again: delete `/data/config/gcp-key.json`. Any key already
minted for a task still in flight is unaffected (it still gets revoked
normally when that task's slot frees, or reaped at 24 hours) — this only
stops new ones from being minted.

## Enabling `grain-self-debug` (bwsalmon/agents#62, #86)

A task issue carrying the `grain-self-debug` label gets four extra MCP
tools, all strictly read-only, for triaging a bug in grain itself rather
than the target repo's own code:

- `read_grain_logs`: recent `journalctl` entries for grain's own
  controller services, `grain-automation.service` and
  `grain-git-proxy.service` — or, via `unit: grain-task`
  (bwsalmon/agents#97), this dispatch's own controller-side `claude -p`
  unit. That unit's stdout is what `capture.py` redirects into the
  transcript file, but its stderr never is, so `grain-task` is the only
  way to see why a run crashed before writing anything to it.
- `check_grain_health`: `health.py`'s ssh/systemd/docker/disk checks —
  the same ones `grain host health` reports — against either the task's
  assigned sandbox or the controller itself.
- `read_grain_config`: one of the deployment's own non-secret config
  files under `/data/config` (`automation.json`, `repo-allowlist.json`,
  `gemini-key.json`, `gcp-key.json`, `sandbox-github-key.json`),
  checked against a fixed allowlist of those five names — never a raw
  path, and never anything under `/data/secrets`.
- `read_automation_audit_log`: recent lines of `audit.py`'s own
  `FileAuditLog` output (`/data/state/automation/audit.log`) — one JSON
  line per dispatch/sweep decision the state machine in `core.py` made.

The exact opposite of `grain-gemini-key` in one way: nothing here needs a
`controller configure` step or an operator decision to turn on.
`provision/controller.sh` grants `grain-agent` read-only
`systemd-journal` group membership unconditionally (for `read_grain_logs`
specifically), and every file the other three tools read is already
either world-readable by construction (`/data/config`'s contents,
`/data/state/automation/audit.log`) or reachable over the same SSH path
`grain-agent`'s MCP server already has to the sandbox — so any deployment
provisioned from this repo already has everything these tools need. The
label on a task issue is the only thing that decides whether that
particular task's agent gets them at all.

There is nothing to disable here short of not applying the label — there
is no config file gating it the way `gemini-key.json` gates
`grain-gemini-key`.

## Enabling `grain-self-repair` (bwsalmon/agents#99)

A task issue carrying the `grain-self-repair` label gets four more MCP
tools — the mutating counterpart to `grain-self-debug` above, kept behind
a deliberately separate label since every self-debug tool is read-only and
these are not:

- `restart_grain_service`: `systemctl restart` on
  `grain-automation.service` or `grain-git-proxy.service`, for a wedged
  service short of rebooting the whole controller.
- `reboot_sandbox`: reboot the task's own assigned sandbox VM.
- `reformat_sandbox`: run the same between-task hygiene
  (`kind delete clusters --all`, `docker system prune -af --volumes`)
  the sweeper already runs automatically once a task finishes, callable
  mid-task instead of only between tasks.
- `reboot_controller`: reboot the controller VM the task's own `claude -p`
  session is running on — the drastic, last-resort one. This ends the
  task's turn immediately (the process making the call is about to be
  killed) and interrupts every other task running concurrently on the same
  controller, which grain's own stranded-work sweep recovers automatically
  once the controller is back — the same incremental-state-persistence
  path `docs/next-session.md` describes existing specifically so "the
  controller VM can be restarted or recreated at any moment" without
  losing a task.

Same "no per-deployment config to turn on" shape as `grain-self-debug`:
`provision/controller.sh` grants `grain-agent` a narrow, unconditional
NOPASSWD sudo rule (`/etc/sudoers.d/grain-agent-self-repair`) covering
exactly the three command lines `restart_grain_service`/`reboot_controller`
need and nothing else; `reboot_sandbox`/`reformat_sandbox` need no new
grant at all, since the sandbox's own cloud-init default user already
carries passwordless sudo (docker group membership already covers
`reformat_sandbox`'s two commands). The label on a task issue is what
decides whether that task's agent actually gets any of the four.

**What this deliberately does not do.** The issue that asked for this
(bwsalmon/agents#99) named two more operations — recreating a sandbox
from scratch, and re-running `grain host bootstrap` to reconverge the
whole deployment — that are not exposed here, because they cannot be from
where these tools run. Both are `HostAdapter` operations
(`grain/adapter/base.py`) against the *host* machine's hypervisor, and the
controller has no credential or network path to that machine at all in
this design (`docs/design.md`, "One host machine runs everything";
`provision/controller.sh` notes plainly that "the public half does NOT
reach the host by anything this script can do" — admin access goes the
other direction, host to controller). Closing that gap for real would mean
building a new host-reachable channel (a host-side listener the controller
authenticates to, or, on GCP, granting the controller's own service
account IAM over the host instance it's itself running inside of, which
`terraform/gcp/iam.tf`'s `agent_conditioned_compute_roles` deliberately
excludes today) — a real design decision with its own blast-radius
tradeoffs, not something to wire up silently as a side effect of this
label. `reboot_sandbox`/`reformat_sandbox`/`reboot_controller` above cover
the recovery ground reachable without it; a genuinely bad VM (corrupted
disk, bad base image) still needs an operator's `grain host recreate`.

## Adding a named GitHub key (bwsalmon/agents#52)

A task labelled `grain-github-<name>` pushes through the git proxy using
the named credential `<name>` for that task only, instead of the
deployment's default (`credentials.json`-selected) one — for a task that
genuinely needs a scope the default deliberately withholds, `workflow`
being the case this was built for (`docs/design.md`, "Scopes to
withhold"). Provisioning one is a single step, and there is no on/off
switch to flip first:

1. Run `grain controller configure --repo owner/name --github-key
   <name>=PATH` (any other `controller configure` flags in the same
   invocation still apply normally — this one is additive, and repeatable
   for more than one named key). This writes only
   `/data/secrets/github/<name>.token` — deliberately **not** a
   `credentials.json` entry, so `<name>` never becomes any repo's default
   credential, only a label-selected override.
2. A task now asks for it by carrying a `grain-github-<name>` label — a
   real GitHub label, applied by a human, the same trust tier as the
   trigger label itself; not a `/directive` line in the issue body. Until
   step 1 is done for that name, the label parks the task with a comment
   explaining why, the same as an unlisted `/repo`. Two different
   `grain-github-*` labels on the same issue park it too — which one
   applies would otherwise be a guess.
3. Run `grain github audit` and expect `<name>` to come back **`flagged`**
   if it carries `workflow` (or another withheld scope) — that is correct,
   not a bug: the scope really is there, deliberately, for this one
   credential. `bot`/`personal` (or whatever covers `credentials.json`'s
   `*` fallback) should still come back clean.

The override applies only for the lifetime of the task that asked for
it — written to `/data/config/sandbox-github-key.json` right before
dispatch (hot-reloaded, no proxy restart needed, the same as
`repo-allowlist.json`) and cleared the moment that sandbox's slot frees,
whether the task succeeded, failed, or was stranded. There is nothing to
disable deployment-wide: simply stop applying the label, or delete the
`<name>.token` file (a label still naming it then parks the task, same as
step 1 never having run).

## Gaps: what this runbook can't yet tell you to automate

Everything below needs either a real target GitHub repo/org, or code this
repo doesn't have yet. Tracked in `docs/roadmap.md`:

- **Deploying this repo's own code and copying the controller's public key
  across the host/controller boundary** (first-time setup steps 5 and 7) —
  both now scripted by `grain host deploy`/`grain host bootstrap`
  (`docs/bootstrap.md`), closing what used to be listed here as
  irreducible. Doing either by hand (steps 5/7 above) still works and is
  occasionally useful for debugging the sequencer itself.
- **`--image`/`--cluster-file`** — closed (`docs/bootstrap.md` Phase 2):
  `Cluster.load()` reads sandbox count, subnet, bridge, image, and per-role
  sizing from a TOML file, and `--image` overrides just the image inline.
  `Cluster.image`'s own dataclass default is still the placeholder string
  `"debian-12"`, so an explicit `--image` (or cluster file) is still
  required before creating anything real — that part isn't automatic, only
  no-longer-requiring-a-source-edit.
- **`recreate()` does not rotate the sandbox token** despite the design
  describing rotation as folded into it — do it as a separate manual step
  (see [Credential audit](#credential-audit) / rotation above).
- **A health warning doesn't quarantine a sandbox** — `grain host health`
  and the sweeper's own post-cleanup check both only report; recreating a
  degraded sandbox is still an operator decision (roadmap item 5). Also not
  verified live: the fully-healthy path (needs a provisioned sandbox with
  docker/kind actually running — the live suite's `booted_sandbox` is
  deliberately bare) and the disk-watermark alarm actually tripping (needs
  a sandbox with a nearly-full disk).
- **Moving any real repo down the credential ladder, and applying branch
  protection, needs a real repo/org with admin access** — nothing to
  script here without one; procedure is above in
  ["Adding or reconfiguring a target repo"](#adding-or-reconfiguring-a-target-repo)
  for whenever one exists.
- **`grain github audit` has only been verified against `FakeTransport`**,
  not a real GitHub credential — this environment has neither a token file
  nor network access to `api.github.com`. Run it live the first time a real
  credential lands in `/data/secrets/github/`.
- **PR/comment creation once a sandbox finishes** doesn't exist yet
  (roadmap item 2) — a successful run currently just leaves a pushed
  branch; nothing opens a PR from it.
