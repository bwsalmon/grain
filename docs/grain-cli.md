# The grain wire contract

> **Proposal.** Companion to [`docs/grain.md`](grain.md), which argues for
> the design; this is the contract that design implies. `pkg/grain` carries
> the types, and `wire_test.go` pins every document below as literal JSON.

The controller reaches a grain over the container runtime — `docker exec`
/ `kubectl exec` for state, `docker logs` / `kubectl logs` for the
trajectory. There is no service, no port and no session: each call is one
process, with argv in, JSON on stdout and an exit code back.

So the interface that has to be stable is **a CLI, not an RPC schema**.
`pkg/grain`'s Go types are the controller-side facade; what is written
down here is what actually crosses between two separately released
artifacts — the daemon binary and the sandbox image.

## Versioning

`contract` is the wire version: one number on each document so the two
ends can detect that they disagree. `grain.Contract` is currently `1`, and
stamping it on every document in both directions is the whole of
negotiation.

It exists because the shim ships in the sandbox image and the controller
in the daemon binary — separately released, separately pinnable. JSON
ignores fields it does not recognise, so without a version the failure is
silent: an old shim handed a spec with a renamed `setup` runs no setup,
and the agent starts flailing in an empty guest.

It is on *every* document rather than negotiated once at create because of
reattach. An upgraded daemon meets grains its predecessor created, in both
directions, so a status written by an older shim and a signal written by a
newer controller each have to say what they are.

A receiver that does not recognise a document's contract **must refuse it
and name both versions**, never interpret it on a best effort: a refusal
costs one run and says exactly what is wrong, where a misread document is
a run that quietly does something else. `grain contract` exists so a
controller can ask before it knows whether the two ends agree, and it
answers with a list rather than a number — an image may speak several
versions through an upgrade, and a controller should take the highest it
also speaks rather than refuse a shim that could have served it.

## What is *not* a CLI call

Half of `pkg/grain`'s Go interface never reaches the shim:

| Go method | mechanism |
| --- | --- |
| `Grains.Create` | `konturctl vm create`, then `grain configure` |
| `Grains.List` | `docker ps --filter label=grain.id` |
| `Grains.Get` | name derivation — no call at all |
| `Grain.Transcript` | `docker logs --since` |
| `Grain.Release` | `docker rm -f` / delete the pod |

Only `Observe`, `Answer` and `Signal` are shim calls. A Go method that
cannot be served by one subcommand or one runtime operation is a sign the
interface has drifted from what the transport can do.

## The CLI

```
grain run
        PID 1 and the sandbox image's entrypoint. Waits for a spec, boots
        the VMM, waits for the guest, applies guest placements, clones,
        runs the repo's setup command, reports provisioned, waits for a
        prompt, runs the agent, reports terminal. Writes trajectory
        records to its own stdout. Does not exit until the grain is done.

grain configure                    < spec.json
grain status                                     > status.json
grain answer   --request <id>      < answer.json
grain signal   --id <id>           < signal.json
grain contract                                   > contract.json
```

**Payloads go on stdin, never argv.** An assembled prompt carries a task's
whole conversation, and `docker exec` argv crosses the runtime's API as
JSON — stdin has no size limit and no quoting hazard.

**`run`'s stdout and `status`'s stdout are different streams**, which
reads as a conflict and is not. `run` is PID 1, so its stdout is the
container log stream; a `status` started by `docker exec` writes to
whoever called it.

**`status` is a verb, not `cat`.** The supervisor writes
`/run/grain/status.json` and the subcommand could just print it, but going
through a verb keeps the on-disk layout out of the contract and gives the
output somewhere to be version-stamped.

### Exit codes

A CLI's exit codes are its error type, so they are part of the contract:

| code | meaning | the controller |
| --- | --- | --- |
| 0 | ok | parses stdout |
| 1 | failed; stderr is the detail | reports it |
| 2 | unknown subcommand or flag | **version skew** — image predates this controller |
| 3 | not configured yet | retries next tick; still `provisioning` |
| 4 | unrecognised contract version | fails the run `setup-failed`, naming both |

The distinction the controller must not lose is **exec-failed versus
shim-failed**: `docker exec` uses 125/126/127 for its own failures and
errors outright when the container is not running, as against propagating
the command's own code. The first class means the container is
unreachable, which is `PhaseLost`; the second means the shim answered and
said no. `mcp.DockerExecRunner`'s `execFailedBeforeGuest` already draws
this line, so there is a precedent to follow rather than a decision to
make.

### Delivery, since every call is a fresh process

`grain answer` and `grain signal` cannot hand anything to the supervisor
directly — different process — so they write into a spool directory it
watches, atomically (temp file, then rename), named by the caller's id.
The supervisor consumes each one and echoes its id in `status.consumed`;
the controller stops resending once it sees its own id there.

That is at-least-once with dedupe by id, and it is what makes `Answer` and
`Signal` idempotent as the Go interface promises. It is also why `signal`
takes an `--id` even though a signal replies to nothing.

## The documents

### `spec.json` → `grain configure`

Written once, at create. Four things:

```json
{
  "contract": 1,
  "framework": "claude",
  "shape": { "cpus": 2, "memoryMB": 8192, "diskGB": 30 },
  "setup": "git clone http://10.0.2.1:8080/bwsalmon/grain.git /w && cd /w && git checkout -b grain/task-311 && ./scripts/setup.sh && git rev-parse HEAD",
  "placements": [
    { "path": "/home/agent/.git-credentials", "content": "https://x:sbx_9f3c1a@10.0.2.1:8080", "mode": "0600" },
    { "path": "/home/debian/.gemini-api-key", "content": "AIza...", "mode": "0600" }
  ],
  "maxRuntime": "2h0m0s"
}
```

**What is not here is the point.** No task, no repository, no branch, no
git credential field and no capability model: a grain knows how to run an
agent in a sandbox and nothing about why. Everything task-shaped arrives
in one of three shapes — in the prompt (delivered by signal), in `setup`
(a script the controller composes, clone included), or in a `placement`
(which is where a credential goes, git's among them).

A shim that understood repositories would have to agree with the
controller about branch naming, proxy URLs, what to do with a half-made
checkout and what a task is — grain's whole task model, crossing an
interface between two separately released artifacts. A shim that runs a
script and places files has no opinions to keep in sync.
`wire_test.go`'s `TestSpecCarriesNoTaskModel` asserts on the marshalled
document that none of it has crept back.

**Placements are all guest-side, and carry no side.** Every capability
grain has that places anything places it in the sandbox —
`githubsandbox`, `gcpkey`, `geminikey`, all `model.SideSandbox` — and each
is material the *work* needs; `geminikey` mints a key for a task and names
its path in the prompt so the work can find it. `model.SideController`
exists and nothing produces one, and `run.go:1832` skips it, "not written
anywhere". A discriminator whose second value has never occurred is not
worth carrying across a versioned wire.

The agent's own credential is the case that looks like it wants the other
side and does not: it is deployment-wide, not per-run, so it reaches the
container as configuration at create and never travels in a Spec. The
sandbox cannot read it because of where the agent runs, not because of a
field here.

**`framework` is a name, not a configuration.** How that CLI is launched,
which flags it takes and whether it needs a private HOME are facts about
the binary, and the binary is in this image. `grain contract` reports
which profiles an image carries, so a task naming one it lacks fails at
create rather than inside a guest.

**`setup` is opaque to the shim**, which runs it and reports its exit code
and output without reading either. That is also how the two-phase start
gets its facts: the controller wrote the script, so it ends it with
whatever the prompt needs read back — `git rev-parse HEAD`, a log of what
earlier attempts pushed — and parses its own output.

**`maxRuntime` is the only limit, and the grain enforces it.** Turns are a
framework's own flag, and `Config.MaxAgentTurns`' doc comment already
concedes both frameworks default to no cap; rebuilds are
`Policy.MaxRebuilds`' alone. This one is decided by the controller and
enforced by the grain because a running agent is spending money, and money
should not keep leaving while a controller is down — see "Who enforces a
deadline" in `docs/grain.md` for why that is the opposite side from
`ProvisionBudget` on purpose, and for what it still does not cover. A
stopped run reports `cancelled` with the limit named.

Durations cross as strings. Go's own marshalling gives nanoseconds as an
integer, which is correct and unreadable, and these get read by people
during incidents.

### `status.json` ← `grain status`

One call, everything: the poll is the only read, so a field split out
would be a second exec per grain per tick.

```json
{
  "contract": 1,
  "phase": "blocked",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
  "requests": [
    { "id": "r-7", "kind": "open_pull_request", "raised": "2026-09-04T19:40:00Z",
      "payload": { "title": "Port the staleness check", "body": "..." } }
  ],
  "health": {
    "container": { "running": true },
    "guest": {
      "ready": true,
      "loadAverage": "0.41 0.30 0.22",
      "memoryUsedMB": 1204, "memoryTotalMB": 8192,
      "diskUsedMB": 6112, "diskTotalMB": 30720,
      "conntrackCount": 812, "conntrackMax": 262144
    }
  },
  "seq": 4471,
  "consumed": ["sig-19"]
}
```

**There is no id, and nothing else echoed back either.** The container is
the identity: a controller execs into one specific container to get a
status, so the answer cannot be ambiguous about whose it is, and a grain
telling you its own name would repeat what you had to know to ask. The
Go `Status` carries an `ID` a backend fills in from the container it read;
it never travels. No task, repo or framework comes back either — the
controller keys by that id and looks the rest up in its own store, which
is the one place any of it is true.

`phase` is one of `provisioning`, `provisioned`, `running`, `blocked`,
`succeeded`, `failed`, `cancelled`, `lost`, `released`. `since` is when it
was entered, and every timeout the controller enforces is a subtraction
against it — so a grain needs no clock agreement beyond that field.

`rebuilds` is how the controller sees self-repair it did not order: the
grain rebuilds its own guest, and the controller only decides when enough
is enough.

`conntrackCount`/`conntrackMax` are there because of the network decision.
Under NAT the guest's traffic fills a table in the pod's namespace that
the guest cannot see, and a full one drops packets — which inside the
sandbox reads as timeouts and hanging fetches. Reporting it is what lets
the agent be told rather than left to misdiagnose.

Once terminal, `result` is set:

```json
{
  "phase": "succeeded",
  "result": {
    "outcome": "succeeded",
    "pushed": { "branch": "grain/task-311", "head": "9f3c1a2" },
    "deferred": [ { "id": "q-2", "kind": "ask_question", "raised": "...",
                    "payload": { "question": "Should this also cover draft PRs?" } } ],
    "usage": { "turns": 34, "inputTokens": 812004, "outputTokens": 41221, "wall": "22m14s" }
  }
}
```

`pushed` is present even on a failed grain, and deliberately: an agent
that commits, pushes and then runs out of turns did the work, and only the
ending failed. Salvaging that branch is a special case in `runOne`'s error
path today; here it is a field the ordinary finish path reads.

### `answer.json` → `grain answer --request r-7`

```json
{ "contract": 1, "ok": true,
  "payload": { "number": 812, "url": "https://github.com/bwsalmon/grain/pull/812" } }
```

A refusal is an answer, not an omission:

```json
{ "contract": 1, "ok": false,
  "err": "this deployment has no GitHub credential that can open pull requests" }
```

Leaving a request unanswered blocks the agent until its deadline. Telling
it no is a turn it can act on.

### `signal.json` → `grain signal --id sig-20`

```json
{ "contract": 1, "kind": "prompt", "prompt": "You are working on task-311...\n" }
{ "contract": 1, "kind": "addenda", "addenda": ["Also fix the flake in TestMergeQueue."] }
{ "contract": 1, "kind": "cancel", "reason": "the task was closed" }
{ "contract": 1, "kind": "pause", "reason": "the deployment met its usage limit" }
```

One mechanism replacing three: `orchestrator`'s `addendaPoller`,
`watchForTaskClosed` and `Pause.register` all exist because a run in
flight has no address anything can deliver to.

### `contract.json` ← `grain contract`

```json
{ "contract": 1, "supported": [1],
  "frameworks": ["claude", "codex"],
  "build": "grain 2.4.1 (9f3c1a2)" }
```

`frameworks` is what this image can actually run. A controller checks it
before dispatching a task that names one, so the failure lands at create
with a name in it rather than inside a guest nobody is watching yet.

### Trajectory records ← `docker logs`

One JSON object per line on the container's stdout.

```json
{"v":1,"seq":41,"t":"2026-09-04T19:55:03.101Z","src":"shim","kind":"phase","data":{"phase":"provisioned"}}
{"v":1,"seq":42,"t":"2026-09-04T19:55:03.940Z","src":"console","data":"[    0.512] EXT4-fs (vda): mounted"}
{"v":1,"seq":43,"t":"2026-09-04T19:55:07.220Z","src":"agent","kind":"tool_use","data":{"name":"run_command"}}
```

**`src` is required and cannot be replaced by writing one source to
stderr**: kontur already routes the guest's serial console to this stream
(`internal/hypervisor/args.go`, `--serial tty`, "so it shows up under
`kubectl logs`"), and `kubectl logs` merges stdout and stderr — so
separating by file descriptor works under docker and nowhere else.

Sharing the stream with the console is what makes a failed boot legible: a
run killed by the provisioning budget can quote the last console lines in
its detail instead of reporting only that time ran out.

**`v` is on every line, not once at the top**, because a reader may join
the stream anywhere — there is no top to have read. For the same reason a
record never spans lines, and an unparseable line is skipped rather than
fatal: the tail of a rotated log routinely begins mid-line.

`seq` is monotonic across the grain's whole life and is the cursor
`status.seq` reports and `Transcript` takes. It is a sequence rather than
a byte offset because `docker logs` and `kubectl logs` are addressed by
time and line — a byte offset is not something either can seek to.

**Logs carry a transcript; they never store one.** The kubelet rotates at
10 MB across 5 files by default, so a long run's early trajectory ages
out. The controller consumes the stream and persists it; the record of
record stays `Config.TranscriptDir`'s.

## An optimisation not taken

`List` is `docker ps` plus a `status` exec per grain — N execs per tick,
which is nothing at the size grain runs at. Since the shim already emits
tagged records, **status snapshots could ride the same log stream**,
making the tail the steady-state path and `grain status` the fallback for
a grain gone quiet.

That would stay level-triggered — a full snapshot, not a delta — and
absence would stay detectable, since `docker ps` still reports container
state. So it does not reopen poll versus push. Written down as the place
to go if exec cost ever matters, rather than built now.
