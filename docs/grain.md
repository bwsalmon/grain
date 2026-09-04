# Grains

> **Proposal.** Nothing here ships yet. `pkg/grain` carries the types, the
> controller's decision table and the wire format as compiling, tested Go.
> This document is what was decided; [`grain-options.md`](grain-options.md)
> is what was considered and why, including the paths not taken and the
> conditions that would reopen them.

**A grain is a polled MCP server that runs one agent in one sandbox.**

Everything below follows from that sentence. The agent talks to an
ordinary MCP server in its own container; that server answers what it can
itself and, for anything else, holds the call open and waits to be *asked*
for it rather than dialling anyone.

## What runs today

The agent CLI is a subprocess **on the controller**. Its MCP config points
at a forked `grain mcpserver -kontur-vm <vm>`, so every `read_file` is a
`docker exec <container> kontur exec` round trip into the sandbox VM. A
run's liveness is a goroutine's stack inside the daemon: `reconcileDispatch`
spawns `runOne`, which blocks through a VM boot, a token mint, a checkout,
an hour of agent, `ProcessResult` and a release.

Most of what is awkward in `pkg/orchestrator` follows from that one fact —
`runOne`'s comment about a setup failure stranding a live row, `InFlight`
and `drainInFlight`, `orphan.go` and `recover.go`, three separate
mechanisms for cancelling a run from outside it, and `recreate.go`'s
registry so an agent can ask for a sandbox it is not co-located with.

## What a grain is

A **container**: the agent CLI, a kontur VMM, and the guest VM that VMM
boots, with the shim as PID 1 holding them together.

```
┌─ grain container (one per run) ─────────────────────────────┐
│  grain run (PID 1)                                          │
│   ├─ kontur (VMM) ── cloud-hypervisor ──┐                   │
│   ├─ agent CLI (claude / agy / codex)   │                   │
│   └─ MCP server ─> /run/kontur/vsock.sock ┼─> guest VM      │ ← the sandbox
│                                          │   checkout,      │
│  holds: credential, prompt, setup,       │   builds, tests  │
│         tools, placements                │                  │
└─────────────────────────────────────────────────────────────┘
        ▲ docker exec / kubectl exec — the controller's only route in
```

The agent is in the container; the sandbox is the guest, one vsock hop
away. That boundary is the whole design.

### The credential boundary

The agent's own credential lives in the **container**, on the far side of
vsock from anything the repo's code can run. It is not in the sandbox, so
a build script or a test cannot read it.

This is better than today, not merely equivalent: one controller process
currently holds every run's secrets alongside the store, the GitHub app key
and the git proxy's signing key. Per-grain containers cut the blast radius
of any one compromise to one run.

The boundary comes from **where the agent runs**, not from anything in the
wire format. The residual: the agent process can read its own token and
could write it into the guest. That is unchanged from today, and is not
what the VM boundary defends against.

## Tools: six built in, the rest from an MCP server

Grain's own are six, every one about the sandbox:

| tool | served |
| --- | --- |
| `run_command`, `read_file`, `write_file`, `edit_file` | locally, over vsock |
| `recreate_sandbox` | locally — a kontur call, now that the VMM is the shim's own child |
| `status` | locally — writes `status.activity`, read off the record stream |

**Everything else comes from an ordinary MCP server the controller runs.**
The controller attaches to a grain by exec'ing `grain proxy`, and over that
connection it serves whatever `mcpServers` the deployment configured. The
shim merges those tools into its own `tools/list` and relays their calls.

`status` is the one escape hatch that becomes *fully* local: `update_status`
is an HTTP hop today to put a phrase on a task's row, and as a built-in it
is a file write that cannot fail and costs the agent nothing.

| tool | today | as a grain |
| --- | --- | --- |
| `run_command`, `read_file`, … | `docker exec` → `kontur exec` | built in — local, vsock |
| `recreate_sandbox` | MCP → daemon REST → registry → four `restore*` | built in — local kontur call |
| `update_status` → `status` | MCP → daemon REST → store write | built in — a local file write |
| `open_pull_request`, `pull_request_status`, `wait_for_checks` | daemon REST / controller's GitHub client | an upstream MCP server |
| `ask_question`, `request_secret` | deferred into the result | an upstream MCP server |
| `comment_on_issue`, `propose_task`, `add_review_comment` | deferred into the result | an upstream MCP server |

### Extending it

Somebody who wants an agent to be able to do something new writes **a plain
stdio MCP server** — the spec's own transport, any official SDK — and lists
it in the controller's config, in the shape every MCP client already takes:

```json
{
  "mcpServers": {
    "acme-tracker": { "command": "/usr/local/bin/acme-mcp",
                      "args": ["--task", "task-311", "--repo", "bwsalmon/grain"] }
  }
}
```

**That server is not aware of any of this.** It does not know a proxy, a
container or a VM is in the path. Per-run context reaches it as launch
arguments, because the controller starts **one instance per grain** — which
is how MCP servers are scoped anyway, the way a filesystem server is given
its root.

Name collisions across merged servers are handled by the convention grain
already implements: `pkg/mcp/toolnames.go`'s `mcp__<namespace>__<tool>`,
"the way a CLI that loaded it from an MCP config reports and admits it",
plus `AgyQualifiedToolName` for agy's single-underscore spelling of the
same thing.

This replaced a scheme where tool schemas were mounted into the container
as declarations. A server advertising its own `tools/list` is one source of
truth; a mounted schema is a second copy to keep in sync.

### Why the shim terminates the session rather than piping bytes

MCP is session-oriented: a client initializes once, and a dead transport
takes the whole session with it — clients generally mark such a server
failed rather than re-initializing. A grain that merely piped bytes to the
controller would therefore lose those tools **for the rest of the run** the
first time the connection dropped, an hour in and silently.

So the shim is the MCP server the agent talks to. It serves `initialize`
and a merged `tools/list`, answers its own six, and relays the rest. The
agent's connection is to the shim and the shim never closes it, so a drop
is something only the shim sees.

**While detached it holds.** In-flight and new upstream calls wait rather
than failing, so the agent goes on working with the sandbox tools — most of
what it does — and blocks only if it reaches for an upstream one. The
controller reattaches on the next tick that finds `upstream.attached`
false. If it never returns, `GRAIN_MAX_RUNTIME` ends the grain like any
other stall.

Two consequences worth naming rather than discovering:

- **A grain waits for its first attach before starting its agent**, saying
  `waiting for the controller to attach` in `activity` while it does.

  It waits rather than starting with only the built-ins because **an
  absent tool is not an error**. An agent never told `open_pull_request`
  exists does not call it and fail — it finishes its work, pushes a branch
  with `run_command`, and opens no pull request. The run looks fine, and
  nothing is wrong until somebody looks. Returning errors instead would
  need the shim to advertise tools it has not been told about, which means
  configuring it with their names and schemas — the mounted declarations
  this design just removed. MCP's `notifications/tools/list_changed` does
  not close the gap either: it is gated on a client capability, support
  across CLIs is uneven, and a client that ignores it loses those tools
  silently — the same failure with more machinery.

  It is barely a wait. The agent does not start until the guest has booted
  and `setup` has run, which is minutes; the attach lands milliseconds
  after create and runs concurrently with all of it. What the rule buys is
  that a genuinely absent controller fails **loudly** — the grain sits in
  `provisioning` until `ProvisionBudget`, with the phrase in its detail —
  rather than producing a run that quietly did less than it should have.

  A grain survives a controller dying *mid*-run on the list it cached;
  only the first attach is required.
- **An upstream server must tolerate seeing a call twice.** A connection
  can drop after the server acted and before its answer arrived, and held
  calls are replayed on reattach. Grain's own pull request path already
  does — `EnsurePullRequest` (`finish.go:937`) is find-or-create — and
  where that is not achievable the controller dedupes by call id.

**The container needs no daemon URL, no task ID and no bearer token.**
`agent.RunConfig.TaskID` exists solely as "the one fact a forked mcpserver
subprocess needs before it can ask the daemon to act on this run's
behalf" — that field goes, with `WithGrainServer` and the `-task` flag.

**Recreating a sandbox stops being a subsystem.** `SandboxRecreations`,
`sandboxRecreation`, `SandboxRebuilder`, `pkg/ui/sandbox_recreate.go` and
its route all go — roughly 900 lines with tests. The rebuild recipe
problem goes with them: `setMaterialized` exists so a rebuild replays
already-minted credentials rather than minting a second set behind a
single revoke, and with the material in the container next to the thing
being rebuilt, a rebuild is "fresh guest, replay the placements, re-run
setup."

Two more things move inside by the same rule: **`ConfigureGitCredentials`**
becomes an ordinary placement, and **`prepareCheckout`** (~500 lines of
`checkout.go`) becomes part of the setup script the controller composes.

## What a grain does not know

There is no task in its configuration, no repository, no branch, no git
credential field and no capability model. **A grain runs an agent in a
sandbox and knows nothing about why.** Everything task-shaped reaches it
in one of four shapes:

- **the prompt** (`/grain/prompt`), assembled by the controller from its
  store;
- **the setup script** (`/grain/setup`), which the controller composes —
  the clone included, since a clone is git commands in a guest;
- **a placement** (`/grain/placements/…`), which is where a credential the
  work needs goes, git's among them;
- **a tool on the controller's MCP server**, for anything the agent should
  be able to ask for beyond its own sandbox.

A shim that understood repositories would have to agree with the
controller about branch naming, proxy URLs, what to do with a half-made
checkout and what a task is — grain's whole task model, crossing an
interface between two separately released artifacts.
`TestGrainEnvCarriesNoTaskModel` asserts on the rendered environment that
none of it has crept back.

**`framework` is a name and a credential, nothing else.** How a CLI is
launched, which flags it takes, whether it needs a private HOME and *where
its credential goes* are facts about that binary, and the binary ships in
the sandbox image. Today the daemon owns all of it (`pkg/agent/claude`,
`/antigravity`, `/codex`, ~5,700 lines), in a different artifact from the
CLIs it describes.

## Configuration

Delivered before the container starts, in two halves: **scalars in the
environment, everything else as files.**

```sh
GRAIN_WIRE_VERSION=v1
GRAIN_FRAMEWORK=claude
GRAIN_MAX_RUNTIME=2h0m0s

# kontur's own, set by grain and never read back by it
CHV_CPUS=2  CHV_MEMORY_MB=8192  CHV_DISK_SIZE_MB=30720
```

```
/grain/credential                                0600
/grain/prompt                                    0644
/grain/setup                                     0755
/grain/placements/home/agent/.git-credentials    0600
```

There is **no configure step**: a container starts knowing what it is, so
there is no window between create and configure for a failure to fall
into, and no "not configured yet" for a poll to mean.

The file half is delivered by whatever the backend already has. On
Kubernetes a Secret or ConfigMap volume **is** this model —
`items: [{key, path, mode}]` gives files at chosen paths with chosen
modes, injected by the kubelet before the container starts, with only a
reference in the pod spec:

```yaml
env:
  - name: GRAIN_FRAMEWORK
    value: claude
volumes:
  - name: material
    secret:
      secretName: grain-task-311
      items:
        - { key: credential,      path: credential,                             mode: 0600 }
        - { key: git-credentials, path: placements/home/agent/.git-credentials, mode: 0600 }
```

**The placements tree is the mapping.** A placement bound for
`/home/agent/.netrc` is mounted at `/grain/placements/home/agent/.netrc`,
so nothing carries a manifest beside it and a Secret's own `items[].path`
says where a key lands in the guest. `PlacementPath` refuses anything not
absolute and in simplest form — containment, since `/a/../../etc/shadow`
under that root escapes the tree — and `GuestPath` re-checks on the way
out, because the shim walks a directory somebody else mounted.

**`/grain/setup` must never embed a credential.** The clone reaches the
proxy with a plain URL and git finds its token in the placement beside it.
`Spec.Redacted()` blanks material for logging and leaves setup alone
deliberately, since a failed run is diagnosed by reading exactly what its
setup tried to do.

**`CHV_*` are kontur's and grain never reads them back.** The shim starts
the VMM as a child and kontur parses its own configuration, so a `Shape`
passes straight through in kontur's vocabulary.

## Poll for state, logs for the trajectory

> **Snapshot state → exec poll. Append-only stream → container logs.**

Both are the controller reaching in; the grain never dials out.

**State rides the same stream.** The shim emits its whole `Status` as a
`kind: "status"` record, so `List` is served from two things a controller
reads anyway — the runtime's own container listing, for which grains exist
and whether each is running, and the log tail it already follows for the
trajectory. **In the steady state that is no exec per grain at all.**

A full snapshot rather than a delta, so this stays level-triggered — the
property `Reconcile` rests on — and absence stays meaningful: container
state comes from the listing, so "running but nothing recent on the
stream" is a wedged shim, which is a distinguishable and more informative
state than an exec that hangs.

Emitted **on change plus a slow heartbeat**, never on a fast fixed
interval: the kubelet rotates at 10 MB across 5 files, and status records
would otherwise eat the budget the trajectory needs.

`grain status` remains as the fallback — for when a stream has gone stale
and the controller wants a fresh answer rather than the last one a grain
chose to give. Worth being honest that it is less independent than it
looks on Kubernetes, where exec and logs both go through the API server
and largely fail together; it is genuinely a second route only under
docker.

A grain the controller cannot reach at all is `PhaseLost`, with a rule for
it; a grain that stopped *pushing* would be silence indistinguishable from
health.

**Trajectory.** The shim writes tagged records to the container's stdout
and the controller reads them back with `docker logs -f`. This is kontur's
own convention — `internal/hypervisor/args.go:108` routes the guest serial
console to stdio with `--serial tty`, "so it shows up under `kubectl
logs`". The runtime buffers, so a controller restarting mid-run resumes
from `--since`; nothing backpressures the agent; replay is the same call
as the tail.

**Logs carry a transcript, they never store one.** The kubelet rotates at
10 MB across 5 files by default, so the controller consumes the stream and
persists it — the record stays `Config.TranscriptDir`'s.

Three things fall out:

1. **Reattach stops being a special case.** Identity is derivable
   (`dispatch.RunID`), state lives in the container, and `List` runs every
   tick — so controller restart is the ordinary path. `orphan.go`,
   `recover.go`, `InFlight` and `drainInFlight` all go.
2. **Tool calls get an order of magnitude faster.** Per `read_file`, today
   is *fork docker CLI → dockerd RPC → `kontur exec` → vsock → guest*; in
   the container it is *fork `kontur exec` → vsock → guest*.
3. **The grain is the container.** Lifetime, identity and liveness are one
   thing. `Release` deletes it and the VMM and guest die with it.

## The wire

The transport is the container runtime, so each call is one process: argv
in, JSON on stdout, an exit code back. What has to stay stable is **a CLI,
not an RPC schema** — `pkg/grain`'s Go types are the controller-side
facade; the CLI is what crosses between the daemon binary and the sandbox
image.

```
grain run
        PID 1 and the image's entrypoint. Boots the VMM, waits for the
        guest, applies placements, runs /grain/setup, starts the agent
        named by GRAIN_FRAMEWORK with /grain/prompt, serving its own
        tools and forwarding the declared ones. Writes trajectory
        records to stdout. Does not exit until the grain is done.

grain proxy
        What the controller execs to attach. Dials SocketUpstream and
        pumps its own stdin and stdout across, so the supervisor gets a
        stream to the controller's MCP server. A socket rather than
        handing over a file descriptor, because `run` and `proxy` are
        separate processes and copying bytes needs no SCM_RIGHTS.

grain status                       > status.json
```

**Two verbs**: attach, and read the status when the record stream has gone
stale. Everything else a controller does to a grain is a container-runtime
operation — create it, list it, tail its logs, destroy it.

`run`'s stdout is the container log stream; a `status` started by `docker
exec` writes to whoever called it. Different streams, which reads as a
conflict and is not.

### Exit codes

| code | meaning | the controller |
| --- | --- | --- |
| 0 | ok | parses stdout |
| 1 | failed; stderr is the detail | reports it |
| 2 | unknown subcommand or flag | **version skew** — image predates this controller |
| 4 | unrecognised wire version | fails the run `setup-failed`, naming both |

The distinction not to lose is **exec-failed versus shim-failed**: `docker
exec` uses 125/126/127 for its own failures and errors when the container
is not running, as against propagating the command's code. The first means
`PhaseLost`; the second means the shim answered and said no.
`mcp.DockerExecRunner`'s `execFailedBeforeGuest` already draws that line.

### The upstream connection

`grain proxy` is one MCP stdio session with the **controller** as server
and the grain as client. It blocks for the life of the connection; the
controller reattaches on any tick that finds `upstream.attached` false,
which is both how a drop is repaired and how a freshly created grain gets
started at all.

It is the one thing here that is not poll-shaped, and it earns that. The
objections that ruled out held connections elsewhere were about what they
carried: push, where silence is ambiguous and edges get lost; the
trajectory, which needs buffering and replay. Tool calls are synchronous
request/response, low volume, and fail visibly — none of it transfers.
Direction is unchanged: the controller opens it, and the grain never dials
out. It rides exec, not the network, so it works in flat mode.

### Versioning

`version` is the wire format — `"v1"`, a Kubernetes-style grade-carrying
string — stamped on every document in both directions. A receiver that does
not recognise one **must refuse it and name both**, never interpret it on a
best effort: JSON ignores unknown fields, so silent misinterpretation is the
default failure otherwise. It is on every document rather than negotiated
once because of reattach — an upgraded daemon meets grains its predecessor
created, in both directions.

Before a grain exists, the image says what it can do:

| label | |
| --- | --- |
| `grain.wire-versions` | wire formats this image speaks, comma separated |
| `grain.frameworks` | agent profiles it carries |

Read once per image with `docker inspect`. That is when it is useful — to
refuse a task naming a framework the image lacks — and asking a grain
would require a grain to exist.

### `status.json`

One call, everything: the poll is the only read, so a field split out is a
second exec per grain per tick.

```json
{
  "version": "v1",
  "phase": "blocked",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
  "setup": { "exitCode": 0, "output": "9f3c1a2\n" },
  "upstream": { "attached": true, "pending": 1, "since": "2026-09-04T19:40:00Z" },
  "health": {
    "container": { "running": true },
    "guest": { "ready": true, "loadAverage": "0.41 0.30 0.22",
               "conntrackCount": 812, "conntrackMax": 262144 }
  },
  "seq": 4471
}
```

`phase` is one of `provisioning`, `running`, `blocked`, `succeeded`,
`failed`, `cancelled`, `lost`, `released`. `since` is when it was entered,
and every timeout the controller enforces is a subtraction against it.

**There is no id, and nothing echoed back.** The container is the identity:
a controller execs into one specific container, so the answer cannot be
ambiguous about whose it is.

**`upstream` is how a drop becomes visible.** `attached` false on a running
grain is one to reattach to; `pending` is how many calls the shim is
holding for a controller that is not there, which the phase alone does not
say.

`conntrackCount`/`conntrackMax` are there because of the network decision:
under NAT the guest's traffic fills a table in the pod's namespace that the
guest cannot see, and a full one drops packets, which inside the sandbox
reads as timeouts and hanging fetches.

Once terminal, `result` is set:

```json
{ "phase": "succeeded",
  "result": {
    "outcome": "succeeded",
    "pushed": { "branch": "grain/task-311", "head": "9f3c1a2" },
    "usage": { "turns": 34, "inputTokens": 812004, "wall": "22m14s" } } }
```

`pushed` is present even on a failed grain, deliberately: an agent that
commits, pushes and then runs out of turns did the work and only the ending
failed. Salvaging that branch is a special case in `runOne`'s error path
today; here it is a field the ordinary finish path reads.

### Stopping a grain

There is no cancel verb. **Stopping a grain is destroying it**: stopping a
container sends SIGTERM and waits out a grace period before SIGKILL, so
the shim — PID 1 — gets that window to stop the agent, write its `Result`,
and power the guest down. That is the whole of a graceful cancellation.

It is the pattern the stack already holds to: kontur's `ShutdownTimeout`
bounds how long the runtime waits for a guest to power off after SIGTERM,
and its manifest's `terminationGracePeriodSeconds` "must comfortably
exceed" it.

Being abrupt costs nothing today does not already cost —
`watchForTaskClosed` and `Pause.register` both just cancel the run's
context, killing the agent mid-turn — and a pushed branch survives a
SIGKILL, since `salvagePushedBranch` asks GitHub whether the branch is
there rather than asking the run.

**A grain must never be restarted.** A restarted one boots a fresh guest,
re-runs its setup and starts the agent again on the same prompt while the
controller still believes it is the same run: `seq` resets and the
trajectory interleaves two runs. Kubernetes needs `restartPolicy: Never`
said explicitly — kontur's own static pod manifest says `Always`
(`internal/staticpod/manifest.go:92`), which is right for a long-lived VM
and wrong for this. Docker's default is already correct.

### `/dev/termination-log`

On the way out the shim writes its `Result` to `/dev/termination-log` as
well as to its status. Kubernetes surfaces that file in
`.status.containerStatuses[].state.terminated.message`, so a finished
grain's outcome arrives in the same pod listing that enumerates it, with
no exec at all.

That covers the read that must not be missed: a grain that finished but
whose `status` exec fails is a run the controller cannot finish, and it
holds a slot until something notices. Pair it with
`terminationMessagePolicy: FallbackToLogsOnError` for a shim that died
before writing one. The cap is a few kilobytes, so a `Result` belongs
there and a trajectory does not; under docker nothing reads the file and
writing it costs a few hundred bytes.

### Trajectory records ← `docker logs`

One JSON object per line on the container's stdout.

```json
{"version":"v1","seq":41,"t":"…","src":"shim","kind":"status","data":{"version":"v1","phase":"running","activity":"running the test suite","seq":41,…}}
{"version":"v1","seq":42,"t":"…","src":"console","data":"[    0.512] EXT4-fs (vda): mounted"}
{"version":"v1","seq":43,"t":"…","src":"agent","kind":"tool_use","data":{"name":"run_command"}}
```

**stdout carries records and nothing else.** Three things share the stream
by design — the shim's narration, the agent's output, and the guest's
serial console — and the shim wraps all three rather than letting any
through raw. kontur routes the console to its own stdio (`--serial tty`,
"so it shows up under `kubectl logs`"), and as the shim's child that
output is the shim's to capture and re-emit. Wrapping is what makes a
console line addressable: a run killed by the provisioning budget can
quote the last few in its detail, which raw interleaved text could not
support.

Anything the shim wants to say to a human goes to **stderr**. Without that
rule a stray line — a library warning, a panic trace — is
indistinguishable from a damaged record, and "skip what does not parse"
stops being a rule about damage and becomes one about mixed content.

**File descriptors cannot do this job**, which is why `src` is in the
record. Docker tags each entry with its stream and its API can return them
separately; Kubernetes' pod log API strips it — the CRI log file carries
the stream per line and the API returns only the message. So fd-splitting
works under one backend and not the other.

`version` is on every line because a reader may join anywhere; a record
never spans lines, and an unparseable one is skipped, since the tail of a
rotated log routinely begins mid-line. `seq` is the cursor `status.seq`
reports and `Transcript` takes — a sequence rather than a byte offset,
because `docker logs` is addressed by time and line.

## The interface

```go
type Grains interface {
	Create(ctx context.Context, spec Spec) (Grain, error)
	List(ctx context.Context) ([]Status, error)
	Get(ctx context.Context, id ID) (Grain, error)
}

type Grain interface {
	ID() ID
	Observe(ctx context.Context) (Status, error)
	Attach(ctx context.Context) (io.ReadWriteCloser, error)
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)
	Release(ctx context.Context) error
}
```

Half of it never reaches the shim: `Create` is `konturctl vm create` with
the grain's environment and mount, `List` is `docker ps --filter
label=grain.id`, `Transcript` is `docker logs --since`, `Release` is
`docker rm -f` (and is also how a grain is cancelled). Two are actual shim
calls, matching the two verbs. A method that cannot be served by one subcommand or one
runtime operation has drifted from what the transport can do.

There is deliberately **no `Rebuild`**. Rebuilding the guest is internal to
the grain; the controller learns of it only as `Status.Rebuilds` going up,
and keeps the policy that needs a view the grain does not have.

## The controller

```go
func (c *Controller) Tick(ctx context.Context, now time.Time) error {
	fleet, _ := c.Grains.List(ctx)   // one call
	live, _ := c.Store.LiveRuns(ctx)

	for _, st := range fleet {
		for _, a := range grain.Reconcile(observed(st, live, now), c.Policy) {
			c.apply(ctx, a)
		}
	}
	for _, d := range dispatch.Cycle(ctx, c.Store, limits, now) {  // unchanged
		c.Grains.Create(ctx, c.specFor(ctx, d))
	}
	// then sync, schedule, releases, qualifications, branches, reviews — unchanged
}
```

`Reconcile` is pure — no store, no backend, no clock of its own — so the
whole per-grain policy is a table test. Its ordering is the decision:

| # | observed | store says | action |
| --- | --- | --- | --- |
| 1 | any phase | no live run | `release` |
| 1 | `released` | live | `fail(lost)` |
| 2 | `lost` (container gone) | live | `fail(lost)`, `release` |
| 3 | terminal | live | `finish`, `release` |
| 4 | `rebuilds > MaxRebuilds` | live | `fail(thrashing)`, `release` |
| 5 | any | task closed | `fail(cancelled)`, `release` |
| 5 | any | paused | `fail(cancelled)`, `release` |
| 6 | `provisioning`, over budget | live | `fail(setup-failed)`, `release` |
| 7 | `running` / `blocked` | live | `attach` if detached; mirror activity |

Row 5 is one tick, not two: signalling and then waiting for the next poll
to see a terminal phase was the only place this table needed a follow-up
round. The controller supplies the outcome rather than reading one back,
because SIGKILL may win.

Row 2 has no repair path, deliberately: a wedged *guest* never reaches the
controller, because the shim rebuilds it. `PhaseLost` means the whole grain
is gone.

The `pkg/orchestrator` equivalent is `runOne` plus `RunDispatch`, ~730
lines whose behaviour can only be observed by dispatching a real run.

### Who enforces a deadline

`GRAIN_MAX_RUNTIME` is decided by the controller and enforced by the grain,
which looks inconsistent beside `Policy.ProvisionBudget` and is not:

- **Before there is an agent, only the controller can act.** A grain wedged
  in provisioning is precisely the one that cannot report being wedged.
- **Once there is an agent, the grain can stop it depending on nobody.** A
  running agent is spending money, and money should not keep leaving while
  a controller is down.

One enforcement point, not two. `Config.MaxRunRuntime`'s own concern — a
stuck run "tying up its share of the concurrency limit" — is served anyway:
the grain goes terminal and the next poll frees the slot. A stopped run
reports `cancelled` with the limit named, which is what `run.go:1424`
already records.

**What it does not cover:** a controller dying five minutes into a two-hour
budget still leaves an hour fifty-five of spending. See
[`grain-options.md`](grain-options.md) for the lease that would bound that
and why it was declined.

## The network: NAT or flat, by what the container layer needs

> **Flat where nothing at the container layer needs network. NAT where
> something does.**

Flat stays kontur's default and is the better mode wherever it applies:
*zero* netfilter in the guest's path, the guest an ordinary endpoint with
the pod's own address and MAC, no state to exhaust. A VM whose container is
only a VMM wrapper should keep it, and nothing here asks those deployments
to change.

A grain needs NAT because it puts an agent in the container and that agent
needs the model API. Under flat, `internal/netshim/setup.go:29` — "the
splice steals the interface's ingress, so the namespace's own stack can
never receive a reply" — so the container can send and never receive.

NAT is a bridge plus masquerade in the pod netns with `eth0` unspliced. It
works in every environment with **no cluster provisioning**: the
requirement that looks like it should block on a managed cluster is writing
`net.ipv4.ip_forward` past Kubernetes' unsafe-sysctl gate, and netshim is
already `privileged: true` everywhere for an unrelated reason
(`gke-pod-exec-example.yaml:52` — the netlink library opens `/dev/net/tun`),
so it can write `/proc/sys` in the pod's own netns.

Two costs, paid only by deployments that select NAT:

1. **No infrastructure-level differentiation.** Agent and sandbox traffic
   share a source address. In-namespace nftables still enforces the split,
   and that enforcement is as strong as the VM boundary — but defence in
   depth and the audit trail are lost, and the separation becomes
   configured rather than structural. **The egress rules are part of the
   work**, not a follow-up.
2. **Conntrack as a new failure mode**, not a performance cost. Flat has
   zero netfilter in the path; NAT adds a finite stateful table, and a full
   one *drops*. Inside the guest that reads as timeouts and hanging
   fetches, from a table outside the VM that nothing the agent can run will
   show — so `GuestHealth` reports it, and the shim is expected to tell the
   agent when it is under pressure.

**NAT does not exist in kontur today** — `internal/cli/vm.go:245` rejects
`-net nat` outright. See "Asks of kontur".

## Costs and open items

1. **The trajectory rides the container log stream**, so it needs tagged
   records to share stdout with the guest console, and the controller must
   persist what it reads. It also puts prompts and model output wherever
   that deployment ships container logs — a per-deployment decision rather
   than a detail.
2. **grain needs its own image.** kontur's final stage is `FROM scratch`
   with `ENTRYPOINT ["/usr/local/bin/kontur"]` — a node-based CLI cannot
   run there. grain ships a sandbox image: a real base, `COPY --from=kontur`
   for the binaries and guest artifacts, the agent CLIs, and `grain run` as
   entrypoint. kontur keeps its scratch image.
3. **Verify kontur tolerates not being PID 1.** Its run mode currently boots
   the VMM as PID 1; as a child of the shim, signal forwarding and zombie
   reaping become the shim's job.
4. **The whole wire is a versioned contract**, not just the configuration.
5. **`HostGrains` is not optional.** Without a backend that runs the agent
   as a plain subprocess against a directory, every test needs a VM.
6. **grain's own `-kontur-net` handling is broken today.**
   `cmd/grain/daemon.go:310` offers the flag and `createArgs` passes it
   through, so `-kontur-net nat` would make every `vm create` fail against
   current kontur; `-kontur-base-ip`/`-kontur-base-port` feed flags kontur
   now ignores.

### Asks of kontur

- **Restore NAT as a selectable mode, beside flat rather than instead of
  it.** The one piece that cannot be built inside grain. Flat stays the
  default and unchanged. `-net` already exists and already rejects `nat`,
  so this restores meaning to a flag rather than adding one. Scope: bridge
  and tap (`ensureBridge`/`ensureTap` exist), the `ip_forward` write,
  nftables masquerade with idempotent teardown matching netshim's existing
  convergence discipline, and the egress rules.
- **`CHV_SETUP_SCRIPT_PATH`** — done, on kontur's branch.
- Promote `internal/execwire` and a thin client to `pkg/`, so a co-located
  shim can dial the guest without forking `kontur exec`.
- Document whether the VMM run mode is PID-1-agnostic (item 3 above).

## Migration

`pkg/model`, `pkg/dispatch`, `pkg/github`, `pkg/gitproxy`, `pkg/ui` and
every non-dispatch reconciler are untouched. This is a refactor of the run
path and the agent's location, not of the task model.

1. `pkg/grain` — the interface, `Reconcile`, and the wire format. **Done.**
2. `HostGrains` — agent as a local subprocess, no VM. Proves the interface
   and the decision table against the existing suite.
3. The controller loop — `Tick` over `List` + `Reconcile`, alongside the
   existing dispatch path behind a flag.
4. kontur's NAT mode (the blocking ask), then `grain run` and the sandbox
   image. Steps 1–3 do not wait on it.
5. `KonturGrains`.
6. Delete: `recreate.go`, `orphan.go`, `recover.go`, `InFlight`, `runOne`,
   `RunDispatch`'s sandbox half, `pkg/ui/sandbox_recreate.go`.
