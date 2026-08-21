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

- [ ] Done

There is no `provision/controller.sh` or equivalent, and no `/data` disk
layout actually created anywhere — `grain/automation/`'s config/secrets/
state paths have only been exercised against ad-hoc directories during
testing. Build the controller's own provisioning script and CLI wiring
(mirroring `provision/sandbox.sh` and `grain host create`), including the
controller SSH keypair generation step (`ssh-keygen` at
`/data/secrets/controller-ssh`) that's currently a manual, undocumented
prerequisite for `libvirt.py`'s `ssh_public_key_path`.

Item 2 narrowed this slightly, worth noting so it isn't rediscovered: sandbox
git-proxy token *minting and distribution* (`grain/proxy/tokens.py`'s
`SandboxTokenStore`, wired into `core.py`'s dispatch path) is now done — a
sandbox without a token yet gets one on its first dispatch, delivered over
the same SSH channel `dispatch.py` already used. That consumes the
controller SSH key this item's still-missing keypair-generation step is
about, but doesn't create it — the controller SSH keypair itself
(`ssh-keygen` at `/data/secrets/controller-ssh`) is still a manual,
undocumented prerequisite, same as everything else this item scoped: the
controller VM, its provisioning script, and its `/data` disk layout.

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
exactly as a real permission failure would be). Also not exercised: the
`--uid=grain-metadata` system user and `provision/controller.sh` itself,
since neither exists yet (item 3, still open) — the launcher assumes both
and documents that assumption in `metadata/config.py`.

## 5. Lifecycle scripts

- [ ] Done

`grain host recreate` covers destroy-then-rebuild. Still missing, per
`docs/design.md` step 8: a between-task cleanup hook (docker system prune
etc., run between agent tasks on a long-lived sandbox), a health check
command, and a disk-watermark alarm.

## 6. Load-test harness for open question 2

- [ ] Done

Design doc open question 2: does 4 vCPU hold two sandboxes plus a
controller under real `kind` + build load? One sandbox at rest was
measured (`docs/design.md`'s implementation-plan step 4 notes). Build a
small script that brings up two sandboxes, drives concurrent `kind`
cluster creation and a build in each, and records CPU/memory pressure —
then actually run it and record the numbers here or in `docs/design.md`.

## 7. Hardening

- [ ] Done

`docs/design.md` step 10: move repos down the credential ladder (App →
fine-grained PAT → machine-account PAT → personal token, per repo), apply
branch protection on target repos, confirm no credential in
`/data/secrets/github/` carries `workflow` scope, and write the operator
runbook the rest of this work has been assuming exists.

## 8. First live issue-to-PR run

- [ ] Done

Verification, not implementation — flagged separately because it needs
things no agent can provide on its own: a real target GitHub repo, a real
credential in `/data/secrets/github/`, and a sandbox with Claude Code
actually logged in (still a manual step per `docs/design.md`). Blocked on
items 2 and 3 above at minimum. This is the point where the whole pipeline
gets checked end to end, the same "verify live" bar every other piece of
this project has been held to.

## 9. Dispatch to an existing PR, not just a labelled issue

- [ ] Done

A second intake path, alongside labelled-issue polling: point an agent at
an *existing* PR to address review feedback, fix CI, or continue work —
"send an agent to a PR and have it address issues," not just originate
one. Fits the split-surface boundary unchanged — the sandbox still only
ever does `git`; it checks out the PR's existing branch and pushes more
commits to it, same as any dispatch. What's new is on the controller side:
- **Trigger**: a label on the PR, a "changes requested" review state, or a
  comment containing a trigger phrase — needs a decision, don't assume.
- **Context**: the dispatched prompt needs the PR's diff and review
  comments instead of (or alongside) an issue's title/body — `github.py`
  needs a `list_pull_requests`/`get_pull_request_comments`-shaped read path
  it doesn't have yet.
- **Pool/state**: `AutomationState` currently keys assignments by issue
  number; decide whether a PR-triggered run reuses that shape or needs its
  own.

Sequenced after item 2 (reuses the same `GitHubClient`/label-move patterns
PR creation establishes) and item 8 (prove the simpler issue→PR loop end
to end before adding a second intake path on top of it).

## 10. A session browser: trigger → trajectory, over SSH

- [ ] Done

Nothing today lets an operator look back at a past run — `AutomationState`
only holds *current* assignments (a released slot's record is gone), and
`audit.py`'s log is one line per decision, not a transcript. Requested:
browse past sessions by their trigger (the issue or PR that started them)
and see the actual trajectory — a text UI, not just the CLI, but still
usable over SSH (matches `docs/design.md`'s "the only inbound port on this
host is SSH" — no web UI).

Real open questions to resolve before coding, not assume:
- **Where do trajectories come from?** `claude -p` writes its own session
  transcript inside the sandbox; nothing pulls it back to the controller
  today. Check what `claude -p` actually leaves behind and where. Likely
  needs the sweeper to fetch it over SSH (same channel `dispatch.py`
  already uses) *before* a finished sandbox's slot gets reused for the next
  task — capture-on-completion, not fetch-on-demand, or a later browse
  finds nothing.
- **Durable history.** `AutomationState` is deliberately live-pool-only;
  this needs a separate append-or-archive store keyed by trigger (issue #
  or PR #) → {sandbox, unit, started_at, finished_at, outcome, transcript
  path}, most likely under `/data/state/automation/sessions/`.
- **TUI toolkit vs. the stdlib-only convention.** `pyproject.toml` is
  deliberately dependency-free ("this runs on a stock Debian host"). Python's
  built-in `curses` fits that; a richer library (textual, urwid) would be
  the first dependency this project takes on — a real trade to weigh
  explicitly, not default past.

Depends on item 2 (PR-triggered sessions need PR creation to exist) and
item 9 (browsing "issue or PR" needs the PR-trigger path to exist) to be
fully meaningful, though an issue-only version is useful on its own before
either lands.
