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

## The tool line

> **If grain declared it, grain serves it. If you declared it, grain
> forwards it.**

Grain's own are six built-ins, every one about the sandbox:

| tool | served |
| --- | --- |
| `run_command`, `read_file`, `write_file`, `edit_file` | locally, over vsock |
| `recreate_sandbox` | locally — a kontur call, now that the VMM is the shim's own child |
| `status` | locally — writes `status.activity`, read on the next poll |

Everything else is an ordinary MCP tool declaration in `/grain/tools/`
that the grain advertises, holds calls to, and relays **without a
vocabulary of its own for it**. So `open_pull_request`, `ask_question`,
`wait_for_checks` and the rest are grain-the-product's tools, declared by
the controller that knows what they mean; a deployment can add a tool
grain has never heard of without grain being changed or released.

| tool | today | as a grain |
| --- | --- | --- |
| `run_command`, `read_file`, … | `docker exec` → `kontur exec` | built in — local, vsock |
| `recreate_sandbox` | MCP → daemon REST → registry → four `restore*` | built in — local kontur call |
| `update_status` → `status` | MCP → daemon REST → store write | built in — a local file write |
| `open_pull_request` | MCP → daemon REST | declared and forwarded |
| `pull_request_status`, `wait_for_checks` | controller's GitHub client | declared and forwarded |
| `ask_question`, `request_secret` | deferred into the result | declared and forwarded |
| `comment_on_issue`, `propose_task`, `add_review_comment` | deferred into the result | declared and forwarded |

`status` is the one escape hatch that becomes *fully* local: `update_status`
is an HTTP hop today to put a phrase on a task's row, and as a built-in it
is a file write that cannot fail and costs the agent nothing.

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
- **a tool declaration** (`/grain/tools/…`), for anything the agent should
  be able to ask for.

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
/grain/tools/open_pull_request.json              0644
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
  - name: tools
    configMap: { name: grain-tools }
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

**State.** Every method is idempotent, none blocks on the work, and
`Observe` returns the whole of what can be seen rather than a delta — the
same level-triggered discipline `orchestrator.Reconciler` already states.
A grain the controller cannot reach is `PhaseLost`, with a rule for it; a
grain that stopped *pushing* would be silence indistinguishable from
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

grain status                       > status.json
grain answer  --call <id>          < answer.json
grain signal  --id <id>            < signal.json
```

Three verbs on the control surface. Payloads go on stdin, never argv — a
prompt-sized document crosses the runtime's API as JSON, and stdin has no
size limit and no quoting hazard.

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

### Delivery

`answer` and `signal` cannot hand anything to the supervisor directly —
different process — so they write into a spool directory it watches,
atomically, named by the caller's id. The supervisor consumes each and
echoes its id in `status.consumed`; the controller stops resending once it
sees its own id there. At-least-once with dedupe by id, which is what makes
both idempotent, and why `signal` takes an `--id` though it replies to
nothing.

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
  "call": {
    "id": "c-7",
    "tool": "open_pull_request",
    "arguments": { "title": "Port the staleness check" },
    "since": "2026-09-04T19:40:00Z"
  },
  "health": {
    "container": { "running": true },
    "guest": { "ready": true, "loadAverage": "0.41 0.30 0.22",
               "conntrackCount": 812, "conntrackMax": 262144 }
  },
  "seq": 4471,
  "consumed": ["sig-19"]
}
```

`phase` is one of `provisioning`, `running`, `blocked`, `succeeded`,
`failed`, `cancelled`, `lost`, `released`. `since` is when it was entered,
and every timeout the controller enforces is a subtraction against it.

**There is no id, and nothing echoed back.** The container is the identity:
a controller execs into one specific container, so the answer cannot be
ambiguous about whose it is.

**`call` is one slot, not a queue.** The shim serves what it can and
forwards the rest; if two forwarded calls arrive together — which parallel
tool use makes possible — it holds the second until the first is settled.
A grain is blocked on something or it is not.

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

### `answer.json` → `grain answer --call c-7`

`mcp.Result`'s own two fields, so the shim returns it to the agent as that
tool's result with nothing to translate:

```json
{ "version": "v1", "text": "opened #812: https://github.com/bwsalmon/grain/pull/812" }
```

A refusal is an answer:

```json
{ "version": "v1", "isError": true,
  "text": "parked for a human; wrap up now" }
```

Leaving a call unanswered blocks the agent until its deadline. **Whether a
tool blocks is the controller's policy, not the wire's**: `wait_for_checks`
is a call it simply does not answer until CI has a verdict, and
`ask_question` is the same mechanism with a different policy — answer it if
a human is watching, or refuse it to reproduce today's end-the-turn
behaviour. Both are one tool result the agent reacts to.

### `signal.json` → `grain signal --id sig-20`

```json
{ "version": "v1", "kind": "addenda", "addenda": ["Also fix the flake in TestMergeQueue."] }
{ "version": "v1", "kind": "cancel", "reason": "the task was closed" }
{ "version": "v1", "kind": "pause", "reason": "the deployment met its usage limit" }
```

One mechanism replacing three: `orchestrator`'s `addendaPoller`,
`watchForTaskClosed` and `Pause.register` all exist because a run in flight
has no address anything can deliver to.

Signals are the half of this channel that is **not** MCP, and that split is
the rule for anything added later: a tool is a declaration and costs grain
nothing, where lifecycle is a change to this contract.

### Trajectory records ← `docker logs`

One JSON object per line on the container's stdout.

```json
{"version":"v1","seq":41,"t":"…","src":"shim","kind":"phase","data":{"phase":"running"}}
{"version":"v1","seq":42,"t":"…","src":"console","data":"[    0.512] EXT4-fs (vda): mounted"}
{"version":"v1","seq":43,"t":"…","src":"agent","kind":"tool_use","data":{"name":"run_command"}}
```

**`src` is required and cannot be replaced by writing one source to
stderr**: kontur already routes the guest console to this stream, and
`kubectl logs` merges stdout and stderr, so splitting by fd works under
docker and nowhere else. Sharing the stream with the console is what makes
a failed boot legible — a run killed by the provisioning budget can quote
the last console lines rather than reporting only that time ran out.

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
	Answer(ctx context.Context, call CallID, ans Answer) error
	Signal(ctx context.Context, sig Signal) error
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)
	Release(ctx context.Context) error
}
```

Half of it never reaches the shim: `Create` is `konturctl vm create` with
the grain's environment and mount, `List` is `docker ps --filter
label=grain.id`, `Transcript` is `docker logs --since`, `Release` is
`docker rm -f`. A method that cannot be served by one subcommand or one
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
| 5 | any | task closed | `signal(cancel)` |
| 5 | any | paused | `signal(pause)` |
| 6 | `provisioning`, over budget | live | `fail(setup-failed)`, `release` |
| 7 | `running` / `blocked` | live | `answer` the one call; `signal(addenda)`; mirror activity |

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
