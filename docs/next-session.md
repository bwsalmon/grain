# Next session: what stands between here and using grain for real

The previous version of this file named two blockers: a real dispatched
`claude -p` could clone and commit inside its sandbox but could not `git
push`, and the controller-side Claude credential path (a
`.claude/.credentials.json` file, `configure_claude_credentials`) had never
been exercised against a real login. **Both are resolved, verified live,
not just reasoned through:**

- `claude -p` no longer runs in the sandbox at all. It runs on the
  controller as `grain-agent`, with its native tool roster reduced to just
  `Task` (`--tools Task`) and replaced by the four MCP tools in
  `grain/automation/mcp_server.py`, which reach the assigned sandbox over
  SSH. See `grain/automation/dispatch.py`'s module docstring for the live
  findings that forced the move, and `docs/design.md`'s "Final choice: no
  credential in the sandbox at all."
- The credential path changed shape: `configure_claude_token`
  (`grain/automation/configure.py`) places a bare `claude setup-token`
  value — deliberately kept separate from any operator's own `claude
  login` session — at a mode-600 path `grain-agent` owns, read into
  `CLAUDE_CODE_OAUTH_TOKEN` at runtime by `dispatch.py`'s own unit script.
  `--claude-token-file` replaces `--claude-credentials-file` throughout.
  A real `grain host bootstrap`, with a real token from a real `claude
  setup-token`, produced a real dispatch: a real edit, a real commit, a
  real push, a real PR, zero permission denials in the transcript. Two
  more bugs surfaced only by running it for real and are already fixed —
  see `docs/roadmap.md` item 8's second "Update" for both.

A third blocker — everything above had only ever run against a mock GitHub
server — is now resolved too. A real fine-grained PAT (`bwsalmon/test1`,
scoped to Contents/Issues/Pull requests read+write) drove a real end-to-end
run: real issue #1, a controller-side `claude -p` whose advertised tool
roster was confirmed to be exactly `Task` plus the four
`mcp__grain-sandbox__*` tools (no native `Bash`/`Edit`/`Write`), a real
edit/commit/`git push` through the proxy to real GitHub, and a real PR
(bwsalmon/test1#2) opened by the sweep's own `branch_exists`/
`create_pull_request` path — not the agent's claim about what it pushed.
`grain github audit` against that same PAT correctly reported it
"unverifiable" (fine-grained PATs expose no scopes header via GitHub's API —
documented limitation, not a bug).

That run surfaced two real gaps, neither of which blocked it but both worth
fixing before reconfiguring a live deployment becomes routine. **Both are
now fixed:**

- **`controller configure` doesn't restart the git proxy, and the proxy
  caches `git_forward_host` at process startup** (`build_proxy` in
  `grain/proxy/server.py` reads `automation.json` once, at boot). Pointing a
  live deployment at a new repo/host with `controller configure` silently
  leaves the proxy forwarding to whatever host it started with — the first
  real dispatch here failed with a proxied 500 because the proxy was still
  targeting a mock server torn down earlier in the same session. **Fixed**:
  `cmd_controller_configure` (`grain/cli.py`) now restarts
  `grain-git-proxy.service` right after writing `automation.json` —
  harmless if the service isn't running yet, since `systemctl restart` on a
  stopped-but-installed unit just starts it. Covered by
  `tests/test_cli.py::test_controller_configure_restarts_the_git_proxy_so_it_picks_up_the_new_config`.
- **A stale `AutomationState` assignment crashes `run_once` with an
  uncaught 404 when the target repo changes out from under it.**
  `core.py`'s `_sweep()` → `_requeue()` calls `self.github.add_label(...)`
  unconditionally on a leftover assignment; if that assignment's issue
  number doesn't exist in the newly-configured repo, `GitHubError` propagates
  uncaught and `run_once` exits nonzero having done nothing else either. Hit
  here after reconfiguring from a mock repo to `bwsalmon/test1` while a
  `sandbox-0 → issue #201` assignment from the earlier mock test was still in
  `/data/state/automation/state.json`. Worked around by hand-clearing the
  state file at the time. **Fixed**: `_requeue` now catches `GitHubError`
  from `remove_label`/`add_label`, logs and moves on for a 404 specifically
  (a stale assignment, not a real failure), and still re-raises anything
  else (a genuine 5xx or auth error). Covered by
  `tests/test_automation_core.py::test_a_requeue_tolerates_a_404_from_add_label_for_a_stale_assignment`
  and `::test_a_requeue_still_raises_a_non_404_github_error`.
- **`AutomationState` was only ever written to disk once per `run_once`
  call, after everything else finished** (`cli.py`'s
  `cmd_automation_run_once`) — not incrementally as `core.py` mutated it
  in memory (`state.assign()` in `_dispatch`, `state.release()` inside
  `sweeper.py`'s `sweep()`). The controller VM can be restarted or
  recreated at any moment, including mid-`run_once` — precisely the
  "host is stopped, or a run dies mid-flight" case the stranded-work
  sweeper (docs/design.md) exists for — and a crash between an in-memory
  `state.assign()` and that one end-of-run save was invisible to every
  recovery path: `_dispatch` had already removed the trigger label (a
  real, already-committed GitHub side effect) before the matching
  assignment was ever written to disk, so the issue would never again
  surface in `_dispatch`'s own poll (no longer trigger-labelled) *and*
  the sweeper had no assignment on disk to find it stranded with either —
  a task lost for good, silently, with no audit trail. Found by inspection
  (bwsalmon/agents#51), not live — worth calling out as a gap in the
  "restarting the VM doesn't strand tasks" story despite the sweeper
  itself being solid. **Fixed**: `Orchestrator` gained a `state_path`
  field and a `_save_state()` helper; `core.py` now saves immediately
  after each state mutation and *before* the GitHub call that depends on
  it becoming irreversible (`_dispatch`'s `state.assign()` before the
  trigger-label removal, `_sweep`'s `sweep()` call — which already
  released every finished/stranded slot in memory — before any of its
  outcomes get processed, plus the smaller `record_pending_question`/
  `clear_pending_question` sites). `cli.py`'s final `state.save()` stays
  as a redundant safety net; `--dry-run` still never touches the real
  file (`state_path=None` in that case). Covered by
  `tests/test_automation_core.py::test_dispatch_persists_the_assignment_before_the_trigger_label_comes_off`,
  `::test_a_task_stranded_by_a_controller_crash_mid_dispatch_is_recovered_on_restart`,
  and `::test_a_sweep_release_is_persisted_before_the_pr_is_opened`.

`docs/design.md` and `docs/roadmap.md` (item 8) are reconciled with this —
both describe the current architecture, not the sandbox-side one.
`docs/system-diagram.md` is **not** reconciled yet: its diagram still shows
`claude -p` running on the sandbox nodes, and its "credential ladder"
narrative is built around a sandbox-side login. That's a dedicated diagram
pass, not a quick text fix, and is listed under "Will bite in the first
week" below rather than done as part of this update.

**Since then, the intake shape changed** (docs/roadmap.md item 15): a
deployment now polls one **task repo** — a queue of issues for the agent
set — and each task names the **target repo** it is for with a `/repo
owner/name` line in its text (`grain/automation/directives.py`; `/pr N`
continues an existing PR, `/base` overrides the PR base). Labels, questions
and replies stay on the task issue; the clone, the push and the PR happen
in the target repo, which must be on the same `repo-allowlist.json` the git
proxy enforces. A task whose directive is missing, malformed or off-list is
*parked* (comment + awaiting-reply label) rather than retried blindly, and
a maintainer's reply can carry the correction. Config renamed accordingly
(`task_owner`/`task_repo`, plus an optional `default_target_repo`), with
the legacy `owner`/`repo` keys still accepted on load — an existing `/data`
needs no edit, and a single-repo deployment (`--task-repo X` with no
`--target-repo`) behaves exactly as before with no directive written
anywhere. **This has not been exercised live yet**: everything below about
running against a real repo still applies, and the first real multi-repo
run is the obvious thing to do next.

This file is the current handoff: what's left, in the order worth doing it.

## Blocks a first real run

### 1. Branch protection on the target repo

Manual, needs admin on that repo, and load-bearing rather than optional: no
direct pushes to the default branch from the agent credential, no
force-push, no deletion, PRs required. `docs/runbook.md`, "Adding or
reconfiguring a target repo," step 3 has the procedure. The design
deliberately enforces write safety at GitHub rather than by inspecting pack
files, so this is the control, not a backstop to one.

### 2. `/data` lives on the controller's root disk, and `recreate` deletes it

`provision/controller.sh` says plainly that `/data` is *expected* to be a
separate persistent disk, and that the libvirt adapter has no notion of one,
so today it is simply a directory on the controller's only qcow2.
Meanwhile `LibvirtAdapter.recreate` (`grain/adapter/base.py:115`) is
destroy-then-create, and `destroy` unlinks that qcow2
(`grain/adapter/libvirt.py:281-283`).

So `grain host recreate controller` — which the runbook and README both
describe as routine maintenance — destroys every credential and all
automation state, silently. `grain host recreate all` is reachable too.
This is the only item on this list that can lose something irrecoverable,
which is why it is worth doing first even though it is not the most
interesting.

Two ways to close it, either acceptable:

- **Adapter work**: a second attached disk per VM, mounted at `/data`,
  surviving destroy. This is what `provision/controller.sh` says to fix.
  **Not done** — this is still the real fix and still open.
- **A guardrail**: make `recreate` refuse the controller without an explicit
  flag, and say why. Cheap, and it removes the sharp edge even before the
  disk work lands. **Done**: `grain host recreate controller` (or `all`)
  now refuses with a clear message unless `--i-know-this-deletes-data` is
  passed (`_check_controller_recreate` in `grain/cli.py`, checked before the
  adapter is even built). `docs/runbook.md`'s controller-SSH-rotation
  example is updated to pass the flag; `tests/test_cli.py` covers refuse/
  allow for both `controller` and `all`, and that `sandboxes` needs no flag.

### 3. The resource budget predates the move — now verified live, concurrency included

**Resolved.** Two real, concurrent dispatches ran to completion on the
default 1 vCPU / 4 GB controller (`Cluster.controller_cpus`/
`controller_mem_mb`, `grain/inventory.py`) against a real `bwsalmon/test1`
deployment — not a toy task either: one scaffolded a full kubebuilder-based
Kubernetes controller from scratch (installing Go/kubectl/kubebuilder,
iterating through real build/toolchain issues, running `make test`), the
other a comparable-sized task. Both finished successfully (100–101 turns,
~$1.85 each, `is_error: false`) and both opened real PRs.

Peak memory across the whole ~32-minute concurrent run: **~984 MB of 3.9 GB
total** — comfortable headroom, not a squeeze. Load average never exceeded
~0.15 on the single vCPU; both `claude -p` sessions spend most of their time
blocked on API round-trips, not burning CPU. Back down to ~350 MB the moment
both sandboxes freed. **No evidence the 1 vCPU/4 GB budget needs raising**
at this concurrency level (two sandboxes) — revisit only if running more
than two concurrently, or if a future task turns out to be genuinely
CPU-bound on the controller side (unlikely, since the actual work happens
on the sandbox over SSH).

**A real bug surfaced getting to this test, not from it, but the same class
already flagged above for `controller configure`, and now fixed:** adding a
second sandbox to an already-bootstrapped deployment
(`grain host bootstrap --sandboxes 2` against a deployment that started with
`--sandboxes 1`) minted the new sandbox's git-proxy token but did **not**
restart the already-running `grain-git-proxy.service` — stage 10 was
`systemctl enable --now`, a no-op if the unit is already active. The proxy
process only ever loads `sandbox-tokens.json` once at startup (same caching
shape as `automation.json`'s `git_forward_host`), so the new sandbox's token
was invisible to it until a manual restart. Symptom hit live: every
`run_once` that tried to dispatch to the new sandbox crashed uncaught
(`fatal: Authentication failed` cloning through the proxy → `CommandError`
propagating out of `_dispatch`, killing that cycle's `run_once` entirely)
until `grain-git-proxy.service` was restarted by hand. **Fixed**: stage 10
now runs `enable` (persist across reboots) and `restart` (unconditional,
not `--now`) as two separate calls — `restart` starts a stopped unit exactly
like `--now` would, and reloads an already-running one either way. Covered
by `tests/test_bootstrap.py::test_stage_10_restarts_the_proxy_not_just_enables_it`.

**Also fixed, unrelated to the sandbox-count bug above:** `_dispatch` had no
try/except around `dispatch()`/`dispatch_pr()` at all — a `CommandError`
from *any* cause partway through starting a unit (not just the
proxy-restart case above) crashed the whole `run_once` uncaught, taking down
every other candidate in that cycle's queue too, not just the one that
failed. **Fixed**: `_dispatch` now catches `CommandError` around the
`dispatch()`/`dispatch_pr()` call specifically, logs "dispatch failed" to
the audit trail, and `continue`s to the next queued candidate — the failed
sandbox is never assigned and the issue's labels are left untouched, so both
are simply retried on a later cycle, the same "log and move on" discipline
`_requeue`/`_finish_question` already apply to a GitHub-side 404. Any
non-`CommandError` exception still propagates (a real bug, not dispatch's
one expected failure mode). Covered by
`tests/test_automation_core.py::test_a_dispatch_failure_does_not_crash_the_rest_of_the_cycle`
and `::test_a_non_command_error_from_dispatch_still_raises`.

## Will bite in the first week

- **`--dry-run` prints a credential-writing command's real stdin in full.**
  `DryRunRunner` (`grain/run.py`) echoes the exact stdin it would have piped
  in, which is exactly the stdin-not-argv channel
  `configure_github_credential`/`configure_claude_token` use to keep
  secrets out of argv/`ps` — so `grain --dry-run ... host bootstrap
  --github-token-file ... --claude-token-file ...` (or `controller
  configure` with either flag) prints the real GitHub token and Claude
  OAuth token in plaintext to whatever captured that output. Found live,
  this session, previewing a real bootstrap command before running it for
  real — both tokens ended up exposed in a place neither should have been.
  `--dry-run` is safe for read-only inspection (`host rules`, `host
  status`) but not for anything that carries a real secret over stdin.
  Fix: redact stdin in `DryRunRunner`'s echo (print a placeholder like
  `<stdin: N bytes>` instead of the literal content), or have the
  credential-writing call sites pass a marker `DryRunRunner` recognizes and
  suppresses.
- **No revocation path for a sandbox token.** `SandboxTokenStore.rotate`
  (`grain/proxy/tokens.py`) is called by nothing — not `recreate`, not any
  CLI verb — and the proxy loads `sandbox-tokens.json` once at startup.
  Killing a leaked token is a hand edit plus a service restart. (What that
  token grants, if leaked: full proxied fetch **and push** to every
  allow-listed repo, from anywhere that can reach `10.100.0.2:8080`. It is
  useless from outside the subnet and entirely sufficient from inside it.)
- **A wedged run holds a sandbox for `max_runtime_minutes`** — 120 by
  default. `sweeper.py` cannot tell slow from stuck, so half the pool can be
  unavailable for two hours. The manual fix (`systemctl stop
  grain-task-<sandbox>`, on the **controller** now) is in the README.
- **Nothing acts on health, and nothing schedules recreate.** Both
  `grain host health` and the sweeper's own check only report; a degraded
  sandbox keeps receiving dispatches. Recreate is the fix for a filling disk
  and the deploy path for image updates, and nothing runs it on a cadence.
- **The only spend control is `runs_per_hour: 60`.** No per-run turn or cost
  cap; a looping run burns until `max_runtime_minutes`. The `result` event
  at the end of each transcript carries `total_cost_usd`/`num_turns` — a
  cheap first version is to read it in `capture.py` and audit-log it.
- **`docs/system-diagram.md` still describes the pre-move architecture** —
  see the note at the top of this file. `design.md` and `roadmap.md` are
  already reconciled; this is the one diagram/table-heavy doc still
  showing `claude -p` on the sandbox nodes and a sandbox-side credential
  ladder.
- **The proxy's audit attribution is a bearer-token claim, not a network
  fact.** `GitProxy.handle` (`grain/proxy/core.py`) authenticates on the
  token alone; the sandbox name it yields is used only for the audit line.
  The anti-spoofing rules already make a sandbox's source address
  unforgeable, so authenticating by source address — the way the metadata
  server already does — would both remove the last secret from the sandbox
  filesystem and make attribution real.

## Accepted limits, listed so they are not rediscovered as bugs

- `grain host egress allowlist` is deny-all, not an allowlist: the rendered
  rule is a comment and the masquerade rule is dropped
  (`grain/adapter/net_linux.py`). There is no proxy shipped to be the "reach
  the world via a proxy" it names, so egress is effectively all-or-nothing.
- The host's own INPUT chain is deliberately unmanaged, so a sandbox can
  reach anything listening on the host's bridge address.
  `render_host_input_rules` exists; nothing applies it.
- GCP token minting has never run against a real project.
- Sequential tasks share a long-lived sandbox: task B inherits task A's
  filesystem. `ensure_workspace` resets the workspace, not the machine.

## Suggested order

1. ~~**Item 2**, first and cheaply (the guardrail form, if the disk work is
   not happening this session) — it is the only irrecoverable failure
   here.~~ **Done** — see item 2 above. The real fix (a second disk for
   `/data`) is still open.
2. ~~**Item 3**, now that a real target repo exists to dispatch against for
   real — the measurement needs real dispatches in flight, ideally
   concurrent ones.~~ **Done** — see item 3 above. Two new bugs surfaced
   doing it (proxy restart on sandbox-count change, `_dispatch`'s missing
   crash tolerance), both still open.
3. **Item 1**, which needs admin on the target repo.

## Reproduce / verify

The mock GitHub server the earlier live runs used was a scratch script, not
checked in; rebuilding it is mechanical — `tests/test_live_issue_to_pr.py`'s
`RealGitHubMock` (REST endpoints) plus `_GitBackendHandler` (a `git
http-backend` CGI) combined into one process on one port, on the host's own
bridge address.

```sh
sudo python3 -m grain.cli --image /var/lib/grain/images/debian-12.qcow2 \
  host bootstrap --task-repo <owner>/<tasks> --target-repo <owner>/<repo> \
  --github-token-file - \
  --github-host <mock-host>:<port> --git-forward-host <mock-host>:<port> \
  --github-insecure-http \
  --claude-token-file <path>  <<< "mock-token"

ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  'cd /opt/grain && sudo python3 -m grain.cli automation run-once'
```

Then read the run, not just its outcome:

```sh
ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  sudo cat /data/state/automation/units/grain-task-sandbox-0/transcript.jsonl
```

Two operational notes worth keeping from the previous version of this file,
both learned the hard way:

- **Do not run an unscoped `python3 -m pytest` on a host with a live
  cluster up.** The live suites default to the same VM names a real
  deployment uses (`controller`, `sandbox-0`) and will destroy it. This
  happened twice in one session. Scope to specific files, or stop the
  cluster first.
- **Stop `grain-automation.timer` before walking away** from a deployment
  with a known-failing dispatch — otherwise it retries the same failure
  every two minutes indefinitely.

```sh
ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  sudo systemctl stop grain-automation.timer
```
