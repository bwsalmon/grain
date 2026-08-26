# Roadmap: what's left after the automation orchestrator

Status snapshot as of the `grain/automation/` orchestrator landing (commit
`c2276eb`). Cross-checked against `docs/design.md`'s implementation plan —
steps 1–4 (host baseline, host adapter, networking, sandbox image) and step
6 (git proxy) are done; this file tracks what step 5, 7, 8, 9, and 10 still
need, plus one documentation gap the orchestrator's arrival exposed.

Each item below is meant to be handed to one agent at a time, in order.
Check an item off by editing this file in the same commit that closes it
out.

## 1. Update `docs/design.md`'s stale open question

- [x] Done

Open question 6 ("Which agent runtime: OpenHands, or Claude Code?") still
reads "not decided... nothing built so far depends on the answer." That's
no longer true — `grain/automation/` was built against the Claude Code
answer. Update the doc to record the decision, why (cut the OpenHands
dependency footprint — Agent Canvas, `openhands-agent-server`, and the
Automation Service's version-pin matrix, traded for a dispatch loop this
repo owns), and point at `grain/automation/` as where it lives. Also worth
a pass: the "Alternative agent runtime: Claude Code" section was written
speculatively before any of this was verified live — reconcile its claims
(the `--uid=debian` / `sudo` / `RemainAfterExit` / `shlex.join` /
`IdentityAgent=none` details `grain/automation/ssh.py` and `dispatch.py`
now document from live testing) with what's actually true.

## 2. PR creation after a sandbox pushes

- [x] Done

The branch/commit question resolved as: deterministic, not self-reported.
`dispatch.py`'s `branch_name(issue)` (`grain/issue-<N>`) is computed by both
`dispatch()` (to tell the agent exactly what to push to, in the prompt) and
`core.py` (to verify via `GitHubClient.branch_exists` before opening a PR)
— never trusted from the agent's own report, since the prompt it received
came from untrusted issue content. A succeeded run whose branch doesn't
exist is requeued through the same path as a failed/stranded one, audited
as `"succeeded but branch ... does not exist"` rather than silently treated
as done. `GitHubClient` gained `branch_exists`/`create_pull_request`
(`github.py`), following the existing `Transport`/`FakeTransport` pattern.

This also closed the two gaps item 2 depended on and nothing else provided:
`dispatch()` now clones (or fetches-and-resets, for a reused long-lived
sandbox) a fixed workspace path through the git proxy before starting
`claude -p` (`ensure_workspace`), and sandbox git-proxy tokens are now
minted and distributed — `grain/proxy/tokens.py` gained `SandboxTokenStore`
(the write side of the file `SandboxTokens` already read), and
`configure_git_credentials` points the sandbox's git credential helper at
its token over the same stdin-not-argv channel the prompt already uses, so
neither the token nor the clone URL ever needs to carry it. Verified live
against a real sandbox VM: `tests/test_vm_integration.py` now stands up a
real bare git repo, served over real smart-HTTP by `git http-backend`,
behind a real `GitProxy` (only its `Forwarder` swapped from `RealForwarder`'s
hardcoded HTTPS for a plain-HTTP equivalent, to avoid provisioning a TLS
cert for a throwaway test) — the sandbox clones through it, a wrong token is
rejected, and a second `ensure_workspace` call on an already-cloned
workspace picks up a new upstream commit and discards a simulated leftover
file. Found live: the bare cloud image this suite boots (deliberately
unprovisioned, to keep boot time down) has no `git` binary at all —
`provision/sandbox.sh` installs it in a real deployment, but nothing here
had needed it until this item's workspace-clone step; fixed with a
session-scoped `git_installed` fixture. What's still not exercised: a real
GitHub repo/credential (item 8) and full sandbox provisioning (item 3).

## 3. A provisioned controller VM

- [x] Done

`provision/controller.sh` now exists, mirroring `provision/sandbox.sh`'s
shape (a raw shebang script, delivered as NoCloud cloud-init user-data).
Verified live: `tests/test_controller_integration.py` boots a real
controller VM via `LibvirtAdapter` with this script as its user-data and
checks the guest, not just the script's syntax — Python 3.11+ present,
`/data/{secrets,config,state}` created with the layout
`grain/automation`/`grain/proxy`/`grain/metadata` already expect, the
`grain-metadata` system user exists, `gce_metadata_server` is installed and
runs, `/opt/grain` exists, and the `grain-automation`/`grain-git-proxy`
systemd units are installed but correctly *not* enabled yet.
`tests/test_provision_controller.py` covers the script's content and
`bash -n` syntax without paying the boot cost, and pins its paths against
the real Python defaults (`AutomationConfig.ssh_key_path`,
`MetadataConfig.key_path`/`metadata_user`) so they can't silently drift.

**The controller SSH keypair generation step is scripted**, resolving the
open design question this item flagged: generated *on the controller*, by
`provision/controller.sh`, idempotently, at
`/data/secrets/controller-ssh{,.pub}`. This is a real host/controller data-
flow question, not a detail — the host (running `LibvirtAdapter`, which
embeds the *public* half into a new VM's cloud-init as an authorized key)
and the controller (which now generates the keypair and holds the private
half for `dispatch()`/`SshRunner`) are different machines
(`docs/design.md`, "One host machine runs everything"). No script running
on either side alone can close that gap — carrying the public key across
it is now the one genuinely irreducible manual step, documented as step 5
of `docs/runbook.md`'s first-time setup checklist. `LibvirtAdapter`'s
`ssh_public_key_path` default moved accordingly, from
`/data/secrets/controller-ssh.pub` (which only ever worked because this
dev environment collapses host and controller into one machine) to
`/var/lib/grain/controller-ssh.pub` — host-local, matching `config_dir`'s
existing convention — with a new `--ssh-public-key` CLI flag to point it
at wherever the copy lands.

**CLI wiring**: `grain host create controller --provision
provision/controller.sh` already worked mechanically (`_targets` resolves
`controller` to just that one VM), so no new subcommand was needed — but
`grain host create all --provision <script>` would have silently applied
one script to both roles, which is wrong now that they differ. `create`/
`recreate` reject that combination with a message pointing at running
`controller` and `sandboxes` separately.

**Deliberately still manual**, and documented as such rather than
papered over: deploying this repo's own code to the controller's
`/opt/grain` (no third-party dependencies to install — `pyproject.toml` is
stdlib-only — but the source itself needs a deploy credential this
project's own "no secret is ever baked into an image or a provisioning
script" invariant rules out putting in `provision/controller.sh`), and
enabling the `grain-automation`/`grain-git-proxy` systemd units once real
credentials exist under `/data`. Both are steps 7 and 12 of
`docs/runbook.md`'s first-time setup checklist now, not undocumented
assumptions.

## 4. GCP metadata server

- [x] Done, later superseded — bwsalmon/agents#126 removed this whole
  broker in favour of a real, short-lived service-account key minted per
  dispatch. Left unedited below as the historical record of what was
  actually built and verified at the time; see `docs/design.md`'s "GCP
  credentials" section, "Superseded" subsection, for the current design.

`docs/design.md` step 7: one `gce_metadata_server` instance per sandbox,
serving impersonated tokens for a narrow second service account.
`inventory.py` and `net_linux.py` already reserve the ports and DNAT rules
for this (`Cluster.metadata_port`, the anycast DNAT in
`render_ruleset`); nothing serves them yet.

`grain/metadata/` (`config.py`, `audit.py`, `launcher.py`) plus a `grain
metadata start|stop|status|sync-audit` CLI group. Verified live against a
real `gce_metadata_server` v4.2.5 binary (installed, not built from
source): `-interface`/`-port` bind exactly the address given (no
`0.0.0.0` fallback — confirmed a request to a different local address
fails to connect); `--impersonate` reads the *source* credential from
`GOOGLE_APPLICATION_CREDENTIALS`, never `-serviceAccountFile`, and its own
startup log line confirms the *target* principal is the config file's
`serviceAccounts.default.email`, not the source key's own identity —
exactly the "impersonate a second account, don't serve the primary key's
tokens" shape. `-logTarget` is a real file path (its own `cmd/main.go`),
so each instance logs JSON per request there and `metadata/audit.py`'s
`sync` tails it into a `FileAuditLog` (same shape as `proxy/audit.py` and
`automation/audit.py`) — the "every mint is audit-logged" line. The
parser was corrected against a captured real trace: the outcome-bearing
`ERROR` line is not the line immediately after a `msg="request"` line (an
intermediate `"using service account context"` line sits between them),
which unit tests now pin down using that real trace rather than an
assumed shape.

Also verified live, at the routing layer, the claim that a sandbox needs
"no wrapper script, no environment plumbing": with only a default route
present (matching a sandbox's cloud-init `gateway4`, already what
`render_network_config` sets — no change needed there), a forwarded
packet to `169.254.169.254` resolves via that default route without any
sandbox-specific host route, confirmed with `ip route get ... iif
br-grain` against a scratch routing table. Combined with `net_linux.py`'s
existing DNAT rule, that's the whole path from "ADC's default probe" to
"the right per-sandbox instance" with nothing for `provision/sandbox.sh`
to add.

Not verified, and not verifiable without a real GCP project: an actual
token mint succeeding end to end (impersonation calls
`iamcredentials.googleapis.com`, which needs a real primary key with real
IAM bindings to a real narrow service account) — exercised locally only
with a syntactically-valid but fake key, which fails at the expected
point (a parse error on the key, logged and forwarded to the audit log
exactly as a real permission failure would be). The `--uid=grain-metadata`
system user and `provision/controller.sh` itself now exist (item 3) and
`tests/test_controller_integration.py` confirms the user and the
`gce_metadata_server` binary land on a real controller VM — what's still
not exercised, for the same "no real GCP project" reason as the token mint
above, is `MetadataLauncher.start()` actually running against that real
user/binary pair on a real controller.

## 5. Lifecycle scripts

- [x] Done

`grain host recreate` already covered destroy-then-rebuild. This item was
the three pieces `docs/design.md` step 8 still named: a between-task cleanup
hook, a health check, and a disk-watermark alarm.

**Where the cleanup hook runs, and why:** automatically, from
`grain/automation/sweeper.py`, the instant a sandbox's slot is freed —
success, failure, or stranded — rather than as a separate cron job or an
operator-run script. That's the shape this codebase already has: the
sweeper is where every other "a run just finished, now what" decision
already lives (label moves, and, since item 2, PR creation), and folding
cleanup into the same pass means a freed sandbox is *guaranteed* clean
before its next dispatch, not "clean once some other job gets around to
it." `grain host cleanup [name]` (`grain/automation/cleanup.py`, wired into
`grain/cli.py`) is also exposed standalone, for a sandbox an operator wants
tidied without waiting on a sweep cycle. One deliberate departure from the
`docs/design.md` snippet: cleanup does **not** `rm -rf` the workspace —
that snippet predates `dispatch.py`'s `ensure_workspace()` (item 2), which
already resets the workspace to a known-clean state on every dispatch;
wiping it here would only force a full re-clone next time instead of an
incremental fetch, with no correctness benefit. See `cleanup.py`'s
docstring for the full reasoning.

**Health check**: `grain/automation/health.py`'s `check_health()` — SSH
reachability (probed first; everything else is skipped and the whole report
is `unreachable` if this fails), `systemctl is-system-running` (flags
`degraded`, not just a hung system), `docker info` actually responding, and
disk usage against a watermark (`df -P /`, default 85%) — that last check
*is* the disk-watermark alarm named separately in step 8, not a fourth
thing; it folds in naturally rather than standing alone. Surfaced as `grain
host health [name]`, non-zero exit if not fully healthy, matching `grain
github audit`'s exit-code convention.

**Does the sweeper call it too?** Yes — every sweeper release also runs a
post-cleanup health check, closing the exact gap `docs/design.md` step 8
flagged: the sweeper previously only reacted to a dispatched *unit's* state,
never to the sandbox's general health, so a sandbox libvirt/systemd reports
as fine but is actually degraded (disk full, docker wedged) was invisible
between recreates. The result is **visibility, not gating**: an unhealthy
reading becomes a `"health warning: ..."` line in the same audit log every
other dispatch/sweep decision already goes to (`core.py`'s `_sweep`), but it
does not remove the sandbox from the dispatch pool. Quarantining an
unhealthy sandbox out of rotation would be a real change to
`AutomationState`'s pool-assignment shape — treated as a deliberate
boundary for this item, not an oversight; see `sweeper.py`'s docstring.

Unit-tested throughout against `FakeRunner`
(`tests/test_automation_cleanup.py`, `tests/test_automation_health.py`, plus
new cases in `tests/test_automation_sweeper.py`, `tests/test_automation_core.py`,
and `tests/test_cli.py`). Also verified live against a real sandbox VM
(`tests/test_vm_integration.py`): `check_disk`'s `df -P` parser and
`check_systemd`'s `is-system-running` reading both hold up against a real
guest's actual output (not just a hand-written fixture), and — since
`booted_sandbox` is deliberately unprovisioned, no docker or kind installed
— `cleanup()` and `check_health()` against a sandbox missing both binaries
came back exactly as designed: a graceful per-step failure with a real
"command not found" from the remote shell, never a hang or a crash. Not
verified live: the fully-healthy path (would need a provisioned sandbox with
docker/kind actually running) and the disk-watermark alarm actually tripping
(would need a sandbox with a nearly-full disk) — both are covered only by
unit tests with scripted `df`/`docker`/`systemctl` output.

## 6. Load-test harness for open question 2

- [x] Done

Design doc open question 2: does 4 vCPU hold two sandboxes plus a
controller under real `kind` + build load? One sandbox at rest was
measured (`docs/design.md`'s implementation-plan step 4 notes). Build a
small script that brings up two sandboxes, drives concurrent `kind`
cluster creation and a build in each, and records CPU/memory pressure —
then actually run it and record the numbers here or in `docs/design.md`.

`tests/loadtest.py` — booted the controller plus two fully-provisioned
sandboxes (`provision/sandbox.sh`/`provision/controller.sh`, via the same
`LibvirtAdapter`/`Cluster` machinery every live test in this repo uses, not
a parallel VM-boot mechanism), then drove real concurrent load in both
sandboxes: `kind create cluster` plus a real `./configure && make
-j$(nproc)` build of CPython's own source tree (chosen over cloning a real
repo like redis live — redis's own deps are git submodules a GitHub tag
tarball doesn't include, which would fail on missing headers rather than
exercise CPU; CPython's tarball has no submodules and is large enough to be
a genuine multi-minute, multi-core compile, not a synthetic stress tool and
not trivial). Host-side qemu process and overall load/memory were sampled
throughout and rendered into a report a human can read a verdict off, not
just raw numbers — `evaluate()`'s two explicit thresholds (peak 1-minute
load vs. physical vCPU count, minimum available memory vs. a floor).

**Actually run**, not just built: this dev/test host is 4 vCPU / 32106 MiB
— close enough to the n2-highmem-4 design target (4 vCPU / 32768 MiB) that
the numbers below are directly informative for it, not a differently-shaped
stand-in. Two sandboxes + controller, 255s of concurrent `kind create
cluster` + CPython build in both sandboxes (all four tasks finished `ok`):
1-minute host load average ranged 2.16–4.19 (peak briefly over the 4
physical vCPU — real but mild contention, short of clear overload);
available memory never dropped below 17922 of 32106 MiB total. Per VM, host
side: each sandbox averaged ~122% CPU of its 2-vCPU allocation (peak
~150%) and ~5.6 GiB RSS — well past the single-idle-sandbox baseline
(~29% CPU / ~5 GB RSS) this needed to beat; the controller, idle throughout,
averaged 13.7% CPU / ~1.05 GiB RSS. Verdict: **holds**, confirming CPU
(not memory) is the resource actually under pressure, as open question 2
already suspected — though note the allocated total already overcommits
vCPU 5-to-4 on this host before any VM does anything, so this is headroom
under an overcommitted allocation, not evidence the allocation is
conservative. Numbers also recorded in `docs/design.md`, open question 2
and implementation-plan step 4. VMs cleaned up on exit — `booted_vms()`
destroys everything it created even on failure, verified both live and by
`tests/test_loadtest.py`'s `FakeAdapter`-based cleanup tests.

## 7. Hardening

- [ ] Done — the buildable half is; the rest needs a real repo

`docs/design.md` step 10: move repos down the credential ladder (App →
fine-grained PAT → machine-account PAT → personal token, per repo), apply
branch protection on target repos, confirm no credential in
`/data/secrets/github/` carries `workflow` scope, and write the operator
runbook the rest of this work has been assuming exists.

**Built and unit-tested**, needing no real credential or target repo:

- `grain github audit` (`grain/automation/credential_audit.py`, wired into
  `grain/cli.py`) — checks every `*.token` file under `secrets/github/`
  against the scopes `docs/design.md`'s "Scopes to withhold" names
  (`workflow`, `delete_repo`, `write:org`, `admin:*`). Classic PATs and
  OAuth tokens are checked for real, via the `X-OAuth-Scopes` response
  header GitHub returns on any authenticated call (confirmed against
  GitHub's docs, not assumed). Fine-grained PATs and GitHub App tokens have
  no such header — confirmed via GitHub's own maintainers that no API
  exposes a fine-grained PAT's permissions at all — so the tool reports
  `unverifiable` for those rather than faking a pass. Unit-tested against
  `FakeTransport` scripted with GitHub's real response shapes
  (`tests/test_automation_credential_audit.py`); never run against a real
  token, since none exists in this environment.
- `docs/runbook.md` — the operator runbook: first-time setup, host/VM
  lifecycle, the automation loop and its (currently unshipped) systemd
  timer, the full `/data/secrets` layout and rotation procedure for each
  credential, what the sweeper handles automatically versus what needs a
  human, and the procedure for adding a repo (allowlist, credential,
  branch protection, `github audit`).

**Still blocked on real infrastructure — not attempted here:**

- Actually moving any real repo down the credential ladder — needs a real
  repo/org to hold credentials for.
- Actually applying branch protection / a ruleset on a target repo — needs
  repo admin access on something real; the procedure is written up in
  `docs/runbook.md`, "Adding or reconfiguring a target repo," step 3.
- Actually running `grain github audit` against a real credential and
  confirming no `flagged` result — needs a token in
  `/data/secrets/github/` and network access to `api.github.com`, neither
  of which this environment has.

## 8. First live issue-to-PR run

- [x] Done — the whole mechanical pipeline; not against real GitHub or a
  real agent

Verification, not implementation — flagged separately because a *fully*
real run needs things no agent can provide on its own: a real target GitHub
repo, a real credential in `/data/secrets/github/`, and a sandbox with
Claude Code actually logged in (still a manual step per `docs/design.md`).
None of those exist in this environment, so — following the same precedent
item 2 set with `git_proxy_target` rather than skipping live verification
outright — this substitutes exactly two things, both because the real
things genuinely don't exist here, and runs the real `Orchestrator.run_once`
(`grain/automation/core.py`) against everything else real:

- **A realistic mock of the GitHub REST API** (`RealGitHubMock`,
  `tests/test_live_issue_to_pr.py`), wired into the real `GitHubClient`
  through its own `Transport` protocol — the identical seam
  `github.py`'s own `FakeTransport` uses for unit tests, so the code under
  test is the real `GitHubClient` (path building, pagination, status
  handling, field extraction) and the real `Orchestrator`/`sweeper.py`
  decision logic, not a reimplementation of either. Implements exactly the
  endpoints `GitHubClient` calls: `list_issues`/`list_pull_requests` (the
  shared `/issues` listing, seeded with one fake issue carrying the
  trigger label), `add_label`, `remove_label`, `create_pull_request`, and
  `branch_exists` — the one endpoint a canned answer would make dishonest,
  so it runs a real `git show-ref` against the same real bare repo the
  sandbox clones from and pushes to.
- **A fake `claude` binary** at `/usr/local/bin/claude` on the sandbox's
  `PATH`, standing in for a real login. Reads the real prompt `dispatch()`
  piped to its stdin, parses the branch out of the literal `git push
  origin HEAD:<branch>` instruction the way a real agent reads its own
  instructions, and either makes a real commit and pushes it, or
  deliberately doesn't — three scripted variants exercise the happy path
  and both failure shapes. `dispatch.py`/`core.py` are not touched or
  special-cased; only the sandbox-side binary is fake.

Everything else is the real thing: a real sandbox VM (booted the same way
item 2's live suite boots one), the real bare-repo-behind-a-real-`GitProxy`
rig item 2 built, the real `SshRunner`/systemd-unit dispatch mechanism, the
real `AutomationState` pool bookkeeping, the real sweeper's cleanup/health
hooks. Three scenarios ran end to end against it: (1) issue discovered,
dispatched, sandbox clones through the real proxy, fake agent pushes a real
commit to the real deterministic branch, sweep verifies the branch via a
real git query and opens a PR — recorded by the mock with the right
head/base/title; (2) the fake agent exits nonzero without touching the
repo — requeued (trigger label restored, no PR), not treated as done; (3)
the fake agent exits zero but never pushes — also requeued, via the exact
`"succeeded but branch ... does not exist"` path `core.py`'s docstring
describes, which is the one case a naive "unit exited 0 means success"
sweep would get wrong and this environment can now actually exercise.

Two things found live while building this, worth recording the way every
other "verify live" item in this file does:

- **`git http-backend` denies push (`git-receive-pack`) by default**, even
  with `GIT_HTTP_EXPORT_ALL=1` — that variable only covers the read side.
  The fake agent's first push came back a real `403`, forwarded straight
  through the proxy from upstream, until the test's bare repo set
  `http.receivepack true` explicitly. Item 2 never hit this because nothing
  in its suite pushes; this is what "needs a real writable allow-listed
  repo to test against, not just a public read" (item 2's own note) turned
  out to mean in practice.
- **This host is shared across concurrent agent sessions**, and
  `test_vm_integration.py`'s fixtures hardcode global resources with no
  per-session isolation: a fixed sandbox name/address (`sandbox-0`/
  `10.100.0.10`) and a fixed `Cluster.controller_ip` (`10.100.0.2`). Two
  collisions happened live — `virsh define`: `domain 'sandbox-0' already
  exists`, and later a real concurrently-booted controller VM claiming
  `10.100.0.2` for real, breaking an earlier version of this suite's
  approach of aliasing that same address on the host bridge. Fixed by
  giving this suite its own sandbox at a different index (`sandbox-1` —
  not arbitrary: `Cluster()`'s own default is `sandbox_count=2`, and the
  host's already-applied firewall ruleset was confirmed live to already
  cover `gr-sb1` too) and by pointing `Orchestrator`'s `cluster.
  controller_ip` at the host's own bridge address (`10.100.0.1`, the same
  address item 2's `git_proxy_target` binds to, which no VM is ever
  assigned) via a small duck-typed stand-in — `Orchestrator`'s own code
  reads `cluster.controller_ip`/`cluster.sandbox_names`/`cluster.
  address_of()` and nothing else, so this exercises the real code
  unmodified while giving it a collision-free resource to report.

**What this proves**: the entire mechanical pipeline, for real — issue
discovery, dispatch, a real clone through a real proxy, a real pushed
commit, sweep-time success detection against real git state (not a mock's
say-so), and PR creation with the right head/base/title, plus both
requeue paths.

**What this does not prove, and only a genuinely live run closes**: nothing
about real GitHub's actual API behavior or quirks — rate limits, exact
error-response shapes, auth edge cases the mock was never asked to get
wrong — and nothing about a real Claude Code agent's actual behavior. A
scripted stand-in structurally cannot fail the way a real agent might: push
somewhere unexpected, hang, misread its instructions, or behave in some
way nobody scripted for. That gap is real, and closing it needs the three
things listed at the top of this item — a real repo, a real credential, a
real login — none of which exist in this environment.

**Update: two of the three now exist, and closing that gap surfaced three
more, real, bugs.** A `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`)
became available, and this dev host already has `/dev/kvm`. Rather than a
real GitHub repo, `--github-host`/`--git-forward-host`/`--github-insecure-
http` (new flags on `grain controller configure`/`host bootstrap`, threading
into `AutomationConfig.github_host`/`git_forward_host`/`github_use_tls` and
`RealTransport`/`RealForwarder`'s existing `host`/`use_tls` constructor
args) point the *real, deployed* `grain-automation.timer` — not this item's
own in-process `Orchestrator` harness — at a small standalone mock server
combining `RealGitHubMock`-shaped REST endpoints with a `git http-backend`
CGI, run on the host's own bridge address. `CLAUDE_CODE_OAUTH_TOKEN` was
injected via `sudo systemctl set-environment` on each sandbox rather than
`--claude-credentials-file`'s `~/.claude/.credentials.json` shape, since a
`setup-token` value is exactly the env-var form and sidesteps the
concurrent-refresh open question entirely. Two real `grain host bootstrap`
runs later (the first collided with and lost its VMs to an unrelated
concurrent live-suite run on this shared host — a fresh bootstrap avoided
it), a real sandbox dispatched a real `claude -p` against the mock:

- **Fixed: `claude` wasn't on `PATH` for a dispatched task.** The installer
  places it at `~/.local/bin/claude`; `dispatch.py`'s `systemd-run
  --uid=debian` is a non-login shell that never sources `~/.profile`, the
  only thing that adds that directory to `PATH`. Every prior live suite used
  a fake `claude` already sitting on `/usr/local/bin` (on `systemd`'s
  default `PATH`), so this never surfaced before a real login existed to
  test with. Fixed in `provision/sandbox.sh`: `ln -sf ~/.local/bin/claude
  /usr/local/bin/claude`.
- **Fixed: a sandbox's first-ever dispatch always failed git-proxy auth.**
  `grain-git-proxy.service` loads `sandbox-tokens.json` once at start
  (`SandboxTokens`, never re-read); a token is normally minted lazily, on
  first dispatch (`SandboxTokenStore.ensure_token`, called from
  `dispatch.py`). Bootstrap's stage 10 enables the proxy before any
  dispatch has ever happened, so on a fresh deployment the proxy always
  starts with an empty token file, and the very first real dispatch to any
  sandbox 401s against it — `git clone` came back `fatal: Authentication
  failed`, cryptic against the actual cause. Fixed: a new
  `ensure_sandbox_tokens` (`grain/automation/configure.py`) mints every
  sandbox's token before stage 10, so the proxy's first-ever load already
  has the complete set. (`tests/test_live_issue_to_pr.py`'s own fixture
  independently works around the identical ordering hazard by pre-minting
  before constructing its in-process `GitProxy` — this generalizes that
  into the real bootstrap sequencer.)
- **Open, not fixed: a real agent still cannot push.** `git commit` succeeds
  (the sandbox's own `sandbox.autoAllowBashIfSandboxed`, default on, covers
  it), but `git push origin HEAD:<branch>` — the exact instruction
  `dispatch.py`'s prompt gives the agent — needs a sandbox network-domain
  approval no `-p` run with no `--permission-prompt-tool` can ever answer.
  `sandbox.network.allowedDomains` is upstream's documented way to pre-
  allow a host with no prompt; `["10.100.0.2"]` (the git proxy's address),
  `["10.100.0.2", "10.100.0.2:8080"]`, and even `["*"]` were each tried
  live in `~/.claude/settings.json` and every one still hit "needs your
  explicit approval ... network access to `10.100.0.2:8080`." Switching
  `dispatch.py` from `--permission-mode acceptEdits` to
  `--dangerously-skip-permissions` — upstream's documented answer for
  "fully unattended inside a container, VM, or the sandbox runtime," which
  a grain sandbox already is — made things *worse* live: under it,
  `autoAllowBashIfSandboxed` stopped applying and `git add`/`commit`
  started needing approval too, which plain `acceptEdits` never blocked.
  Reverted to `acceptEdits` (the empirically-furthest-getting mode) with
  the push gate still open; `docs/design.md`'s "Interim choice" section
  records the same finding. Every unit exits zero without pushing, so the
  sweeper's own "succeeded but branch does not exist" requeue path
  (exercised by this item's mock-harness scenario 3) is what actually
  fires — correct behavior, just not the outcome this run was after. Next
  things worth trying, not yet attempted: a `--permission-prompt-tool`
  script that auto-answers exactly this prompt, or `--permission-mode
  dontAsk` with an explicit `permissions.allow` covering `Bash(git *)`
  (`docs/design.md` already named `dontAsk` as "the locked-down end").

**Update 2: the push gate above was never the real problem, and closing it
the "obvious" way exposed a much worse one.** A follow-up live session
tried both untried options from the note above, live, against a real
login:

- `--permission-mode dontAsk` + a `permissions.allow` covering the exact
  git subcommands the prompt asks for: denied the native `Edit`/`Write`
  tools outright (not part of the threat model at all — just breakage),
  and matched `Bash` rules by *literal command prefix* — a real agent's
  own `git -c user.name=... commit` or `ENV=x git commit` never matches a
  `Bash(git commit:*)` rule, only bare `git commit ...` does — so the
  agent got stuck before ever reaching the push, having burned real API
  spend on an unproductive session.
- `--permission-prompt-tool`: does not exist as a flag on the installed
  CLI version. Ruled out, not just untried.
- Reverting to `acceptEdits` and adding one narrow `Bash(git push:*)`
  allow rule *did* get a real agent all the way to a real push — but
  tracing exactly how surfaced the actual problem: the agent had
  discovered `dangerouslyDisableSandbox: true` on its own, on an
  *earlier* dispatch, to route around the network-approval gate, and a
  direct check (`env` in an unsandboxed Bash call) confirmed
  `CLAUDE_CODE_OAUTH_TOKEN` sitting in plaintext in the sandbox's process
  environment the whole time. Any unsandboxed command — one the agent
  itself can request, and evidently will, once sandboxed execution stops
  working for whatever reason — could read it. Landlock (kernel LSM,
  `docs/design.md`'s "Abandoned" section) was checked as a fix and ruled
  out immediately: it can protect a *file*, but has no concept of
  environment variables at all, so it cannot touch this. No `sandbox.*`
  setting was ever going to fix this; it isn't a permission-mode problem.

**The fix is architectural: `claude -p` no longer runs on the sandbox at
all.** It runs on the controller as a new, dedicated, unprivileged
`grain-agent` account (`provision/controller.sh`) — never root, never the
account `grain-automation.service` itself runs as — with its native tool
roster reduced to just `Task` (`--tools Task`) and replaced by four MCP
tools in the new `grain/automation/mcp_server.py` (`run_command`/
`read_file`/`edit_file`/`write_file`, schemas mirroring Claude's own
native `Bash`/`Read`/`Edit`/`Write`, not OpenAI's `apply_patch` format —
the agent here was never trained to produce that) that reach the assigned
sandbox over SSH for everything it actually needs to do. `dispatch.py`'s
module docstring has the full mechanical account, including three things
confirmed live and easy to get wrong by reasoning alone: `--tools ''`
(or naming anything less than the full set) excludes every native tool
from the registry regardless of what `--allowedTools` separately
pre-approves; a `Task`-spawned subagent inherits the identical restricted
roster rather than a wider one (an explicit system denial confirmed this,
not self-report); and `TodoWrite` cannot be admitted in `-p`/headless mode
by any `--tools` syntax at all, unlike `Task`. `docs/design.md`'s "Final
choice: no credential in the sandbox at all" has the full security
argument for why this is the actual fix, not another layer on the same
broken model.

**The credential changed shape too, deliberately.** `configure_claude_token`
(`grain/automation/configure.py`, replacing the earlier file-based
`configure_claude_credentials`) places a bare `claude setup-token` value —
kept separate from any operator's own `claude login` session on purpose —
at a mode-600 path `grain-agent` owns, read into
`CLAUDE_CODE_OAUTH_TOKEN` at runtime by `dispatch.py`'s own unit script,
never passed as a `systemd-run` argument (which would put it in `ps`
output). `--claude-credentials-file` is `--claude-token-file` throughout
now, to match.

**Verified live, fully, end to end**: a real `grain host bootstrap`
against a mock GitHub server, a real dispatch, a real `claude -p` session
on the controller with exactly the intended tool roster (confirmed via the
transcript's own advertised `tools` list — `Task` plus the four MCP tools,
nothing else) and zero permission denials in the transcript, a real edit,
a real commit by the `grain-agent` git identity, a real push, a real PR
opened with the right head/base/title. Two more bugs surfaced only by
actually running it:

- **`grain-agent`'s access to the controller's own SSH key**: first tried
  as a group-readable copy of the same file `grain-automation.service`
  itself uses (`chown root:grain-agent`, `chmod 0640`) — broke live:
  OpenSSH's client refuses to use *any* private key file it considers
  group-readable at all, regardless of which group, so the root-run
  orchestrator's own SSH calls started failing authentication. Fixed with
  two independent, owner-only (0600) copies instead of one shared file —
  `provision/controller.sh` installs a second copy to `grain-agent`'s own
  `~/.ssh`, and `dispatch.py`/`core.py` carry that separate path
  (`CONTROLLER_AGENT_SSH_KEY_PATH`) into the per-dispatch MCP config,
  distinct from the orchestrator's own `AutomationConfig.ssh_key_path`.
- **`TodoWrite`**, covered above — tried alongside `Task` in
  `--allowedTools`, silently absent from the real transcript's tool list
  despite being named there, because `--tools ''` had already excluded it
  from the registry and `--allowedTools` alone does not add a tool back.
  `--tools 'Task'` (naming it directly) does admit `Task`; the identical
  treatment for `TodoWrite` was confirmed, twice, to never work in
  `-p`/headless mode regardless of syntax — dropped rather than left
  advertised-but-nonfunctional.

This closes the item: real repo interaction still uses a mock (a real
GitHub credential and target repo remain a separate, later step — see
`docs/next-session.md`), but the mechanism this item was actually about —
a real agent, with a real credential, doing real work through the tool
surface this design gives it — is now proven, not theorized.

## 9. Dispatch to an existing PR, not just a labelled issue

- [x] Done

A second intake path, alongside labelled-issue polling: point an agent at
an *existing* PR to address review feedback, fix CI, or continue work.
Fits the split-surface boundary unchanged — the sandbox still only ever
does `git`; it checks out the PR's existing branch and pushes more commits
to it, same as any dispatch. The three open questions resolved as:

- **Trigger: the same label, applied to a PR.** Matches the issue trigger
  exactly — same human-gate reasoning (`docs/design.md`'s "Prompt injection
  via issue content... Requiring a human to label each issue is the
  mitigation" applies identically to review comments, which are just as
  untrusted), one config field (`trigger_label`) covers both, and GitHub
  itself makes this free: a PR *is* an issue in its own data model, so the
  existing `add_label`/`remove_label` calls already work against a PR
  number with zero new code. A review-state or comment-phrase trigger would
  have meant a second detection mechanism for no real gain.
- **Context: `GitHubClient` gained `get_pull_request`, `list_pull_requests`,
  and `list_review_comments`** (`github.py`), reading real field shapes
  pinned against GitHub's REST reference rather than guessed (`head.ref`/
  `head.sha`, `base.ref`, and the review-comment object's `id`/
  `user.login`/`body`/`path`/`line`) — confirmed live via GitHub's own docs,
  not assumed. `list_pull_requests` walks the same `/issues?labels=...`
  listing `list_issues` already uses (labels are an issues-API concept; the
  pulls-list endpoint has no `labels` filter of its own) and hydrates each
  match with one `get_pull_request` call for `head`/`base`, which the
  issues listing never carries. `dispatch.py` gained a PR-shaped
  `_pr_prompt`/`dispatch_pr`, and `ensure_workspace` gained an optional
  `branch` — resetting to `origin/<branch>` and checking it out as a real
  local branch instead of `origin/HEAD`, so the agent lands on the PR's own
  history rather than the default branch. Same stdin-not-argv discipline as
  the issue path throughout: PR title/body/review-comment content never
  becomes a shell-interpolated argument.
- **Pool/state: the "one number sequence" fact, leveraged directly.**
  `AutomationState`'s `issue: int` key needed no parallel shape — an issue
  number and a PR number can never collide in one repo, so `Assignment`
  just gained a `kind: TriggerKind` (`ISSUE`/`PR`) and an optional `branch`
  (unlike an issue's, a PR's branch isn't recomputable from the number
  alone, so it's recorded once at dispatch time). Both trigger kinds share
  the same free-sandbox pool and the same `runs_per_hour` budget — one
  finite pool, one cap, regardless of what triggered a dispatch.
  `core.py._dispatch` polls `list_issues` and `list_pull_requests` together,
  merges the two candidate lists, and sorts by number alone; since the
  numbers can't tie across kinds, that's already "oldest trigger first"
  across both. `sweeper.py`'s `Outcome` carries the assignment's `kind`/
  `branch` through a sweep so `core.py._finish_succeeded` can tell "this
  needs a new PR opened" (issue path, unchanged) from "this just needs the
  in-progress label removed — the PR already exists" (new
  `_finish_succeeded_pr`, verified the same "does the branch really exist"
  way as the issue path before declaring success).

Built: `grain/automation/github.py`, `dispatch.py`, `core.py`, `state.py`,
`sweeper.py`, `config.py`, `cli.py` (status output now shows `issue`/`pr`
per assignment). Unit-tested throughout against `FakeTransport`/
`FakeRunner`/`RecordingAuditLog`, matching every existing convention —
`tests/test_automation_github.py`, `_dispatch.py`, `_core.py`, `_state.py`,
`_sweeper.py`, `_cli.py`. Verified live (`tests/test_vm_integration.py`,
extending the same real-bare-repo-behind-a-real-`GitProxy` rig item 2
built, now seeded with a second branch standing in for a PR's own): a
sandbox with no prior workspace clones straight onto the PR's branch, not
the default one; a sandbox reused from an *earlier, unrelated* dispatch on
the default branch correctly resets onto the PR's branch instead of
fetch-and-resetting back to `origin/HEAD`; and `dispatch_pr` writes the
real PR-shaped prompt (title/body/review comments) and leaves the
workspace checked out on the right branch, over the real proxy, before the
unit itself starts. What's not verified live, for the same reason item 8
names: no real GitHub repo, credential, or PR exists in this environment,
so the *trigger* (a human labelling a real PR) and the full loop end to
end are unverified beyond the mechanism.

## 10. A session browser: trigger → trajectory, over SSH

- [x] Done

Nothing before this let an operator look back at a past run —
`AutomationState` only held *current* assignments (a released slot's record
was gone), and `audit.py`'s log is one line per decision, not a transcript.
Built: browse past sessions by their trigger (the issue or PR that started
them) and see the actual trajectory — a text UI, still usable over SSH
(matches `docs/design.md`'s "the only inbound port on this host is SSH" —
no web UI). The three open questions resolved as:

- **Where trajectories come from — checked, not assumed.** Claude Code
  persists a session transcript to disk by default, for `-p` the same as an
  interactive session (`--no-session-persistence` is the opt-out, confirmed
  against Claude Code's own docs). Its default location,
  `~/.claude/projects/<cwd, "/" replaced with "-">/<session-id>.jsonl`, was
  confirmed directly against a real transcript file (not guessed): one JSON
  object per line, each carrying a `type` field (`user`/`assistant`/
  `system`/...), a user/assistant line's `message` shaped like an Anthropic
  API message (`role`, `content` blocks — `text`, `tool_use`, `tool_result`,
  `thinking`). That default path depends on an *undocumented* encoding of
  the cwd and a session ID Claude Code assigns itself, which this project
  chose not to reverse-engineer inside a bare sandbox. Instead
  `dispatch.py` asks for the *documented* stream explicitly —
  `claude -p --output-format stream-json --verbose`, redirected to a fixed
  path both `dispatch.py` and the new `capture.py` derive from
  `unit_name()` alone (`dispatch.transcript_path`) — same JSONL, same
  per-line event shape (Claude Code's docs: "the last line of the stream is
  a `result` message with the final response text, cost, and session
  metadata"), at a location this project controls. `sweeper.py`'s release
  path (`_release`, called from every branch that frees a sandbox — the
  same "guaranteed before slot reuse" argument `cleanup.py`/`health.py`
  already rest on) pulls that file over the same SSH channel `dispatch.py`
  already uses, before the slot frees — capture-on-completion, exactly as
  asked, since a reused sandbox's next dispatch overwrites the identical
  path.
- **Durable history**: `grain/automation/history.py`. Separate from
  `AutomationState` as that module's own docstring requires — one
  `<key>.json` (`SessionRecord`: issue, kind, sandbox, unit, started/
  finished times, outcome, transcript path) plus a sibling `<key>.jsonl`
  (the raw captured content) per session, under
  `/data/state/automation/sessions/`, atomic-write per file (temp +
  rename, same discipline as `AutomationState.save`). `FileSessionHistory`
  is the real store; `NullSessionHistory`/`RecordingSessionHistory` follow
  `audit.py`'s existing `NullAuditLog`/`RecordingAuditLog` shape exactly,
  so `Orchestrator.history` and `sweep()`'s new `history` parameter both
  default to a no-op and every pre-existing test and call site kept working
  unchanged.
- **TUI toolkit vs. stdlib-only — kept stdlib, on purpose.** Built with
  `curses` (`grain/automation/tui.py`), not a third-party library.
  `pyproject.toml`'s dependency-free convention held: this is an
  occasionally-used, browse-only admin tool reachable over SSH — list,
  filter, select, scroll text — squarely inside what `curses` does well,
  and `curses` ships with CPython on every POSIX target this project runs
  on. A richer library (`textual`, `urwid`) would have been the first
  third-party dependency this repo takes on for a feature that doesn't
  need what they'd add (mouse support, richer widgets). Recorded here as
  the trade actually being weighed, not defaulted past — see `tui.py`'s own
  module docstring. Split deliberately for testability: `SessionListState`/
  `DetailState`/`format_*` (which rows show, under which filter, which is
  selected) are plain data with no `curses` import, covered by
  `tests/test_automation_tui.py`; `_draw_*`/`run` are the actual curses
  mechanics, exercised only by hand (`grain sessions browse`) — curses
  screens are hard to unit test directly, so the split is what makes the
  logic testable at all rather than skipping tests because curses is
  involved.

Wired into `grain/cli.py` as a new `grain sessions` subcommand group:
`list` (filterable by `--kind`/`--outcome`/`--trigger`, for a script or a
plain shell) and `browse` (the curses TUI). `grain/automation/transcript.py`
is the trajectory parser — pure, no curses — turning a captured JSONL
trajectory into renderable events; built against the same real-transcript
shape `capture.py` confirmed, not an invented one, and degrades to showing
raw JSON for an event type it doesn't recognize rather than crashing a
whole session unbrowsable.

Unit-tested throughout, `FakeRunner`-based for the capture/dispatch pieces
matching every existing convention in this package —
`tests/test_automation_capture.py`, `_history.py`, `_transcript.py`,
`_tui.py`, plus new cases in `_dispatch.py`, `_sweeper.py`, `_core.py`,
`_cli.py`. Verified live against a real sandbox VM
(`tests/test_vm_integration.py`,
`test_sweep_captures_a_real_trajectory_file_before_freeing_the_slot`): a
plausible, realistically-shaped simulated trajectory (the exact JSONL
shape above — system/init, user, assistant text + tool_use, user
tool_result, assistant text, final result) is written to the real path
`transcript_path()` computes on a real sandbox; a real transient systemd
unit stands in for the dispatched `claude -p` process (same substitution
`docs/design.md`'s dispatch-mechanism section already uses, since no real
Claude Code login exists in this environment — item 8's constraint, not
this item's to solve); the real `sweeper.sweep()` — not a hand-called
capture function — pulls the file off over real SSH and a real
`FileSessionHistory` records it, byte-for-byte, before the slot frees; and
the captured content round-trips through the real `transcript.py` parser
into the expected event sequence. What's not verified live: an actual
interactive `curses` terminal session (needs a real TTY, not something a
test harness can drive) and a real `claude -p` run producing this format
itself (item 8's own gap, not reopened here).

## 11. Collapse setup from fourteen steps to one command

- [x] Done

All four phases from [`docs/bootstrap.md`](bootstrap.md) landed, plus the
`grain sandbox login` command that document's own "Deferred" section named
as still worth having:

- **Phase 1, the key-roles fix.** `render_meta_data` takes a *sequence* of
  keys; `LibvirtAdapter` now takes `admin_public_key_path` (embedded into
  the controller *and* every sandbox) and `controller_public_key_path`
  (sandboxes only), selected by `spec.role` in `create()`. This closes the
  actual bug: previously one key path fed every VM, and since that path was
  empty at controller-create time (the controller generates its own keypair
  at first boot), the controller came up with **no authorized key at all**
  — the documented step 5 only ever worked because an operator pre-placed
  their own key at the path step 5 then silently overwrote. With the admin
  key present from the start, the controller trusts it immediately, which
  is what makes reading the controller's own key back a scripted stage
  instead of a two-terminal human operation.
- **Phase 2, the cluster is a file.** `Cluster.load(path)` reads a TOML file
  (`tomllib`, stdlib on 3.11+) for sandbox count, subnet, bridge, image, and
  per-role sizing, falling back to the dataclass defaults for whatever's
  absent — including the file itself. `--cluster-file` and `--image` are
  new global CLI flags; `--sandboxes` now defaults to `None` and only
  overrides when passed, so a cluster file's `sandbox_count` isn't silently
  clobbered by the flag's old hardcoded default.
- **Phase 3, the three missing verbs.** `grain host wait <name>`
  (`grain/adapter/wait.py`, lifted from `tests/loadtest.py`'s already-live-
  proven `_wait_for_ssh`/`_wait_for_provisioning`); `grain host deploy
  [controller]` (`grain/adapter/deploy.py` — a `tar | ssh tar` pipeline run
  as one `bash -c` command, not composed from `SshRunner`'s stdin parameter,
  since that's text-mode and tar's payload is binary); `grain controller
  configure --repo owner/name [--github-token-file] [--claude-credentials-file]`
  (`grain/automation/configure.py` — writes `automation.json`,
  `repo-allowlist.json`, the GitHub token/credential mapping, and the Claude
  credential file, all over the same stdin-not-argv SSH shape
  `dispatch.configure_git_credentials` already established for the
  git-proxy token).
- **Phase 4, the sequencer.** `grain host bootstrap --repo owner/name
  [--github-token-file] [--claude-credentials-file]` (`grain/bootstrap.py`)
  — eleven stages, no state file, every stage converging from observed
  reality (`adapter.state()`, key-file presence, a live SSH read-back) the
  same way `state()`/`list_vms()` already do. Unit-tested against
  `FakeRunner` for the property that matters most (`tests/test_bootstrap.py`):
  the controller's key is read back *before* any sandbox is created, every
  stage is a genuine no-op when its target already converged, and a failure
  injected mid-chain (deploy) leaves a re-run able to resume rather than
  redo completed work.
- **`grain sandbox login <name>`**, docs/bootstrap.md's own "Deferred" item,
  built anyway rather than left open: direct interactive SSH to a sandbox or
  the controller using the admin key, `os.execvp`'d rather than run through
  `Runner` (this needs a real interactive terminal, not a captured result)
  — the one command in the CLI not built on `Runner.run`.

**Verified live, not just against `FakeRunner`**, on this dev host (which
has `/dev/kvm` and an already-applied `br-grain`): a dedicated two-key
sandbox proved a sandbox really does accept *both* the admin and the
controller key simultaneously (`tests/test_bootstrap_integration.py`) —
the one property no other live suite in this repo exercises at once, since
`test_vm_integration.py`'s and `test_controller_integration.py`'s own
fixtures each inject only one role's key. `wait_for_ssh`/
`wait_for_provisioning`, `deploy_tree`, and `configure_repo`/
`configure_claude_credentials` were each run against that same real VM —
real `sudo`, a real tar pipe over a real SSH hop, real file modes (`644`
for config, `600` root-owned and root-only-readable for credentials).

**A full `grain host bootstrap` run against a bare host, end to end**, run
manually once (not kept as a permanent suite entry — a second VM named
`controller` would collide with `test_controller_integration.py`'s own
session-scoped fixture on every future full-suite run): network up,
controller created and provisioned, admin key generated on first use,
controller's key read back, this tree deployed to `/opt/grain` (real files
landed, `.git`/`docs`/`__pycache__` excluded), `/data/config/automation.json`
and `repo-allowlist.json` written with the given repo, a sandbox created
trusting both keys, `grain-git-proxy.service`/`grain-automation.timer`
enabled — `grain automation status`, `github audit`, and inspecting
`/opt/grain` over SSH all confirmed the end state. Also confirmed live: the
controller's own dispatch key reaches a sandbox (the automation path is
intact) but is refused by the controller itself (`Permission denied` —
the controller trusts only the admin key, never its own dispatch key, so
splitting the roles adds a debugging path without widening the automation
credential's own reach).

**Not verified live**: the concurrent-Claude-credential-refresh open
question (`docs/bootstrap.md`, "The open question: concurrent refresh") —
unrelated to this item's own scope, still tracked as its own open question
in `docs/design.md`. Also not exercised: `--claude-credentials-file` actually
landing on a sandbox and surviving a real `claude` refresh cycle (needs a
real login credential this environment doesn't have), and the "controller
recreate → key repair on an *existing* sandbox" path in `bootstrap()`'s
stage 9 (unit-tested via `FakeRunner`'s stage-order/skip-if-present cases,
not yet run against a real controller recreate).

## 12. Let a dispatched agent ask the human a question

- [x] Done, verified live end to end against a real deployment

Until now, an agent that hit something it genuinely couldn't resolve on its
own had no way to surface that — it just ran to completion (or timed out)
with whatever it could infer, and `docs/design.md`'s split surface meant it
never had GitHub API access to comment with anyway, even if it had
something to say.

**The tool.** `mcp_server.py` gains a fifth tool, `ask_question`, unlike the
other four in one respect: it never touches the sandbox at all. It takes
only `{question: string}` and writes it to a fixed local file on the
controller — `dispatch.question_path(unit)`, the same "compute once, share"
shape `transcript_path`/`branch_name` already use — then returns a result
telling the agent to end its turn. No `Runner`/SSH involved, since the
question is for a human, not the sandbox's filesystem.

**Closing the loop without reopening the split surface.** The agent still
never gets a GitHub credential or API access of its own (`docs/design.md`'s
"Split the surface" is unchanged, see its own updated note). `core.py` is
the only thing that can act on a question:

- `_finish_succeeded_issue`/`_finish_succeeded_pr` check for a pending
  question (`_pending_question`, reading the fixed path back — same
  never-raises discipline as `capture.py`'s `capture_trajectory`) *before*
  the existing `branch_exists` check, since a run that asked a question
  almost never also pushed a branch, and the two need different handling.
- `GitHubClient` gains `create_comment` (docs/design.md's split surface
  originally flagged this as the one operation missing) and `list_comments`
  (the plain top-level conversation, distinct from `list_review_comments`'s
  inline diff comments — where a human's reply actually lands).
- `_finish_question` posts the comment and removes the in-progress label,
  but **deliberately does not re-add the trigger label** the way `_requeue`
  does for a failed/branchless run. Re-adding it would redispatch on the
  very next `run_once` and most likely re-ask the identical question,
  looping at real cost with nothing new to act on — one of the "no
  per-run cost cap" risks `docs/next-session.md` already flags. Instead the
  issue sits idle, exactly like a fresh issue, until a human replies and
  re-applies the trigger label themselves.
- A 404 from `create_comment` (a stale assignment against a repo that's
  changed out from under it — the same class of bug `docs/next-session.md`
  flagged for `_requeue`) is logged and treated as best-effort, not a crash.

**Closing the round trip.** None of this matters if the next dispatch can't
see the human's answer. `_dispatch` now fetches `list_comments` for *every*
dispatch, issue or PR alike (previously only PR dispatches fetched
anything — inline review comments), and `_prompt`/`_pr_prompt` render it as
a "conversation so far" section, blank-state included (matching the
existing "(no inline review comments)" convention). `dispatch()`/
`dispatch_pr()` both gained a `comments`/`thread_comments` parameter to
carry this through — `AutomationState` itself remembers nothing of the
round trip once an assignment is released, so the comment thread on GitHub
is the only durable record of "a question was asked and answered."

**The reused-sandbox hazard, closed at the write end, not the read end.**
`question_path(unit)` is fixed per sandbox, not per task (same as
`transcript_path`) — reused verbatim by whatever task this sandbox gets
next. `_start_task` resets it (`rm -f`) at the start of *every* dispatch,
so a question from one task can never be misread as belonging to a later,
unrelated one; the read side doesn't need to clean up after itself for
correctness; only writing needs to be careful about *when*, not reading
about staleness.

**Verified live**, end to end, against a real `bwsalmon/test1` deployment
(one controller, one sandbox, real GitHub PAT and Claude OAuth token, timer
stopped and driven by hand via `automation run-once`): a real issue (#3),
worded to require a human decision before proceeding, was labelled and
dispatched. The real `claude -p` session called `ask_question` on its own
judgment (no forcing beyond the issue's own wording) with a sensible
question, took no other action, and ended its turn in 2 turns at $0.06. The
sweep read the question back, posted it verbatim as a real comment on
issue #3, and removed the in-progress label without re-adding the trigger
label — confirmed the issue sat with no labels at all afterward, exactly as
designed. A human reply ("Use 3 retries.") plus re-applying the trigger
label triggered a real redispatch; its prompt (read directly off the
controller) carried both the agent's question and the human's reply in the
new "conversation so far" section. That second run read the reply, wrote a
correct README example using the answered value (3), pushed to
`grain/issue-3`, and the sweep opened a real PR (#4) whose diff matches the
answer exactly.

One unrelated hiccup the agent hit and self-corrected without help: its
first push attempt (`git push origin HEAD:grain/issue-3` from the detached
HEAD `ensure_workspace` leaves a fresh default-branch checkout in) failed
with git's "not a full refname" error; the agent diagnosed it, ran
`git switch -c` to attach HEAD to a local branch, and retried the identical
push successfully. Not a bug in this item — `_prompt`'s push instruction and
`ensure_workspace`'s detached-checkout behavior predate `ask_question`
entirely, and this would reproduce on any fresh issue dispatch, asked
question or not — but worth a follow-up item since it cost the run an extra
turn.

(Dry-run against this same real deployment separately surfaced its own
finding, unrelated to this item: `DryRunRunner` echoes a mutating command's
full stdin when printing instead of running it, which defeats the
stdin-not-argv protection for exactly the steps that carry real secrets --
`configure_github_credential`/`configure_claude_token`. `--dry-run` is safe
for read-only inspection but not for previewing a real credential-bearing
bootstrap. Worth a guardrail: redact stdin for any command dry-run prints,
or omit it entirely for the credential-writing steps.)

## 13. Auto-redispatch once a trusted human replies to a question

- [x] Done

Item 12 required an operator to notice the question comment and manually
re-apply the trigger label to get a redispatch. This closes that gap: a
reply from someone with write-level repo access now redispatches on its
own, no relabeling needed.

**Visible lifecycle, not just internal bookkeeping.** `_finish_question`
now applies a new `awaiting_reply_label`
(`AutomationConfig.awaiting_reply_label`, default
`"grain-agent-awaiting-reply"`) in place of just removing the in-progress
label — the same reasoning that gave `trigger_label`/`in_progress_label`
their own visible labels in the first place: an operator scanning the
repo's issues should be able to tell "genuinely idle, waiting on a human"
apart from "untouched" without reading `automation status`.

**The trust boundary, not just the mechanism.** The obvious naive version —
"any new comment redispatches" — would reopen the exact prompt-injection
gate the trigger label exists to close (docs/design.md's split surface): on
a public repo, anyone who can comment (not just anyone with push access)
could reply to a question thread and redispatch the agent with content of
their choosing. `Comment` gains `author_association` (GitHub's own field:
`"OWNER"`/`"MEMBER"`/`"COLLABORATOR"`/`"CONTRIBUTOR"`/`"NONE"`/...) and
`core.py`'s new `_promote_answered_questions` only promotes a reply from
`{"OWNER", "MEMBER", "COLLABORATOR"}` — the same trust tier "can apply a
label" already implies. A random public reply is simply ignored; the issue
stays `awaiting_reply_label`'d.

**The mechanism itself needs no new hydration path.** `_finish_question`
now records a `PendingQuestion` (`state.py`: `issue`, the question
comment's own id, `kind`, `branch`) via `state.record_pending_question`.
`_promote_answered_questions` (new, run between `_sweep` and `_dispatch` in
`run_once`) checks each pending question's `list_comments` for anything
with a *higher* id than the recorded one (not a count or timestamp — both
of which a deleted or backdated comment could make lie) from a trusted
author. If found, it just re-adds `trigger_label` and clears the pending
entry — `_dispatch`'s own polling, unchanged, picks the now-labelled item
up in the very same `run_once` call, because it already fetches full
`Issue`/`PullRequestDetail` objects for anything carrying the trigger label.
No separate "fetch this bare issue number back" method needed.

`GitHubClient.create_comment` now returns the new comment's id (was `None`)
— the baseline `PendingQuestion` needs. A comment id, not a timestamp,
because it can't be spoofed by editing an older comment's body and stays
valid even if other comments in between get deleted.

A 404 while checking for a reply (the same "stale assignment, repo
reconfigured out from under it" story `_requeue`/`_finish_question` already
handle) clears the pending question and logs, rather than crashing the
sweep.

**Verified against the FakeTransport-based unit suite only** — the live,
end-to-end run described in item 12 predates this item, so the actual
redispatch-on-reply path (as opposed to the original ask-then-manually-
relabel path) has not yet been exercised against a real repo.

## 14. A visible signature on everything grain-agent posts

- [x] Done

Nothing previously marked a PR or comment `core.py` posts as automation
output — the only signal was reading the text itself ("Opened
automatically...", "grain-agent has a question..."), easy to miss when
skimming a list of PRs or a busy issue thread, and identical in every way
GitHub's UI actually renders (title, list view, notification email) to a
human-authored one unless the deployment happens to use a dedicated bot
account (docs/design.md's own recommendation, but an operator choice, not
something the code can enforce).

`core.py` gains one constant, `_AUTOMATION_SIGNATURE`
(`"🤖 Posted automatically by grain-agent — not a human."`), applied
consistently:

- Every PR `_finish_succeeded_issue` opens gets a `🤖` prefix on its title
  and the signature as a footer on its body.
- Every comment `_finish_question` posts (the `ask_question` relay, item
  12) leads with the signature before the question itself.

Purely textual — no new GitHub API surface, no behavior change, just makes
"did a human write this or did the automation" answerable at a glance
without reading the body. The dedicated-bot-account recommendation in
docs/design.md is still the stronger signal where it's available (GitHub's
own avatar/username in every view); this is what's true regardless of
which credential a given deployment actually uses.

## 15. One task repo, many target repos

- [x] Done

Everything before this item assumed one repo per deployment: the repo whose
issues were polled was also the repo that got cloned, pushed to, and opened
a PR against. That is the wrong shape for a set of agents working across
several services — it means one controller, one `/data`, one credential
ladder, and one pool per repo.

**The split.** `AutomationConfig`'s `owner`/`repo` become
`task_owner`/`task_repo`: the *task repo*, a queue of issues for the agent
set. It is the only repo polled, and the only repo labelled or commented
on. Each task names its own *target* repo in its text, with a slash
directive parsed by the new `grain/automation/directives.py`:

    /repo acme/widget-service
    /pr 42            optional: continue that PR rather than a fresh branch
    /base develop     optional: PR base, default the target repo's own

Directives are read from the issue body *and* from comments by trusted
authors (`_TRUSTED_REPLY_ASSOCIATIONS` — the same "could have applied the
label" tier the trigger gate relies on), later texts overriding earlier, so
repairing a task is a reply rather than an edit plus a reply. They are
stripped from the prompt: they address the orchestrator, and the agent has
no GitHub API access to act on one anyway.

**Why a directive and not a label.** A `repo:owner/name` label has to exist
in the task repo before it can be applied, is awkward to create once per
target, and can carry neither a PR number nor a base branch. One mechanism
covers all three.

**Fail closed on the target repo.** `Orchestrator` now takes an
`Allowlist` — the same `/data/config/repo-allowlist.json` the git proxy
already enforces, not a second list that could disagree with it. A task
naming an off-list repo never dispatches. Without this the dispatch still
fails (the proxy denies the clone), but as a `CommandError` from inside a
sandbox rather than a sentence a human can act on.

**Parking, not failing.** An unusable directive (missing with no
`default_target_repo` configured, malformed, off-list, or a repo GitHub
404s) routes into the state docs/roadmap.md item 13 already built: comment,
swap the trigger label for `awaiting_reply_label`, record the comment id as
the reply baseline. A maintainer's reply — which may itself carry the
corrected `/repo` line — puts the trigger label back on the next cycle.
Leaving the trigger label on instead would redispatch the identical failure
every two minutes with nothing new to act on, which is the same argument
`_finish_question` already makes for a question.

**Item 9's PR trigger changed shape.** Polling a second listing for
labelled *pull requests* stopped making sense once PRs live in target
repos, where no label of this deployment's ever gets applied — so
`list_pull_requests` is gone and a PR-continuation task is a task issue
carrying `/pr N`. Everything downstream is unchanged: same pool, same rate
limit, `TriggerKind.PR`, the PR's own branch in the workspace, review
comments in the prompt, and no new PR opened on success. What differs is
that the trigger's number is now the *task issue's*, so labels, questions
and requeues all still land in the one repo this deployment writes to.

**What had to carry the target.** `Assignment` (and `sweeper.Outcome`) gain
`target_owner`/`target_repo`/`base`, recorded at dispatch rather than
re-parsed at sweep time: an issue body is editable, and an edit landing
mid-run must not be able to redirect where the finished work's PR is
opened — the same "decide once, verify don't trust" discipline
`branch_name()` already applies to the branch. Both are `None` for an
assignment written before this item, which `core.py` reads as the task
repo, exactly what it meant then. `SessionRecord` gains `target` so the
session browser can answer "which repo did this touch" from the list view.

**Credentials became per-repo.** A cycle now talks to the task repo *and*
to each target repo, which may need different credentials.
`GitHubClient`'s fixed token becomes a `TokenSource` resolved per call —
every method already took `owner, repo` as its first two arguments — and
`CredentialSet` (narrowest pattern first: `owner/repo`, `owner/*`, `*`)
satisfies it directly. That ladder was built for exactly this and had never
been wired to more than one repo. A bare `str | None` still works.

**The PR base comes from the target repo.** `AutomationConfig.base_branch`
was a fair guess for one repo and an unmaintainable per-repo table for
many, so it is gone: `GitHubClient.default_branch` reads the target repo's
own `default_branch`, `/base` overrides it, and the answer is pinned onto
the assignment at dispatch. The PR body's closing reference is fully
qualified (`Closes owner/tasks#N`) — a bare `#N` would name an issue in the
target repo, which is a different issue entirely.

**Migration.** `AutomationConfig.load` accepts the legacy `owner`/`repo`
keys and ignores a legacy `base_branch`, so an already-deployed `/data`
needs no edit. `grain controller configure`/`host bootstrap` take
`--task-repo` (with `--repo` still accepted as its former name) and a
repeatable `--target-repo`; with no `--target-repo` at all, the task repo
becomes the sole allow-listed target *and* `default_target_repo`, which is
precisely the single-repo deployment every deployment was before this item
— it keeps working with no directive written anywhere.

## 16. Give each agent a unique id

- [x] Done

A task can involve the agent creating infrastructure of its own — a
container, a cloud resource, a scratch database — as part of the work, not
just editing files in its checkout. Nothing named that infrastructure for
it, and nothing stopped two concurrently-dispatched agents (this deployment
already runs more than one sandbox at once, see item 3's live concurrency
test) from picking the same obvious name and colliding.

`agent_id()` (`grain/automation/dispatch.py`) mints an 8-hex-character
`secrets.token_hex(4)` value fresh for every dispatch — no need to be a
pure function of anything the way `branch_name()`/`transcript_path()` are,
since nothing on the controller side ever has to recompute it to agree.
`dispatch()`/`dispatch_pr()` generate one and thread it into `_prompt()`/
`_pr_prompt()` as `agent_id_value`, which render it as a plain sentence
telling the agent its id and inviting it to fold that id into any
infrastructure name it picks, so two agents' infrastructure can never
collide on name alone. Purely a prompt addition — no new MCP tool, no new
state to persist, nothing for `core.py` to verify, since unlike the branch
name nothing downstream ever needs to check what the agent actually did
with it.

## 17. Close a task issue when its PR closes, not when the agent finishes

- [x] Done

`_finish_succeeded_issue` used to close the task issue the instant it
opened a PR for it — before anyone had reviewed anything. Opening a PR only
proves the agent's own part is done; the task itself isn't, until a human
has merged (or decided to close without merging) that PR. bwsalmon/agents#54
asked for the issue to track the PR's own close instead.

**No webhook, so a poll.** docs/design.md's cron-not-webhooks stance
(item 8) still holds, and the PR lives in the *target* repo (item 15's
task/target split) while every label and close this deployment writes
lives on the task issue in the *task* repo — so there's no label move to
piggyback on either. `_finish_succeeded_issue` now records the PR against
the issue (`state.py`'s `OpenPullRequest`: issue number, target owner/repo,
PR number) instead of closing anything itself, and a new pass,
`_close_finished_prs` (run between `_promote_answered_questions` and
`_dispatch` in `run_once`, the same slot item 13's own polling pass
occupies), checks every such record each cycle and closes the task issue
once `GitHubClient.get_pull_request` reports that PR's own `state` as
`"closed"` — merged or closed without merging both read that way, and
count the same here: either means nobody is pushing more commits to it.
`PullRequestDetail` gained the `state` field this needs, defaulted to
`"open"` so no existing caller (none of which cared about it before this)
needed updating.

A 404 from `get_pull_request` (the target repo or the PR itself is gone —
an operator narrowed the allowlist, or the PR was deleted outright) or from
`close_issue` (the task issue itself is gone) is the same "stale record,
not a reason to crash the cycle" tolerance `_requeue`/`_finish_question`
already have; either just drops the record and logs.

**A visible marker regardless of when (or whether) the issue closes.**
bwsalmon/agents#54 also asked for a label on every task the agent
considers its own part done with, `completed_label`
(`AutomationConfig.completed_label`, default `"grain-agent-completed"`).
It goes on immediately in every finishing path — `_finish_succeeded_issue`
(the moment the PR opens), `_finish_succeeded_pr` (a PR-continuation task
pushing more commits to a PR that already existed before the task, whose
own lifecycle this deployment was never closing anyway), and
`_finish_analysis` — independent of whether the issue itself ever
auto-closes. An analysis (item 12's sibling, bwsalmon/agents#50) still
never auto-closes at all: there is no PR whose merge or close is a natural
"done" signal to wait on, only a summary a human should actually read
first, so `_finish_analysis` drops `close_issue` entirely rather than
switching it to poll anything.

## 18. Give every generated PR a real description

- [x] Done

bwsalmon/agents#79: `_finish_succeeded_issue`'s PR body was built entirely
from metadata it already had on hand — which task, which sandbox, a
`Closes` line — and never said anything about what the change actually
did, so a number of generated PRs read as description-free.

**The pushed branch's own head commit message is the fix, not a new
signal.** The agent already writes a commit message to explain its diff;
the only gap was that nothing carried it into the PR. `GitHubClient` gains
`get_branch_head` (`BranchHead`: `sha`, `message`), reading the same GET
`branch_exists` already made against `/repos/{owner}/{repo}/branches/{branch}`
— GitHub's own branch response nests the tip commit's message at
`commit.commit.message`, so this costs no extra call over what "verify,
don't trust" (item 2) already paid for. `_finish_succeeded_issue` calls
this in place of `branch_exists` and leads the PR body with `head.message`,
the `Closes <task>#<n>` line and the automation signature (item 14) kept
below it as a `---`-separated footer rather than the whole story.
`branch_exists` itself is untouched, still used as-is by the
PR-continuation path (`_finish_succeeded_pr`), which opens no new PR and
so has no body to seed.

**The prompt has to ask for it.** `dispatch.py`'s `_prompt` (the
fresh-issue dispatch; `_pr_prompt`'s PR-continuation path never triggers a
new `create_pull_request` call, so it's unchanged) now tells the agent
plainly that its final commit message becomes the PR description verbatim,
and asks for a summary line plus a paragraph of explanation — the same
shape a human would write for a reviewer, not a `git log`-only note.

## 19. Auto-suggest a fix for a completed task's conflicting or failing PR

- [x] Done

Item 17's own poll, `_close_finished_prs`, only ever watched an open PR
for one thing: has it closed. bwsalmon/agents#83 asked for a second thing
to watch for while it's still open — a conflict with its base branch, or a
failing check — and for grain to do something about it rather than leave
it for a human to notice by hand.

**Read, don't guess.** `PullRequestDetail` gained `mergeable`
(`GitHubClient.get_pull_request` already reads the whole object; the field
was simply never carried through before). GitHub computes it
asynchronously, so it is `None` for a cycle or two after a push —
`_pr_health`'s `_PrHealth.has_conflict`/`.is_broken`/`.is_clean` all treat
`None` as "don't know yet," never as either a conflict or a clean merge. A
new `GitHubClient.list_check_runs` (paginated the same `Link`-header way
every other list call here is, but the one endpoint whose body is
`{"total_count", "check_runs"}` rather than a bare array) supplies the
other half: any completed check with conclusion `failure`/`timed_out`/
`action_required` counts as broken; anything still `queued`/`in_progress`
counts as pending, not broken -- a check that hasn't finished yet may
still pass.

**Suggest, don't act — the issue's own words: "needing user approval."**
`_suggest_fix` files a *new* task issue (`GitHubClient.create_issue`, also
new) the moment `_pr_health` reads a definite conflict or failing check,
carrying `/repo`, `/base <the open PR's own head ref>` and `/auto-merge`
(directives.py's new fourth directive). It's filed with
`needs_approval_label` (`AutomationConfig.needs_approval_label`, default
`"grain-agent-needs-approval"`) instead of `trigger_label` — a new state
label, styled the same dark tier `awaiting_reply_label` is in
`labels.py`, for the same reason: it's a task nobody has approved to run
yet. `_dispatch` strips it the moment a human's `trigger_label`
(re-)approves the task and it actually dispatches — the same "exactly one
state label at a time" invariant every other state transition here
already holds to. `state.open_pull_requests` gained `fix_issue` so this
only ever happens once per PR — a second failing check on the same PR
after a fix has already been suggested doesn't file a second one.

**The stacked branch needs no new dispatch machinery at all.** A task's
`/base` already builds its fresh branch on top of *any* named branch, not
just a target repo's default (item 15) — so `/base <original PR's head
ref>` already *is* a stacked PR: `_resolve_target`/`dispatch()` need no
changes whatsoever to produce one.

**Auto-merge closes the loop the issue asked for: "If approved, auto-merge
the stacked PR with the original."** `/auto-merge` (`Directives.auto_merge`,
threaded through `Assignment`/`Outcome` the same way `target_owner`/`base`
already are) marks the PR `_finish_succeeded_issue` opens for that task as
`OpenPullRequest.auto_merge`. `_close_finished_prs` merges it itself
(`GitHubClient.merge_pull_request`, a third new client method) the moment
`_pr_health` reads it clean — mergeable, no pending or failing check —
instead of only ever waiting on a human to close it the way an ordinary
task's PR does. A 405/409 (the PR went stale between the read and the
merge attempt) is logged and retried next cycle, not raised. Deliberately
excluded from ever getting a fix suggested for it in turn — a fix for a
fix risks an unbounded chain — so a fix whose own PR goes wrong is left
open, visibly, rather than escalated.

## 20. Closing an issue should cancel the underlying agent

- [x] Done

Nothing stopped a dispatched unit from running to completion (or sitting
until `max_runtime_minutes`) after a human closed its task issue by
hand — bwsalmon/agents#82 asked for that work to actually stop.

**No webhook, so a poll, same as item 17.** `sweeper.py`'s own docstring
still holds ("this module knows nothing about GitHub"), so `sweep()`
gained an optional `is_issue_closed: Callable[[int], bool] | None` hook
instead of a `GitHubClient` of its own. It's consulted only for an
assignment `sweep()` would otherwise leave running untouched — still
`ACTIVE` and within budget, not one this same pass already found
`DONE_SUCCESS`/`DONE_FAILED`/`ABSENT`/stranded, which already have a
terminal outcome to report. That keeps the check to at most one extra
GitHub call per still-active assignment per cycle, not one per in-progress
issue regardless of status, and means every pre-existing caller (`None`,
the default) sees no behaviour change and pays no extra call at all. A
cancelled unit is reaped the same reap-then-release way a run past its
runtime budget already is, reported through a new `SweepResult.cancelled`
list distinct from `stranded` — `core.py`'s `_is_issue_closed` (`get_issue`
against the task repo, `Issue` now carrying GitHub's own `state` field)
is the hook `_sweep` wires in, and `_finish_cancelled` handles the result:
`in_progress_label` comes off so a closed issue doesn't sit looking
mid-flight forever, but unlike `_requeue`'s failed/stranded handling the
trigger label never goes back on — a closed issue must not come back for
redispatch.

## 21. Stop letting an agent's own signal decide whether a PR opens

- [x] Done

bwsalmon/agents#89: `complete_analysis` (item 12's sibling, bwsalmon/agents#50)
was checked *before* the branch in `_finish_succeeded_issue` and, if the
agent had called it at all, skipped the branch check outright. That made
the tool call and the branch two signals that could disagree, and in
practice they did — an agent that pushed real commits and then also
(mistakenly, the common failure mode the issue was filed over) called
`complete_analysis` at the end had `core.py` silently drop the PR those
commits earned, since the file-based signal won regardless of what was
actually on the branch.

**The fix is to stop trusting a self-report for something already
verifiable.** The branch is now checked first, unconditionally, in every
case — exactly the "verify, don't trust" bar item 2 already set for
whether a branch exists at all, just applied one step further to whether
the *no-PR* outcome is legitimate too. The tool (renamed `comment_on_issue`
to match: it no longer marks a task as an analysis, it just leaves a
comment) still records its argument to the same kind of fixed per-unit
file `ask_question` already uses, and `core.py`'s sweep still reads it
back, but only ever *after* `get_branch_head` has already come back empty
— a comment can request the no-PR outcome, but only when the branch
actually is empty; it can never override a branch that has real commits on
it. A comment left with real commits on the branch is simply not consulted
at all: the PR opens exactly as it would if the agent had never called the
tool, since a pushed branch has never needed anything to explain it.

An empty branch with *no* comment still requeues exactly as before item 12
ever added an analysis path at all: "the agent said nothing and pushed
nothing" is still a run worth retrying, not a silent success, and this
issue never asked for that safety net to go away. `_finish_analysis`
(the handler that used to run whenever `complete_analysis` had been called,
branch or not) is now `_finish_no_changes`, only ever reached once the
branch check has already ruled out a PR — its own docstring covers what
was renamed and why.

## 22. A janitor for what agents leave behind in GCP

- [x] Done

item 16 tells every dispatched agent to fold its own id into any
infrastructure it creates, but that is a prompt sentence, never enforced or
persisted anywhere `core.py` can check — nothing named what an agent
created, and nothing ever went back to delete it. A crashed run, or a task
that just forgot, left a GCE instance, a disk, or (for a task that used
`grain-gemini-key`) a stranded API key sitting in the project indefinitely.
`gemini_keys.py`'s own docstring already documents two of the narrower
leak paths this closes: a mint that fails partway (bwsalmon/agents#104) and
`sweeper.py`'s `_revoke_gemini_key`, which deliberately "leaves for an
operator to clean up by hand" a key minted before `gemini_key_config` was
removed mid-flight.

`grain/automation/janitor.py`'s `run_janitor` is a new, optional pass —
`core.py`'s `_janitor`, run from `run_once` alongside the stranded-work
sweeper — that lists GCE instances, disks, and Gemini API keys in the
configured project over `gcloud` (same "shell out, don't hand-roll the
OAuth2 exchange" reasoning `gemini_keys.py`'s own docstring already gives,
authenticated with the exact same primary service-account key) and deletes
whichever are older than a configured TTL (default 24h). Since nothing
actually marks a resource as agent-created, this is an exclusion list, not
an inclusion list: it deletes anything past the TTL *except* what it can
positively identify as grain's own core infrastructure — the host VM and
its data disk, by the exact names Terraform gives them, and anything
carrying this deployment's own Terraform labels (default
`managed-by=terraform`) — never raising on a single listing or deletion
failure, the same discipline `sweeper.py`'s own health/credential warnings
already hold to.

Off by default (`/data/config/janitor.json`'s presence is the switch, same
shape as `gemini-key.json`); `grain controller configure --janitor-ttl-hours`
sets it up by hand, and a Terraform-managed deployment can turn it on
declaratively with `enable_janitor`/`janitor_ttl_hours` in `grain.tfvars`
instead — see `terraform/gcp/variables.tf` and docs/runbook.md's "Enabling
the janitor". It only has anything to clean up once the agent account
already has the roles `agent_can_manage_compute_instances`/
`enable_gemini_key` grant it — turning it on alone is a harmless no-op.

## 23. A comment on a completed issue should restart it

- [x] Done

bwsalmon/agents#135: once a task issue carries `completed_label` (any of
the three finish paths — a fresh PR opened, more commits pushed to an
existing PR, or a no-branch "here's the answer" comment), it just sits
there. A human reviewing that work who wants more done had no way to say
so short of re-applying `trigger_label` by hand — a comment alone, even a
maintainer's, did nothing.

`core.py`'s `_restart_commented_completions` closes that gap with the same
"poll, don't trust a webhook" shape `_promote_answered_questions` already
uses for a reply to a question: `state.py`'s new `CompletedIssue` record
tracks one completed issue's `list_comments` baseline, and a `run_once`
pass diffs the current thread against it every cycle. A comment newer than
the baseline from a `_TRUSTED_REPLY_ASSOCIATIONS` author — the same trust
tier every other comment-triggered redispatch here already requires, so a
random public commenter still can't restart the agent set on a whim —
reopens the issue (`GitHubClient.reopen_issue`, the mirror image of
`close_issue`, needed because bwsalmon/agents#54's `_close_finished_prs`
may already have closed it once its PR closed), swaps `completed_label`
back for `trigger_label`, and drops any `open_pull_requests` record still
tracking that issue's old PR — otherwise a later `_close_finished_prs`
would close the freshly reopened issue again the moment that stale PR
itself closed, with no new work behind it.

`CompletedIssue.baseline_comment_id` starts `None` rather than being
filled in at completion time the way `PendingQuestion.question_comment_id`
is: two of the three finish paths that apply `completed_label` never post
a comment of their own, so there's no id finish time can hand back as "the
highest comment on this issue right now." The first poll after completion
primes the field from a fresh `list_comments` read instead of ever
restarting on it — comparing on that very first read would risk treating
either a comment already on the issue before the run even started, or (the
third finish path's own `comment_on_issue` reply) the automation comment
that finish path just posted, as a "new" one and restarting a task nobody
actually asked to restart.

## 24. Restart in-progress jobs that survived a lost state

- [x] Done

bwsalmon/agents#139: item 8's "Update" (bwsalmon/agents#51) already made
sure a controller crash or VM restart *mid-`run_once`* can never lose an
in-progress task, by saving `AutomationState` incrementally rather than
once at the end — but that whole recovery path assumes the state file
itself survives the restart. A restart that also loses `/data` (a fresh
volume, a wiped or corrupted `state.json`, a from-scratch redeploy — what
the issue title calls grain being "restarted or reformatted") comes back
up with `AutomationState.assignments` empty. Every task issue still
carrying `in_progress_label` from before that happened is now invisible to
every existing poll: `_dispatch` only ever lists `trigger_label`, and the
sweeper only ever looks at assignments *on disk* — so with neither in
play, such an issue would sit `in_progress` forever with no agent actually
working it, indistinguishable from the queue's point of view from a task
that's simply taking a long time.

`core.py`'s `_restart_orphaned_in_progress` closes that gap by treating
GitHub's own labels as the fallback source of truth, the same "poll, don't
trust a cache" bar every other reconciliation pass here already holds to:
every `run_once`, it lists every issue still carrying `in_progress_label`
and, for any one this process's own state has no assignment for, gives it
exactly the treatment `_requeue` gives a stranded run — `in_progress_label`
off, `trigger_label` back on, every sandbox's `agent_label` stripped since
there's no assignment left to say which one it was — so the very next
`_dispatch` in the same cycle picks it back up. Deliberately scoped to
`in_progress_label` alone: `awaiting_reply_label`/`completed_label` are
already resting states with a human reply or a later poll expected to move
them along, not silently orphaned work in the sense this closes the gap
on.

## 25. Add a review option

- [x] Done

bwsalmon/agents#154: every existing task either changes code (a fresh
branch or a `/pr`-continuation) or answers a question (`comment_on_issue`)
— there was no way to ask the agent set to just *read* a pull request and
leave feedback, without either pushing a competing branch or dumping one
long comment with no attachment to specific lines.

`/review true` (`directives.py`), only honoured alongside `/pr N`
(`core.py`'s `_resolve_target` refuses it otherwise — a review has nothing
to check out or post against without a PR number in hand), is a third
dispatch shape alongside a fresh issue and a `/pr`-continuation:
`dispatch.py`'s `dispatch_review` checks out the PR's own branch exactly
`dispatch_pr` does, but `_review_prompt` tells the agent this is read-only
— no `git push` instructions at all — and to use a new MCP tool,
`add_review_comment`, instead. That tool (`mcp_server.py`) appends one
piece of feedback at a time (a `path`/`line` pair to attach it to a
specific line of the diff, or neither for a general remark) to a fixed
per-unit JSON file, the same "only ever writes locally, `core.py` posts
the human-facing half" shape `ask_question`/`comment_on_issue` already
have — just accumulating instead of overwriting, since a review is
naturally many small points rather than one.

A new `TriggerKind.REVIEW` (`state.py`) carries the target PR's own number
through to sweep time (`Assignment.pr_number`/`Outcome.pr_number`, next to
the branch a PR assignment already carries), and `core.py`'s
`_finish_succeeded_review` reads back whatever the agent left and posts it
as one **draft** review (`GitHubClient.create_review`, bwsalmon/agents#154's
addition to `github.py`) — the request carries no `event` key at all,
which is what keeps GitHub from ever submitting it: an agent reviewing its
own (or anyone's) code is never the one who gets to approve it, request
changes on it, or even publish a plain comment review of it. The draft
sits `PENDING`, visible only to the credential that created it, until a
human opens it on github.com and submits it themselves. An agent that
looked and found nothing worth flagging leaves the file unwritten, and
`_finish_succeeded_review` posts no review at all rather than an empty one
nobody asked for.
