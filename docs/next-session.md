# Next session: what stands between here and using grain for real

The previous version of this file named a single blocker: a real dispatched
`claude -p` could clone and commit inside its sandbox but could not `git
push`, because the push needed a sandbox network-domain approval a headless
run has no way to answer. **That blocker is gone** — not solved on its own
terms, but removed by the redesign that followed it. `claude -p` no longer
runs in the sandbox at all. It runs on the controller as `grain-agent`,
with its native tool roster emptied (`--tools ""`) and replaced by the four
MCP tools in `grain/automation/mcp_server.py`, which reach the assigned
sandbox over SSH. See `grain/automation/dispatch.py`'s module docstring for
the live findings that forced the move, and the README's "Where the agent
runs, and why it moved."

This file is the current handoff: what's left, in the order worth doing it.

> **Note on the rest of `docs/`.** `design.md`, `system-diagram.md`,
> `roadmap.md` (item 8 especially), and `runbook.md` all still describe
> `claude -p` running on the sandbox with a login credential there. They are
> accurate on everything else. Reconciling them is worth its own pass and is
> listed under "Will bite in the first week" below, not as a blocker.

## Blocks a first real run

### 1. No real agent has ever completed a run through the four MCP tools

Every live verification to date substitutes a *scripted* stand-in for
`claude` (`tests/test_live_issue_to_pr.py`'s `_install_fake_claude`, three
variants). What that proves is the mechanism: dispatch, the real clone
through the real proxy, real git state at sweep time, PR creation, and both
requeue paths. What it structurally cannot prove is how a real session
behaves when its entire native roster is gone and `run_command`/`read_file`/
`edit_file`/`write_file` against a remote workspace are all it has.

Open questions a first real run answers: does the agent work comfortably
through a tool surface with no `Glob`/`Grep` (it has `run_command`, so
`rg`/`find` are reachable, but it was never trained to reach for them that
way); is `read_file`'s `cat -n` mirror close enough that `edit_file`'s
exact-string matching lands; does it respect the "push with exactly this
command" instruction in `dispatch.py`'s `_prompt`.

**Verify:** one real dispatch against the mock-GitHub rig (see "Reproduce"
below), then read the transcript at
`/data/state/automation/units/grain-task-<sandbox>/transcript.jsonl` — or
`grain sessions browse` — rather than only checking whether a PR appeared.

### 2. The controller-side Claude credential path is unverified

Worth separating from item 1 because it is concrete and checkable.
`configure_claude_credentials` (`grain/automation/configure.py`) writes
`/home/grain-agent/.claude/.credentials.json`, and `_start_task`
(`dispatch.py`) starts the unit with no environment at all — so that file is
the *only* way the dispatched agent authenticates. Nothing anywhere in
`grain/` sets `CLAUDE_CODE_OAUTH_TOKEN`.

The one live run that ever had a real login did **not** use that path: a
`claude setup-token` value is not the `.credentials.json` shape, so it was
injected as an env var via `systemctl set-environment` on each *sandbox*.
That path no longer exists. So the credential mechanism the code actually
ships has never been exercised against a real login.

Two things to establish:

- What a real login writes, and whether `--claude-credentials-file` accepts
  it as-is. If the practical credential is a `setup-token`-shaped value,
  `dispatch.py` needs a `--setenv=` on the `systemd-run` call and
  `configure.py` needs somewhere to put it — neither exists today.
- **Concurrent refresh.** With `sandbox_count=2`, two `claude -p` processes
  run as the same `grain-agent` user against the same credentials file, and
  OAuth refresh writes that file back. This was explicitly sidestepped last
  time (the env var avoided it) and is now on the main path. Whether
  Claude Code locks that write is unknown here.

### 3. Real GitHub: a repo, a credential, and an audit against it

Everything to date runs against `RealGitHubMock`. Needed: a target repo, a
machine account or fine-grained PAT invited as a collaborator, its token in
`/data/secrets/github/`, the `credentials.json` mapping, the repo on
`repo-allowlist.json`, and `grain github audit` run against the real token —
its withheld-scope check has only ever seen scripted response shapes
(`docs/roadmap.md` item 7 has the detail on what the audit can and cannot
verify for fine-grained PATs).

### 4. Branch protection on the target repo

Manual, needs admin on that repo, and load-bearing rather than optional: no
direct pushes to the default branch from the agent credential, no
force-push, no deletion, PRs required. `docs/runbook.md`, "Adding or
reconfiguring a target repo," step 3 has the procedure. The design
deliberately enforces write safety at GitHub rather than by inspecting pack
files, so this is the control, not a backstop to one.

### 5. `/data` lives on the controller's root disk, and `recreate` deletes it

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
- **A guardrail**: make `recreate` refuse the controller without an explicit
  flag, and say why. Cheap, and it removes the sharp edge even before the
  disk work lands.

### 6. The resource budget predates the move

The controller is 1 vCPU / 4 GB (`Cluster.controller_cpus`/`controller_mem_mb`,
`grain/inventory.py`). It now hosts every concurrent `claude -p` *and* its
MCP server child, on top of the git proxy and one metadata server per
sandbox. `tests/loadtest.py`'s numbers — and `docs/design.md`'s resource
budget that quotes them — were measured with the agent work happening on
the sandboxes and the controller nearly idle. That budget no longer
describes this system.

**Verify:** re-run `python3 -m tests.loadtest` with real dispatches in
flight, and watch the controller specifically. Expect to raise
`controller_cpus`/`controller_mem_mb`, which on a 4-vCPU host means
revisiting the sandbox count too.

## Will bite in the first week

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
- **The only spend control is `runs_per_hour: 10`.** No per-run turn or cost
  cap; a looping run burns until `max_runtime_minutes`. The `result` event
  at the end of each transcript carries `total_cost_usd`/`num_turns` — a
  cheap first version is to read it in `capture.py` and audit-log it.
- **`docs/` still describes the pre-move architecture.** See the note at the
  top of this file. `roadmap.md` item 8's "Update" section is the worst of
  it: the code's own docstrings cite it as the authority for the *new*
  design while it describes the *old* one.
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

1. **Item 5**, first and cheaply (the guardrail form, if the disk work is
   not happening this session) — it is the only irrecoverable failure here.
2. **Items 2 and 1 together**, on the mock-GitHub rig: place a real login,
   dispatch one real issue, read the transcript. No real repo or GitHub
   credential needed, so this is the cheapest way to answer the biggest
   unknown.
3. **Item 6**, once real dispatches are running — the measurement needs
   them.
4. **Items 3 and 4**, which need a real repo and admin on it.

## Reproduce / verify

The mock GitHub server the earlier live runs used was a scratch script, not
checked in; rebuilding it is mechanical — `tests/test_live_issue_to_pr.py`'s
`RealGitHubMock` (REST endpoints) plus `_GitBackendHandler` (a `git
http-backend` CGI) combined into one process on one port, on the host's own
bridge address.

```sh
sudo python3 -m grain.cli --image /var/lib/grain/images/debian-12.qcow2 \
  host bootstrap --repo <owner>/<repo> --github-token-file - \
  --github-host <mock-host>:<port> --git-forward-host <mock-host>:<port> \
  --github-insecure-http \
  --claude-credentials-file <path>  <<< "mock-token"

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
