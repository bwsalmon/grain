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

`docs/design.md` and `docs/roadmap.md` (item 8) are reconciled with this —
both describe the current architecture, not the sandbox-side one.
`docs/system-diagram.md` is **not** reconciled yet: its diagram still shows
`claude -p` running on the sandbox nodes, and its "credential ladder"
narrative is built around a sandbox-side login. That's a dedicated diagram
pass, not a quick text fix, and is listed under "Will bite in the first
week" below rather than done as part of this update.

This file is the current handoff: what's left, in the order worth doing it.

## Blocks a first real run

### 1. Real GitHub: a repo, a credential, and an audit against it

Every live verification to date, including the real-agent run above, runs
against a mock GitHub server, never the real API. Needed: a target repo, a
machine account or fine-grained PAT invited as a collaborator, its token in
`/data/secrets/github/`, the `credentials.json` mapping, the repo on
`repo-allowlist.json`, and `grain github audit` run against the real token —
its withheld-scope check has only ever seen scripted response shapes
(`docs/roadmap.md` item 7 has the detail on what the audit can and cannot
verify for fine-grained PATs). This is genuinely the next open question a
real run answers: nothing about real GitHub's actual API behavior or
quirks — rate limits, exact error-response shapes, auth edge cases — has
been exercised yet, mocked or otherwise.

### 2. Branch protection on the target repo

Manual, needs admin on that repo, and load-bearing rather than optional: no
direct pushes to the default branch from the agent credential, no
force-push, no deletion, PRs required. `docs/runbook.md`, "Adding or
reconfiguring a target repo," step 3 has the procedure. The design
deliberately enforces write safety at GitHub rather than by inspecting pack
files, so this is the control, not a backstop to one.

### 3. `/data` lives on the controller's root disk, and `recreate` deletes it

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

### 4. The resource budget predates the move, and concurrency is unverified

The controller is 1 vCPU / 4 GB (`Cluster.controller_cpus`/`controller_mem_mb`,
`grain/inventory.py`). It now hosts every concurrent `claude -p` *and* its
MCP server child, on top of the git proxy and one metadata server per
sandbox. `tests/loadtest.py`'s numbers — and `docs/design.md`'s resource
budget that quotes them — were measured with the agent work happening on
the sandboxes and the controller nearly idle. That budget no longer
describes this system. The one real dispatch done so far only ever
exercised one `claude -p`/MCP-server pair at a time; with `sandbox_count=2`,
two can now run concurrently on the controller as the same `grain-agent`
account, and nothing in `mcp_server.py` was written with that concurrency
in mind, because nothing forced the question yet.

**Verify:** re-run `python3 -m tests.loadtest` with real dispatches in
flight — ideally two concurrent ones — and watch the controller
specifically. Expect to raise `controller_cpus`/`controller_mem_mb`, which
on a 4-vCPU host means revisiting the sandbox count too.

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

1. **Item 3**, first and cheaply (the guardrail form, if the disk work is
   not happening this session) — it is the only irrecoverable failure here.
2. **Item 4**, once a real target repo exists to dispatch against for real —
   the measurement needs real dispatches in flight, ideally concurrent ones.
3. **Items 1 and 2**, which need a real repo and admin on it.

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
