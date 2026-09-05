# Grains

> **Proposal.** Nothing here ships yet. `pkg/grain` carries the types, the
> controller's decision table and the wire format as compiling, tested Go.
> This document is what was decided; [`grain-options.md`](grain-options.md)
> is what was considered and why, including the paths not taken and the
> conditions that would reopen them.

**A grain is a polled container that runs one agent against one sandbox.**

Everything below follows from that sentence, and it has two faces that
should not be confused. Facing its agent, a grain is an ordinary MCP
server over stdio, and everything it serves is about the sandbox. Facing
its controller, it is read rather than called: it emits records and is
listed, never dials out, holds nothing open, answers no questions, and
does not know a controller exists.

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
│  grain-shim (PID 1)                                         │
│   ├─ kontur (VMM) ── cloud-hypervisor ──┐                   │
│   ├─ agent CLI (claude / agy / codex)   │                   │
│   └─ MCP server ─> /run/kontur/vsock.sock ┼─> guest VM      │ ← the sandbox
│                                          │   checkout,      │
│  holds: credential, prompt, setup,       │   builds, tests  │
│         tools, placements                │                  │
└─────────────────────────────────────────────────────────────┘
        │ stdout: trajectory + status records — the only route out
        ▼ and there is no route in: the controller creates, lists,
          tails and destroys, and never calls
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

## Tools: six built in, and nothing else is a tool

Grain's own are six, every one about the sandbox:

| tool | served |
| --- | --- |
| `run_command`, `read_file`, `write_file`, `edit_file` | locally, over vsock |
| `recreate_sandbox` | locally — a kontur call, now that the VMM is the shim's own child |
| `status` | locally — writes `status.activity`, read off the record stream |

**Everything else is a CLI in the guest**, run with `run_command` like
anything else the agent runs. `grain open-pull-request --title "…"`, run
in the guest,
not a tool call. Grain has no vocabulary for any of it and does not know a
controller exists.

| tool | today | as a grain |
| --- | --- | --- |
| `run_command`, `read_file`, … | `docker exec` → `kontur exec` | built in — local, vsock |
| `recreate_sandbox` | MCP → daemon REST → registry → four `restore*` | built in — local kontur call |
| `update_status` → `status` | MCP → daemon REST → store write | built in — a local file write |
| `open_pull_request`, `pull_request_status`, `wait_for_checks` | daemon REST / controller's GitHub client | the guest CLI |
| `ask_question`, `request_secret` | deferred into the result | the guest CLI |
| `comment_on_issue`, `propose_task`, `add_review_comment` | deferred into the result | the guest CLI |

`status` is the one escape hatch that becomes *fully* local: `update_status`
is an HTTP hop today to put a phrase on a task's row, and as a built-in it
is a file write that cannot fail and costs the agent nothing.

### Three things can set `activity`, and setup is why

The agent's `status` tool is the cheapest route and not the only one,
because it is not always available:

| writer | how | when |
| --- | --- | --- |
| the shim | directly | the coarse steps it drives — booting, placements, starting setup |
| the agent | the `status` tool | a local file write in the container, no vsock hop |
| **anything in the sandbox** | **writing `/run/grain/activity`** | setup, and long guest commands |

The third closes a gap the other two cannot. `activity`'s own worked
example is `"cloning acme/widgets"` — which happens during **setup, before
there is an agent to call a tool**. Without a guest-side writer,
`PhaseProvisioning` could say a grain was provisioning but never what it
had got to, and a grain killed by `ProvisionBudget` is exactly the one
where that difference matters: "still cloning acme/widgets after ten
minutes" names a stuck clone, where "still provisioning" names nothing.
It also covers a build the agent is blocked on, which cannot ask the agent
to speak for it.

**The mechanism is a file, read on a round trip that already happens.**
The shim reads the guest for `health.guest` on its heartbeat; picking up
one more path on that call costs nothing and inherits that cadence. It is
advisory by construction — last writer wins, a torn read is a garbled
phrase — which is what lets it be a plain file rather than a channel with
a protocol. `grain activity` in the guest writes it atomically; a setup
script with
nothing but a shell can `echo` into it, and the file is the contract
either way.

It is deliberately **outside `/grain`**: that tree is mounted inward and is
grain's own material, and a guest-writable path inside it would let the
sandbox appear to have authored a prompt or a credential
(`TestTheGuestActivityPathIsOutsideTheMountedTree`).

### This is the git proxy's shape, reused

Not a new mechanism — the one grain already has, pointed at a second
service:

| | git | controller |
| --- | --- | --- |
| how the guest reaches it | an address the proxy advertises | the same |
| where the credential lives | a placement in the guest | a placement in the guest |
| where the address lives | the clone URL in `setup` | a placement beside the token |
| what authorizes a request | `Store.GitScope` → live run → its repos | the same resolution → what that run may ask |

`startGitProxy` already binds `0.0.0.0` and advertises a host when
`-kontur-git-proxy-host` is set — "typically the docker bridge gateway
address the guest's own outbound NAT routes through to reach this host" —
and binds loopback only when sandboxes share the daemon's netns. A
guest-reachable service on the controller is a deployed, working shape,
and it is **already a separate listener** from the daemon's REST API and
UI (`startGitProxy` and `startUIServer` are two servers on two ports).

**A credential in the guest is not a new exposure.** Git's token is
already there, because git runs there. What makes that safe is worth
copying exactly:

- **Authorization resolves through the live run, not the token.**
  `authorize.go`: "A sandbox with no live run authorizes nothing, which is
  the same fail-closed default a missing allowlist file gave." So a leaked
  credential is dead the moment the run ends — no expiry to tune, no
  revocation race.
- **Each request is checked**, not just the bearer.
- **A forbidden set** refuses some things to every sandbox regardless — for
  git, grain's own state repository when it holds encrypted secrets.

**What does not come for free is the scope.** Git's is repos; a controller
CLI needs its own answer to *what may this grain ask for* — open a pull
request on its own branch, comment on its own task, propose a task, ask
its own question. "Authenticated as this grain" is not "allowed to do
this", and that check is the controller's to write, modelled on
`authorize.go` rather than inherited from it.

### What this deletes

The MCP-to-controller path, entirely: no `ControllerURL`, no container-side
token, no loopback proxy in the shim, no Streamable-HTTP-versus-SSE split
to serve both `codex` and `agy`, and no per-framework MCP config beyond the
shim's own stdio server.

It also means **a grain needs no controller to run**. Nothing attaches,
nothing is held open, nothing waits.

**The container needs no daemon URL, no task ID and no bearer token.**
`agent.RunConfig.TaskID` exists solely as "the one fact a forked mcpserver
subprocess needs before it can ask the daemon to act on this run's
behalf" — that field goes, with `WithGrainServer` and the `-task` flag.

**Recreating a sandbox stops being a subsystem.** `SandboxRecreations`,
`sandboxRecreation`, `SandboxRebuilder`, `pkg/ui/sandbox_recreate.go` and
its route all go — roughly 900 lines with tests.

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
- **a CLI in the guest**, with its credential as a placement, for
  anything the agent should be able to ask for beyond its own sandbox.

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
into, and no "not configured yet" for a first status to mean.

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

## The log stream is the whole read

> **Everything a grain says, it says on stdout. There is no call into it.**

The shim emits its whole `Status` as a `kind: "status"` record on the same
stream that carries the trajectory, so `List` is served from two things a
controller reads anyway — the runtime's own container listing, for which
grains exist and whether each is running, and the log tail it already
follows. **That is the whole of it: no exec, ever, healthy or not.**

A full snapshot rather than a delta, so this stays level-triggered — the
property `Reconcile` rests on — and absence stays meaningful: container
state comes from the listing, so "running but nothing recent on the
stream" is a wedged shim, which is a distinguishable and more informative
state than an exec that hangs.

Emitted **on change plus a slow heartbeat**, never on a fast fixed
interval: the kubelet rotates at 10 MB across 5 files, and status records
would otherwise eat the budget the trajectory needs.

**`grain status` is gone, and with it the last call into a grain.** It
survived for a while as a fallback for a stale stream, and it did not earn
its keep: on Kubernetes exec and logs both go through the API server and
largely fail together, so it was a second route only under docker — and
even there it is the *same shim* answering, so a wedged one returns a
stale answer or hangs rather than a fresh answer. The state it was meant
to rescue is exactly the state it cannot report.

The cost is real and bounded: after log rotation loses the last status
record, a controller waits up to one heartbeat to learn a grain's phase
instead of asking. That is the heartbeat interval's job, and a knob is a
better place for that tradeoff than a second channel.

What falls out is that **a grain has no inbound surface at all.** Its
input arrives before it starts, as environment and files; its output is
records on stdout and a file at `/dev/termination-log`. Nothing to
authenticate, nothing to version as an API, nothing that can hang.

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

The transport is the container runtime, and nothing calls into a grain, so
what has to stay stable is **an input tree and an output format, not an
RPC schema** — `pkg/grain`'s Go types are the controller-side facade; the
environment, the files under `/grain` and the records on stdout are what
cross between the daemon binary and the sandbox image.

```
grain-shim
        PID 1 and the image's entrypoint, invoked with no arguments at
        all. Boots the VMM, waits for the guest, applies placements,
        installs the guest CLI, runs /grain/setup, starts the agent named
        by GRAIN_FRAMEWORK with /grain/prompt, serving the six sandbox
        tools and nothing else. Writes trajectory and status records to
        stdout. Does not exit until the grain is done.
```

**No verb at all**, which is the shape rather than an economy. Everything
a controller does to a grain is a container-runtime operation — create it,
list it, tail its logs, destroy it — so there is no second subcommand for
one to call, and a lone `run` would be ceremony around a program that does
one thing. An agent reaching past its sandbox does so as a command in the
guest, which to the shim is an ordinary `run_command` and to this program
is nothing at all.

Which means the stable contract is not a CLI in the controller's
direction: it is the **input tree** a grain is created with and the
**record format** it writes. Everything the shim needs, it reads from the
environment and from `/grain`.

Adding a subcommand later would be breaking, since argv-empty is the
contract. That is the right way round: a new subcommand would be a new way
to call into a grain, and not being able to add one quietly is the
property this whole section is about.

### Exit codes

`grain-shim`'s own, read where the runtime reports it —
`.status.containerStatuses[].state.terminated.exitCode`, or `docker
inspect` — rather than from any call:

| code | meaning | the controller |
| --- | --- | --- |
| 0 | ok | the `Result` is on the stream and in the termination log |
| 1 | failed; stderr and the termination log are the detail | reports it |
| 4 | unrecognised wire version | fails the run `setup-failed`, naming both |

Code 4 is what version skew looks like now that there are no subcommands
to get wrong: an image older than its controller meets a
`GRAIN_WIRE_VERSION` it does not know, and `SpecFromEnv` refuses it before
anything boots. Exiting distinctly matters because the alternative — a
generic failure — is indistinguishable from a bad setup script, and the
two want different responses.

The distinction that used to live here, **exec-failed versus
shim-failed**, does not disappear so much as move inside: it is now the
shim's own, between `kontur exec` failing before the guest ran and a guest
command that ran and failed. `mcp.DockerExecRunner`'s
`execFailedBeforeGuest` already draws that line, and it is a `run_command`
result rather than anything a controller sees.

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

### The status record

One record, everything — a whole snapshot on each emission, which is what
lets a controller that missed the last one lose latency rather than
correctness:

```json
{
  "version": "v1",
  "phase": "running",
  "since": "2026-09-04T19:41:12Z",
  "activity": "waiting for CI",
  "rebuilds": 1,
  "setup": { "exitCode": 0, "output": "9f3c1a2\n" },
  "health": {
    "container": { "running": true },
    "guest": { "ready": true, "loadAverage": "0.41 0.30 0.22",
               "conntrackCount": 812, "conntrackMax": 262144 }
  },
  "seq": 4471
}
```

`phase` is one of `provisioning`, `running`, `succeeded`,
`failed`, `cancelled`, `lost`, `released`. `since` is when it was entered,
and every timeout the controller enforces is a subtraction against it.

**There is no id, and nothing echoed back.** The container is the identity:
a record is read off one specific container's stream, so the answer cannot
be ambiguous about whose it is.

**There is no `container` health either**, for the same reason and a
sharper one: a grain cannot report that it is unreachable, and one that
could answer has already answered the question. The backend fills both in
from the listing while merging it with the stream, and
`TestAGrainReportsNeitherItsNameNorItsReachability` holds the split exact
— which matters more now that there is no second channel to reconcile
against.

**There is no `blocked` phase.** An agent waiting on the controller is
waiting on a command it ran itself in the guest, which to the shim is an
ordinary `run_command` that has not returned — and that is enough. The
controller is the far end of whatever that command called, so it already
knows which grains are waiting on it.

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

That covers the read that must not be missed, and it is load-bearing now
that the stream is the only other channel: a grain that finished but whose
final status record was lost to rotation is a run the controller cannot
finish, and it holds a slot until something notices. The termination log
is not subject to rotation and the listing carries it, so the ending
survives what the stream does not. Pair it with
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
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)
	Release(ctx context.Context) error
}
```

**None of it reaches the shim.** `Create` is `konturctl vm create` with the
grain's environment and mount, `List` is `docker ps --filter
label=grain.id` merged with the log tail, `Transcript` is `docker logs
--since`, `Release` is `docker rm -f` (and is also how a grain is
cancelled). Every method is one container-runtime operation, and a method
that cannot be served by one has drifted from what the transport can do.

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
| 7 | `running` | live | mirror activity |

Row 5 is one tick, not two: signalling and then waiting for the next tick
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
the grain goes terminal and the next tick frees the slot. A stopped run
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

## Three binaries, one per trust zone

The split falls out of the credential boundary rather than being chosen
against it: each binary runs in exactly one of the three places, and the
places are the same ones every other decision here turns on.

| binary | runs | does | reaches |
| --- | --- | --- | --- |
| **`grain`** — the controller | the operator's host | UI, work selection, dispatch, git proxy, the guest-CLI endpoint, and today's operator verbs — `state`, `secrets`, `repo`, `sync`, `list`, `pause`, `metrics`, the image builders | the container runtime; GitHub |
| **`grain-shim`** | the container, as PID 1 | boots the VMM, applies placements, runs setup, starts the agent, serves the six sandbox tools, writes records to stdout | the guest, over vsock |
| **`grain`** — the guest CLI | the guest | what the agent runs with `run_command`: `activity` locally, and the controller verbs — `open-pull-request`, `wait-for-checks`, `ask-question` | the controller, with a placed credential |

Two of them are called `grain`, and that is the decision rather than an
oversight — see "Naming" below. They never share a filesystem, and they
are the same interface seen from its two ends. **In prose here: "the
controller", "the shim", "the guest CLI".** Role words, because that is
what the ambiguity costs and it is a price other projects pay too.

Ships alongside, not grain's: `kontur` in the container layer
(`COPY --from=kontur`, since kontur's own final stage is `FROM scratch`),
and `kontur-agent` in the guest.

### Where each one is baked, which is fewer places than it looks

Grain publishes **two images, as it does today**: its own, and the sandbox
image (`sandboximage.go`, `grainimage.go`, stamped into each other at link
time so a build says which sandbox it expects). The sandbox image is
already *one artifact carrying both halves* — "the container and the guest
are the same thing, with one tag to stamp and one to pull", since kontur
generates the SSH keypair per VM boot and the guest disk stopped needing
to be built per host. So all three binaries live in two images:

| image | holds |
| --- | --- |
| grain's own | the controller |
| the sandbox image | `grain-shim` + `kontur` + the agent CLIs in the container layer; `kontur-agent` in the guest disk; **the guest CLI** |

**The guest CLI is baked into the container layer and installed into the
guest by the shim**, on the same provisioning pass that applies
placements. Not *as* a placement — a placement's bytes come from the Spec,
and a Spec carrying a binary is the configuration path being used as a
delivery mechanism — but by the same walk, from a fixed path in the
container to a fixed path in the guest.

That way round because the guest disk is *derived* from a published kontur
image (`scripts/kontur/build-guest.sh`) rather than authored, so keeping
grain's binaries out of the derivation keeps it a derivation. It also
means the guest CLI versions with the shim that reads its `activity` file,
which is the coupling that would otherwise need a rule.

### But it is a separable artifact, and should be published as one

The default path is the sandbox image; it should not be the only one. A
deployment running a different agent harness — no grain shim, no kontur
guest, an agent somewhere else entirely — still wants the controller
verbs, and the controller does not care what is on the other end: it
resolves a token to a live run and checks scope, which is true of any
caller.

Two things follow for how the guest CLI is built:

- **Static, with no assumption that grain is around it.** It has to run in
  a guest grain did not build.
- **Its verbs split by what they assume, and that split is worth being
  explicit about.** The controller verbs need only an address and a
  credential. `activity` needs grain's shim to be reading
  `/run/grain/activity` — outside a grain, it writes a file nobody reads.
  A no-op sink rather than an error, since a harness that cannot report
  activity should not thereby fail to open a pull request.

**This resolves the guest-writer question**: one guest CLI means `activity`
is a verb of it rather than a second binary. The consequence to build
against is that **the guest CLI must run with no credential present** —
`grain activity` writes `/run/grain/activity` and needs neither an address
nor a token, so the credential is per-verb rather than a startup
requirement. One that refused to start without a token would make the
local verb depend on a deployment having configured the remote ones.

## Naming

**The principle: give the new name to the thing with the least human
traffic.** Operators type the controller daily. Agents are told the guest
CLI's name in prompts, and models read it. Nobody types the shim — it is
one `ENTRYPOINT` line. So the shim absorbs the new name, and the two that
people and models handle keep the one they know.

### The controller keeps `grain`

`cmd/grain` is the daemon today — `grain daemon`, `grain state`, `grain
secrets`. Moving that name onto a different program is the one change here
that fails *by running something else* rather than by not resolving, which
is the worst way for a rename to fail: every script, unit file and runbook
that says `grain daemon` would quietly start a shim.

Nothing needed that rename. The clean split comes from naming the shim,
which no human types.

A wholesale product rename — `drift`, `drift-shim` — would also be
coherent, since nothing gets recycled. Two notes if that is ever tempting:
[driftctl](https://github.com/snyk/driftctl) is an existing Snyk CLI for
Terraform drift detection, so the term is occupied in exactly this
neighbourhood; and "drift" names the condition a reconciler exists to
correct rather than the cure.

### The guest CLI is `grain`, not `grainctl`

`-ctl` is not a neutral suffix. It marks **an operator at a terminal
talking to a control plane** — `kubectl`, `systemctl`, `journalctl`,
`etcdctl`, `doctl`, `flyctl`, `driftctl`, and `konturctl` next door. The
guest CLI is the opposite: the sandboxed thing asking for what it cannot
do itself, and the one holding a credential. A newcomer reading
`konturctl` and `grainctl` side by side would guess exactly wrong about
which is which.

The comparable category — **a client inside a workload, calling a control
plane, authenticated by a scoped credential from its environment** — names
things differently, and consistently:

| system | in-workload client | verbs |
| --- | --- | --- |
| [Buildkite](https://buildkite.com/docs/agent/cli/reference) | `buildkite-agent` | `annotate`, `meta-data set`, `artifact upload` |
| GitHub Actions | `gh` | run-scoped `GITHUB_TOKEN` from the environment |
| AWS (Lambda/ECS) | `aws` | task role, no credential in the code |
| Vault | `vault` | scoped token |
| Cloud Build | `gcloud` | build-scoped service account |

Buildkite is the closest structural match found: a binary run inside the
job, scoped to that job, with verbs that ask the control plane to do what
the job cannot. `buildkite-agent annotate` is `grain open-pull-request`
with a different noun.

**Nothing in that category uses `-ctl`.** Two conventions are live — name
it after the service, or `<product>-agent` — and **`-agent` is unavailable
here**, triply: the model agent is what runs in the container,
`kontur-agent` is the guest daemon, and `grain-agent` would collide with
both. So: named after the service.

The prose ambiguity is real and is the price every project in that table
pays (`aws` the CLI against AWS the service, `vault` against Vault). They
pay it with role words in prose, which this document already has — the
controller, the shim, the guest CLI.

### One binary or two

Buildkite makes these the *same* binary: `buildkite-agent start` runs the
agent, `buildkite-agent annotate` runs inside the job. One binary in two
modes is available here too, and has that precedent.

Split anyway, for a plainer reason than trust: the guest CLI is meant to
be publishable on its own for other agent harnesses ("But it is a
separable artifact"), and shipping a VMM-booting shim to somebody who
wants three verbs is a poor trade. The security argument for splitting is
weaker than it looks and should not be leaned on — the shim's powers come
from the vsock socket and `/dev/kvm` in the container, not from the
binary, so a copy in the guest could not boot anything.

## Costs and open items

1. **The trajectory rides the container log stream**, so it needs tagged
   records to share stdout with the guest console, and the controller must
   persist what it reads. It also puts prompts and model output wherever
   that deployment ships container logs — a per-deployment decision rather
   than a detail.
2. **The sandbox image gains a real base and an entrypoint.** kontur's
   final stage is `FROM scratch` with
   `ENTRYPOINT ["/usr/local/bin/kontur"]`, so a node-based CLI cannot run
   there; kontur keeps its scratch image. Grain's sandbox image needs a
   real base, `COPY --from=kontur`, the agent CLIs, the guest CLI, and
   `grain-shim` as entrypoint. Still two published images, not three — the
   sandbox image already carries the container layer and the guest disk
   together.
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
7. **The controller's scope check is unwritten**, and it is the one piece
   of the guest-CLI decision that is not already built. `GitScope` answers
   "which repos", which is not "may this grain open a pull request on this
   branch, comment on this task, ask this question". Modelled on
   `authorize.go` — per-request, fail-closed, resolved through the live
   run — but written fresh.
8. **An escape hatch has to be told about.** An MCP client asks
   `tools/list`; a CLI does not announce itself, and an agent never told
   the guest CLI exists finishes without opening a pull request and reports
   success. Naming it belongs in the prompt or the setup script, per
   framework, and is part of the work rather than documentation.
9. **Two binaries are called `grain`, and the docs are the only thing
   keeping them apart** ("Naming"). The controller and the guest CLI never
   share a filesystem, so nothing at runtime can confuse them — but a
   reader can, and so can a search. The mitigations are cheap and belong
   in the work: role words in prose (the controller, the shim, the guest
   CLI), `grain --help` in each saying which one it is and where it runs,
   and the guest CLI refusing the controller's operator verbs with that
   sentence rather than "unknown command".

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
4. **Split the binary.** Today's `cmd/grain` is the controller and nothing
   else, and it keeps both its name and its verbs; `grain-shim` and the
   guest CLI are both new, so nothing an operator runs changes.
   `mcpserver.go` is deleted rather than moved.
5. The guest CLI and the controller-side listener it calls: the scope check
   (item 7 above), then the verbs that today are MCP tools reaching the
   daemon. Shares `gitproxy`'s token store and `startGitProxy`'s
   advertise-host handling. `grain activity` lands here too and depends
   on none of it.
6. kontur's NAT mode (the blocking ask), then `grain-shim` and the sandbox
   image. Steps 1–5 do not wait on it.
7. `KonturGrains`.
8. Delete: `recreate.go`, `orphan.go`, `recover.go`, `InFlight`, `runOne`,
   `RunDispatch`'s sandbox half, `pkg/ui/sandbox_recreate.go`.
