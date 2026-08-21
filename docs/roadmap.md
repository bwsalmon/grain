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

- [ ] Done

The most obvious gap: a sandbox pushes its branch through the git proxy,
but nothing opens the PR — sandboxes hold no GitHub API access by design
(the split surface, `docs/design.md` "GitHub access"). `core.py`'s sweep
already detects a successfully-finished run; extend that path (or a
sibling step) to open a PR via `GitHubClient`, using the same credential
selection `CredentialSet` already does for issue labels. Needs a design
decision on how the controller knows what branch/commit the sandbox
pushed — worth resolving before coding, not assuming.

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

## 4. GCP metadata server

- [ ] Done

`docs/design.md` step 7: one `gce_metadata_server` instance per sandbox,
serving impersonated tokens for a narrow second service account.
`inventory.py` and `net_linux.py` already reserve the ports and DNAT rules
for this (`Cluster.metadata_port`, the anycast DNAT in
`render_ruleset`); nothing serves them yet.

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
