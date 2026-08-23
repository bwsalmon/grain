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

- [x] Done

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
