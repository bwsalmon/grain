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
| `grain host up` | Creates the private bridge/network and applies the firewall policy | host |
| `grain host create/start/stop/destroy/recreate <name>` | VM lifecycle | host |
| `grain host status` | Lists VM state + assigned address | host |
| `grain host rules [--dry-run]` | Prints the firewall ruleset without applying it | host |
| `grain automation run-once` | Sweep stranded work, then poll GitHub and dispatch | controller |
| `grain automation status` | Show current sandbox↔issue assignments | controller |
| `grain github audit` | Check every credential under `secrets/github/` for withheld scopes | controller |
| `python3 -m grain.proxy.server` | Runs the git proxy (not wired into `grain` yet — its own entry point) | controller |

`--data-dir` (default `/data`), `--sandboxes` (default `2`), and
`--ssh-public-key` (default `/var/lib/grain/controller-ssh.pub`, **host**-
local — see step 5 below) are **global flags on `grain` itself, before the
subcommand group** — e.g. `grain --data-dir /data automation run-once`, not
`grain automation run-once --data-dir /data`.

## First-time setup checklist

Most of this is scripted (`provision/controller.sh`, `provision/sandbox.sh`);
what's left is genuinely one-time, per-deployment data — a GitHub
credential, a repo allowlist, and (once) copying a public key across the
host/controller boundary — that no script can responsibly bake in. See
`docs/design.md`, "Secrets on /data": **no secret is ever baked into an
image or a provisioning script.**

1. **Host baseline.** Debian, nested virtualization enabled, `/dev/kvm`
   present. Confirm with `ls /dev/kvm`.
2. **Bring the host network up**:
   ```sh
   sudo python3 -m grain.cli host rules          # read the policy first
   sudo python3 -m grain.cli host up
   ```
   This creates the `br-grain` bridge and applies the default-open-egress
   nftables policy. Idempotent — safe to re-run after any inventory change.
3. **Fetch a base image**, shared by the controller and every sandbox.
   `LibvirtAdapter.create()` passes `Cluster.image` straight to `qemu-img
   create -b <image>` as a backing file — it must be a **local path to a
   qcow2 image**, not a name Lima or libvirt resolves for you
   (`grain/adapter/libvirt.py`'s own docstring is explicit about this: "no
   automatic image download"). The live integration suites
   (`tests/test_vm_integration.py`, `tests/test_controller_integration.py`)
   fetch:
   ```sh
   sudo mkdir -p /var/lib/grain/images
   curl -fsSL -o /var/lib/grain/images/debian-12.qcow2 \
     https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
   ```
   **Known gap**: `Cluster.image` (`grain/inventory.py`) defaults to the
   bare string `"debian-12"`, and there is no `--image` flag on `grain
   host create`/`recreate` to point it at the path above. Today the only
   way to use a real image is to edit `Cluster.image`'s default in
   `grain/inventory.py`, or drive `LibvirtAdapter`/`Cluster` directly from
   a Python script the way the integration tests do
   (`Cluster(sandbox_count=..., image=str(base_image), ...)`). Worth fixing
   before this is used for real — flagged again in
   [Gaps](#gaps-what-this-runbook-cant-yet-tell-you-to-automate).
4. **Create the controller VM**, provisioned by `provision/controller.sh`
   (run this **on the host**):
   ```sh
   sudo python3 -m grain.cli host create controller --provision provision/controller.sh
   ```
   Use the `controller` target specifically, not `all` — `grain host
   create`/`recreate` now refuses `--provision` combined with `all`, since
   the controller and the sandboxes need different scripts. The script
   installs Python 3.11+, `gce_metadata_server`, the `grain-metadata`
   system user, the `/data/{secrets,config,state}` layout every module in
   `grain/automation`, `grain/proxy` and `grain/metadata` expects, the
   systemd units for the automation timer and the git proxy (installed but
   not enabled), and — new — **generates the controller's own SSH
   keypair**, `/data/secrets/controller-ssh{,.pub}`, idempotently, on the
   controller itself. It does not deploy this repo's own code, and it does
   not enable any service; both need real data (this repo's source, and
   the credentials below) that a provisioning script has no business
   holding. See the script's own comments, or `/etc/grain-tools/README` on
   the controller once it's booted.
5. **Copy the controller's SSH public key to the host.** This is the one
   step this repo genuinely cannot script end to end: the host (running
   `grain host create`) and the controller (running everything else) are
   different machines (`docs/design.md`, "One host machine runs
   everything"), so a file that's generated *on* the controller has to be
   carried across that gap by hand before `LibvirtAdapter` (which runs *on
   the host*) can embed it into a sandbox's cloud-init as an authorized
   key. It is a public key, not a secret, so this copy needs no special
   handling beyond getting the bytes across correctly:
   ```sh
   # From the host, once the controller has finished booting (its address
   # is Cluster().controller_ip, 10.100.0.2 by default):
   ssh debian@10.100.0.2 cat /data/secrets/controller-ssh.pub \
     | sudo tee /var/lib/grain/controller-ssh.pub > /dev/null
   ```
   (Substitute whatever key/method gives you SSH access to the freshly
   created controller — that's a separate, ordinary admin-access question,
   not something `provision/controller.sh` sets up.) `/var/lib/grain/
   controller-ssh.pub` is `LibvirtAdapter`'s default `--ssh-public-key`
   path — override it with that flag if you'd rather keep the copy
   elsewhere. A sandbox created *before* this file exists on the host gets
   no authorized key at all (`render_meta_data` treats a missing file as
   "no key to inject" rather than erroring), so do this before the next
   step.
6. **Create the sandbox VMs**, provisioned by `provision/sandbox.sh` (also
   on the host):
   ```sh
   sudo python3 -m grain.cli host create sandboxes --provision provision/sandbox.sh
   sudo python3 -m grain.cli host status
   ```
7. **Deploy this repo's code to the controller**, at `/opt/grain` (created,
   empty, by `provision/controller.sh`). `grain` has no third-party
   dependencies (`pyproject.toml` — stdlib only), so this is the only thing
   missing before `python3 -m grain.cli` works there:
   ```sh
   ssh debian@10.100.0.2 sudo git clone <your-remote> /opt/grain
   ```
   `provision/controller.sh` deliberately doesn't do this itself — it would
   need a deploy credential baked into a provisioning script, which is
   exactly what "no secret is ever baked into an image or a provisioning
   script" rules out. Use your own credentials, the same way you'd deploy
   any other private repo.
8. **GitHub credential files**, under `/data/secrets/github/`:
   ```
   /data/secrets/github/
     credentials.json     # {"owner/repo": "name", "owner/*": "name", "*": "name"}
     bot.token             # e.g. the machine-account PAT
     personal.token         # last resort
   ```
   Every value in `credentials.json` other than the literal string
   `"anonymous"` must have a matching `<name>.token` file next to it
   (`grain/proxy/credentials.py`) — a `0600` file holding the raw token,
   trailing whitespace is stripped. `chmod 0600` every token file. **Do not
   grant `workflow`, `delete_repo`, `write:org`, or any `admin:*` scope to
   any of these** — see [Credential audit](#credential-audit) below.
9. **The repo allowlist**, `/data/config/repo-allowlist.json` — a plain
   JSON array of `"owner/repo"` strings, default-deny, hot-reloaded
   (`grain/proxy/allowlist.py` re-reads it on every request, no restart
   needed). A repo must be on this list *and* covered by a
   `credentials.json` pattern before the proxy will forward anything for
   it.
10. **Sandbox tokens**, `/data/secrets/sandbox-tokens.json` — maps sandbox
    name to its bearer token:
    ```json
    {"sandbox-0": "<random token>", "sandbox-1": "<random token>"}
    ```
    Generate with e.g. `python3 -c "import secrets; print(secrets.token_hex(32))"`
    per sandbox. This is what the sandbox's git credential helper presents to
    the proxy as the HTTP Basic password (`grain/proxy/tokens.py`) — nothing
    provisions or injects this automatically today; wiring it into the
    sandbox's cloud-init user-data (so a fresh sandbox's git credential
    helper is actually configured to use it) is also still manual.
11. **`automation.json`**, `/data/config/automation.json` — the only two
    fields with no default (`AutomationConfig`, `grain/automation/config.py`):
    ```json
    {"owner": "your-org", "repo": "your-repo"}
    ```
    Everything else has a default worth knowing: `trigger_label:
    "grain-agent"`, `in_progress_label: "grain-agent-in-progress"`,
    `ssh_user: "debian"`, `ssh_key_path: "/data/secrets/controller-ssh"`,
    `runs_per_hour: 10`, `max_runtime_minutes: 120`. Override any of them by
    including the key in this file.
12. **Enable the git proxy and the automation timer**, now that `/opt/grain`
    holds real code and `/data` holds real credentials (SSH to the
    controller for this — `provision/controller.sh` installed these units
    but deliberately left them disabled):
    ```sh
    sudo systemctl enable --now grain-git-proxy.service
    sudo systemctl enable --now grain-automation.timer
    ```
13. **Claude Code login in each sandbox.** Per `docs/design.md`'s
    "Interim choice" section, this is a manual, per-sandbox step — SSH in as
    `debian` and run whatever `claude` login flow is current. There is no
    automation for this and none is planned until the controller-side LLM
    proxy design lands.
14. **Verify before trusting it**: `grain --data-dir /data automation
    status` should list every configured sandbox as `free`, and `grain
    --data-dir /data github audit` should print one line per credential
    file with no `flagged` verdicts (see below).

## Running automation

`grain automation run-once` does one pass: sweep stranded/finished work,
poll `owner/repo` for open issues carrying `trigger_label`, and dispatch to
any free sandbox within the rate limit. It is meant to be **invoked
periodically by something else, not run as a daemon** — `docs/design.md`
says "invoked by a systemd timer."

**That timer is now shipped**: `provision/controller.sh` installs
`grain-automation.service`/`.timer` (a two-minute `OnUnitActiveSec`, same
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

## Credential file layout and rotation

```
/data/
  secrets/
    controller-ssh, controller-ssh.pub   # controller -> sandbox SSH identity
    sandbox-tokens.json                  # sandbox name -> bearer token (git proxy auth)
    github/
      credentials.json                   # owner/repo pattern -> credential name
      <name>.token                       # one file per credential named in credentials.json
  config/
    repo-allowlist.json                  # ["owner/repo", ...], default-deny
    automation.json                      # AutomationConfig
  state/
    automation/state.json, audit.log
    git-proxy/audit.log
```

Rotation is uniformly **"replace the file, restart the one service that
reads it"** (`docs/design.md`, "Operations") — nothing here watches
`secrets/` for changes the way the allowlist watches `config/`:

- **A GitHub credential** (`bot.token`, `personal.token`, ...): overwrite
  the file with the new token, `chmod 0600` it, restart the git proxy
  (`python3 -m grain.proxy.server`) and, if `automation.json`'s `owner`/
  `repo` resolve to that credential, the process invoking `automation
  run-once` too — `CredentialSet` and `build_orchestrator` both load once at
  construction (`grain/proxy/credentials.py`'s own docstring is explicit
  about this), so a running process keeps using the old token until
  restarted.
- **A sandbox token**: generate a new one, update its entry in
  `sandbox-tokens.json`, restart the git proxy. **The old token stays valid
  everywhere else that reads the same file until the proxy restarts** —
  there's no in-place invalidation. Note also that `HostAdapter.recreate()`
  (`grain/adapter/base.py`) does **not** touch `sandbox-tokens.json` at
  all, despite `docs/design.md` describing rotation as "folded into
  recreate" — today that's aspirational, not implemented. Recreating a
  sandbox does *not* rotate its token; do that as a separate, manual step.
- **The controller SSH key**: generated once, on the controller, by
  `provision/controller.sh` (idempotently — it will not touch an existing
  `/data/secrets/controller-ssh`). To rotate it: delete both files on the
  controller, re-run the keygen line from `provision/controller.sh` by
  hand (or re-provision the controller, which loses everything else under
  `/data` too unless it's on a separate persistent disk — see
  `docs/design.md`, "Secrets on `/data`"), redo step 5 of first-time setup
  (copy the new `.pub` to the host's `--ssh-public-key` path), then
  re-run `grain host create`/`recreate` for every sandbox — the public key
  is baked into each sandbox's cloud-init seed at creation time, not
  re-read later, so this is a full sandbox recreation cycle, not a
  hot-swap.

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
(`grain/automation/sweeper.py`), and handles three cases **automatically**,
with no operator action:

- The dispatched unit finished successfully → label moved back off
  in-progress, sandbox freed.
- The unit finished with a failure → issue re-labelled with the trigger
  label (put back in the queue), sandbox freed.
- The unit is missing entirely (never started, or the sandbox was recreated
  out from under it) or has run past `max_runtime_minutes` → treated as
  stranded, issue re-labelled, sandbox freed.

What is **not automatic**:

- **A wedged sandbox that still reports `ACTIVE`** — e.g. `claude -p` is
  hung but hasn't exceeded `max_runtime_minutes` yet. The sweeper leaves it
  alone by design (it can't distinguish "slow" from "stuck"). If you
  suspect this, check with `grain automation status` plus `ssh -i
  /data/secrets/controller-ssh debian@<address> systemctl status
  grain-task-<sandbox>`, and either wait it out or manually stop the unit
  (`sudo systemctl stop grain-task-<sandbox>` on the sandbox) and re-run
  `automation run-once` to let the sweep pick it up as stranded.
- **Between-task hygiene** (`kind delete clusters --all`, `docker system
  prune -af --volumes`, clearing the work directory) — `docs/design.md`
  describes this but no hook runs it automatically yet
  (`docs/roadmap.md` item 5). Run it by hand over SSH between tasks on a
  sandbox you're reusing, or just recreate the sandbox.
- **Disk-watermark and health checks** — also not built yet (same roadmap
  item). Watch disk manually for now: `ssh ... df -h /`.
- **Base-image updates** — recreate is the deploy path
  (`grain host recreate <name> --provision provision/sandbox.sh`), but
  nothing schedules it; `docs/design.md` suggests weekly.

## Adding or reconfiguring a target repo

1. Add `"owner/repo"` to `/data/config/repo-allowlist.json` (hot-reloaded —
   no restart).
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

## Gaps: what this runbook can't yet tell you to automate

Everything below needs either a real target GitHub repo/org, or code this
repo doesn't have yet. Tracked in `docs/roadmap.md`:

- **This repo's own code still has to be deployed to the controller by
  hand** (`/opt/grain`, first-time setup step 7) — `provision/controller.sh`
  deliberately doesn't do this itself, since it would need a deploy
  credential baked into a provisioning script (roadmap item 3, closed as
  "as scripted as it responsibly gets").
- **The controller's public SSH key still has to cross the host/controller
  boundary by hand** (first-time setup step 5) — this is inherent to the
  host and controller being different machines, not something a script on
  either side alone can close.
- **No `--image` flag** on `grain host create`/`recreate` — `Cluster.image`
  has to be edited in source or set by driving the adapter from a script
  directly (see step 3 of first-time setup above). Small, but blocks a
  clean "just run this command" story.
- **`recreate()` does not rotate the sandbox token** despite the design
  describing rotation as folded into it — do it as a separate manual step
  (see [Credential audit](#credential-audit) / rotation above).
- **No between-task cleanup hook, health check, or disk-watermark alarm**
  (roadmap item 5).
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
