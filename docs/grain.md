# Grains: moving the agent next to its sandbox

> **Proposal.** Nothing in this document ships yet. `pkg/grain` carries
> the interface and the controller's decision table (`Reconcile`) as
> compiling, tested Go, and [`docs/grain-cli.md`](grain-cli.md) is the
> wire contract this design implies — the in-container CLI, its
> documents, and the trajectory records; everything else here is the argument for them and
> the work they imply. The network decision is recorded in "The network:
> NAT or flat", with the alternatives kept beside it.

## What runs today

The agent CLI is a subprocess **on the controller**. Its MCP config points
at a forked `grain mcpserver -kontur-vm <vm>`, and every `read_file` or
`run_command` is a `docker exec <container> kontur exec` round trip into
the sandbox VM. A run's liveness is a goroutine's stack inside the daemon:
`reconcileDispatch` spawns `runOne`, which blocks through a VM boot, a
token mint, a checkout, an hour of agent, `ProcessResult` and a release.

Most of what is awkward in `pkg/orchestrator` follows from that one fact:

- `runOne`'s sixty-line comment about a setup failure stranding a live
  row, and the `ranAgent` guard that exists to prevent it.
- `InFlight` and `drainInFlight`, so a shutdown does not abandon runs.
- `orphan.go` and `recover.go`, sweeping at startup for what a crash left.
- `watchForTaskClosed`, `addendaPoller` and `Pause.register`: three
  separate mechanisms for "something outside wants this run to stop, or
  to hear something", because a run in flight has no address.
- `recreate.go`'s registry, its lookup-by-task-ID, its four `restore*`
  methods and the `POST /api/tasks/{id}/sandbox/recreate` hop behind them
  — all so an agent can ask for a fresh sandbox it is not co-located with.

## What a grain is

A grain is a **container**: the agent CLI, a kontur VMM, and the guest VM
that VMM boots, with a shim as PID 1 holding them together.

```
┌─ grain container (one per run) ─────────────────────────────┐
│  grain run (PID 1)                                         │
│   ├─ kontur (VMM) ── cloud-hypervisor ──┐                   │
│   ├─ agent CLI (claude / agy / codex)   │                   │
│   └─ mcpserver ─> /run/kontur/vsock.sock ┼─> guest VM       │ ← the sandbox
│                                          │   checkout,      │
│  holds: model credential, Spec,          │   builds, tests  │
│         transcript, status.json          │                  │
└─────────────────────────────────────────────────────────────┘
        ▲ docker exec / kubectl exec — the controller's only route in
```

The agent is in the container; the sandbox is the guest, one vsock hop
away. That boundary is the whole design.

### The credential boundary

The agent's own credential lives in the **container**, on the far side of
vsock from anything the repo's code can run. It is not in the sandbox, so
a build script or a test cannot read it.

This is better than today, not merely equivalent. Right now one controller
process holds every run's secrets alongside the store, the GitHub app key
and the git proxy's signing key. Per-grain containers cut the blast radius
of any one compromise to one run.

**The boundary comes from where the agent runs, not from anything in the
Spec.** An earlier draft of this had `Placement` carry a destination so
that model-facing keys could land container-side; that was wrong on the
facts. Every capability grain has that places anything places it in the
sandbox — `githubsandbox`, `gcpkey` and `geminikey`, all
`model.SideSandbox` — and each is material the *work* needs.
`geminikey` is the one that looks like a counterexample: it mints a key
for a task and names the path in the prompt so the work can find it, which
is the sandbox's business and not the agent's. `model.SideController`
exists and nothing produces one; `run.go:1832` skips it, "not written
anywhere".

So placements are all guest-side and carry no side at all. The agent's own
credential travels beside the framework name instead —
`"framework": {"name": "claude", "credential": "..."}` — because only the
profile in the image knows where that CLI expects it, and because which
credential a grain needs follows from the framework its *task* chose
(`model.Task.AgentFramework`), so container configuration would mean
shipping every deployment's every credential into every container. It is
not path-addressed, which is what distinguishes it from a placement and
why dropping `dest` cost nothing.

It arrives as a file at `/grain/credential`, which on Kubernetes is a key
in a mounted Secret — so the pod spec holds a reference, the value keeps
the Secret's own RBAC and encryption at rest, and the container's own
environment carries no material at all. The profile reads it and hands it
to its CLI however that CLI wants it. The sandbox cannot read it either
way, for the structural reason above.

The residual: the agent process can read its own token and could write it
into the guest. That is unchanged from today, and it is not what the VM
boundary defends against.

## The line: local versus controller

**A tool is local to the grain iff it touches only the sandbox. It is a
controller request iff it touches the store, GitHub, or a human.**

| tool | today | as a grain |
| --- | --- | --- |
| `run_command`, `read_file`, `write_file`, `edit_file` | `docker exec` → `kontur exec` | **built in** — local, vsock |
| `recreate_sandbox` | MCP → daemon REST → registry → four `restore*` | **built in** — local kontur call |
| `update_status` → `status` | MCP → daemon REST → store write | **built in** — writes `status.activity`, read on poll |
| `open_pull_request` | MCP → daemon REST | **declared and forwarded** |
| `pull_request_status`, `wait_for_checks` | controller's GitHub client | declared and forwarded |
| `ask_question`, `request_secret` | deferred into the result | declared and forwarded |
| `comment_on_issue`, `propose_task`, `add_review_comment` | deferred into the result | declared and forwarded |

The line is not a list of tool names: **if grain declared it, grain serves
it; if you declared it, grain forwards it.** Grain's own are the six
built-ins above, every one of them about the sandbox. Everything else is
an ordinary MCP tool declaration in `/grain/tools/` that the grain
advertises, holds calls to, and relays without a vocabulary of its own —
so `open_pull_request` and `ask_question` are grain-the-product's tools,
declared by the controller that knows what they mean, and a deployment
can add one grain has never heard of without grain being changed.

A forwarded tool is not translated into a request of our own: the shim
holds the MCP call open, it surfaces as `Status.Call`, the controller
executes it, and `Answer` is `mcp.Result`'s own two fields handed straight
back as the tool's result. At most one is outstanding at a time — the
status has one slot, not a queue. Whether a tool blocks the agent or tells
it to wrap up is the controller's policy, expressed as how fast it
answers and what it answers with, and is invisible to the grain. See
`docs/grain-cli.md`, "This is MCP over a poll".

**The container needs no daemon URL, no task ID and no bearer token.**
`agent.RunConfig.TaskID`'s entire justification is "the one fact a forked
`mcpserver` subprocess needs before it can ask the daemon to act on this
run's behalf". With nothing left to ask, that field goes, along with
`WithGrainServer` and the `-task` flag.

**Recreating a sandbox stops being a subsystem.** It is a local kontur
call: the agent asks kontur, in its own container, to throw the guest away
and boot a fresh one. `SandboxRecreations`, `sandboxRecreation`,
`SandboxRebuilder`, `pkg/ui/sandbox_recreate.go`, its route and its client
method all go — roughly 900 lines with their tests. The rebuild *recipe*
problem dissolves with them: `setMaterialized` exists today so a rebuild
replays already-minted credentials rather than minting a second set behind
the back of a single revoke, and with the `Spec` sitting in the container
next to the thing being rebuilt, a rebuild is "fresh guest, replay
`Spec.Placements`, re-run `Spec.Setup`". No registry, no re-mint,
no cross-goroutine coordination.

Two more things move inside by the same rule:

- **`ConfigureGitCredentials`** comes off the sandbox interface and
  becomes an ordinary `Placement`. The controller still mints the token —
  it is the proxy's — and revokes it at reap; writing it is the same work
  said uniformly, which takes a method off the interface and a special
  case out of the setup path.
- **`prepareCheckout`** (~500 lines of `checkout.go`, currently cloning
  through MCP round trips) becomes part of `Spec.Setup` — a script the
  controller composes and the shim runs without reading. A clone is git
  commands in the guest and nothing more, and this is what keeps
  repositories, branches and proxy URLs out of the wire entirely.

## Poll, not push

**Decision: the controller polls, over the container runtime's exec.** The
grain never initiates. Four options were weighed:

1. **Poll via exec** — `grain status` per grain per tick.
2. **Push with a credential** — the shim holds a token and posts to the
   daemon's REST API. This is what grain does today for `update_status`,
   `open_pull_request` and `recreate_sandbox`.
3. **Attached stream over exec** — a persistent exec per grain, the shim
   writing events into it as they happen: exec transport, push semantics.
4. **Poll via shared volume** — the shim writes `status.json` to a
   hostPath the controller reads directly.

### Why not push

**Push forces NAT, and the network decision deliberately keeps flat
available.** Under flat mode the container can send but never receive —
the splice steals ingress, so no connection can be established at all.
There is no HTTP push from a flat-mode grain. Choosing push would close
the door the section below leaves open: a tunnelled grain, or any later
shape with nothing at the container layer needing network, could no longer
report at all. Exec-poll works under either mode because it never touches
the container's network.

**Silence is information under poll and ambiguous under push.** A grain
the controller cannot reach is a grain that has failed — `PhaseLost`, with
a reconcile row for it — and the controller's view is at most one tick
stale, with the staleness itself detectable. A grain that has stopped
pushing might be dead, wedged, or idle, and nothing distinguishes them. So
push systems grow a heartbeat, which is a poll rebuilt with the failure
detector living inside the thing being monitored.

Underneath that: **poll is naturally level-triggered and push is naturally
edge-triggered, and edges get lost.** A push that lands while the
controller is restarting is gone, so push needs retry, at-least-once
delivery, idempotency keys and dedupe on receipt. `Reconcile` rests
entirely on level-triggering — running one is always safe, skipping one
costs latency rather than correctness — and push either gives that up or
rebuilds it by pushing the whole `Status` every time, which forfeits the
efficiency that motivated pushing.

### What push would genuinely have bought

Recorded honestly, so that neither is rediscovered as an argument later.

**Latency**, but only in one place. `ask_question` waits on a human, so a
tick is noise beside it; `open_pull_request` and `wait_for_checks` are
fine at tick granularity; cancellation is controller-to-grain and so is
push-shaped already. The one signal that genuinely suffers is the live
transcript, which is handled as an exception below.

**Scale.** Poll is O(grains × ticks) where push is O(events), so idle
grains cost nothing under push. This does not bite at the size grain runs
at — a single-operator cluster bounded by `-max-workers`, so single digits
to low tens, where the difference is irrelevant. **If it ever does, the
answer is a node-local aggregator the controller polls once per tick,
making it O(nodes) — not push.** Written down explicitly so that "polling
does not scale" is not later read as "we should have pushed": the
aggregator keeps every property above intact and push forfeits all of
them.

### What the credential argument is and is not

It would be easy to argue against push on the grounds that it needs a
credential in the container. That argument is weak and worth not making.
The container is the *trusted* side of this design — untrusted code runs
in the guest, behind vsock — so a control-plane token there sits in the
same trust zone as the model API credential already beside it.

The real cost is the **authorization surface**: what a token may do,
scoped to which grain, minted when, revoked when, and what happens when a
grain claims to be a run it is not. `pkg/gitproxy` is ~1,900 lines and a
good share of it is exactly that — `tokens.go`, `authorize.go`,
`forbidden.go`. Not danger; work, and a place bugs live.

Push also partly undoes the simplification this proposal is for.
`recreate_sandbox` collapsed from a subsystem into a local call precisely
because it stopped needing a daemon hop; making push the architecture
reinstates daemon hops as the norm and grows `pkg/ui` an ingest surface
rather than shrinking it.

### The trajectory goes to stdout

The trajectory is the one signal that genuinely wants low latency, and it
is not control-plane data — it is an append-only stream a human watches.
So it does not go through `Observe` at all: **the shim writes it to the
container's stdout, and the controller reads it back with the runtime's
own log stream** — `docker logs -f`, `kubectl logs -f`.

This extends a convention kontur already holds to rather than inventing
one. `internal/hypervisor/args.go:108` routes the guest's serial console
to kontur's own stdio with `--serial tty`, in its own words "so it shows
up under `kubectl logs`". Container stdout is already this stack's
observability channel; the shim is a second writer to it.

It is better than the held-open exec it replaces on four counts:

- **The runtime buffers and retains.** A controller that restarts mid-run
  resumes from `--since` rather than losing what it missed. A dropped exec
  loses the stream outright.
- **No backpressure onto the agent.** The daemon drains stdout
  continuously. A controller that stopped reading a held-open exec would
  eventually block the shim's write, which means a UI tab left open could
  stall a run.
- **Replay is free.** History comes from the same call as the tail, with
  no separate seek path.
- **Log collection gets it for nothing.** Wherever a deployment already
  ships container logs, the trajectory arrives with them.

**Records must be tagged, and this is not optional.** kontur already
writes the guest console to that stream, so without tags the trajectory
and the console interleave unreadably. Stream separation is not available
either: `kubectl logs` merges stdout and stderr, so splitting by fd works
under docker and not on Kubernetes. So the shim emits structured lines
carrying their own source and a monotonic sequence number, and the
controller demultiplexes.

That tagging turns the interleaving problem into a feature: **the guest
console arrives on the same channel**, which is exactly what is missing
when diagnosing a grain that never finished booting. A
`PhaseProvisioning` run killed by `Policy.ProvisionBudget` can quote the
last console lines in its `setup-failed` detail rather than reporting only
that time ran out.

Two limits worth stating plainly:

- **Logs are transport, not storage.** The kubelet rotates container logs
  (10 MB across 5 files by default) and docker's drivers have their own
  caps, so a long run's early trajectory can age out. The controller
  consumes the stream and persists it; the transcript of record stays
  `Config.TranscriptDir`'s, exactly as it is today. `Grain.Transcript`'s
  doc comment says this too, because it is the kind of thing that gets
  forgotten precisely once.
- **It widens where the trajectory lands.** Prompts, model output and
  whatever the agent read go wherever that deployment's container logs go
  — local disk under docker, Cloud Logging by default on GKE. Today they
  reach only `TranscriptDir` on the controller. Worth a deliberate
  decision per deployment rather than a discovery.

The cursor follows from the transport: a **sequence number, not a byte
offset**, since `docker logs` and `kubectl logs` are addressed by time and
line. A monotonic per-record sequence is the one cursor both a log stream
and a plain file can honour, which is why `Status.Seq` is what it is.

So the split is cleaner than "an exception to polling":

> **Snapshot state → exec poll. Append-only stream → container logs.**

Both are still the controller reaching in. The grain never dials out under
either.

### Transport is not the interface

`Observe` says nothing about how a status is fetched, which leaves option
4 available as an implementation detail rather than a different design. On
the docker backend the controller and its grains share a host, so reading
a hostPath is strictly cheaper than a `docker exec` with identical
semantics; it stops working the moment they are on different nodes.
`KonturGrains` may choose it; nothing above it needs to know.

### What the shape gives back

Every method on `Grain` is idempotent, none blocks on the work, and
`Observe` returns the whole of what can be seen rather than a delta. The
controller compares that answer to what it wants and issues at most one
round of actions per tick — the same level-triggered discipline
`orchestrator.Reconciler` already states.

Three things fall out that are not obvious:

1. **Reattach stops being a special case.** Identity is derivable
   (`dispatch.RunID`), state lives in the container, and `List` runs every
   tick — so controller restart is the ordinary path. `orphan.go`,
   `recover.go`, `InFlight` and `drainInFlight` all go, along with
   `runOne`'s detached-context cleanup.
2. **Tool calls get an order of magnitude faster.** This one is
   co-location rather than polling: per `read_file`, today is *fork docker
   CLI → dockerd RPC → `kontur exec` → vsock → guest*, and in the
   container it is *fork `kontur exec` → vsock → guest*, or a bare socket
   dial if kontur promotes `internal/execwire` out of `internal/`. The
   docker CLI spawn and daemon round trip were the expensive part.
3. **The grain is the container.** Lifetime, identity and liveness are one
   thing. `Release` deletes the container; the VMM and guest die with it.
   No orphan agent process, no supervision problem, no deferred cleanup
   racing a cancellation.

## The two-phase start

The controller assembles the prompt, because it reads the store — the
task's conversation, its previous attempts, the deployment's and the
repo's prompt extensions. But `previousAttemptsSection` needs the commits
earlier attempts pushed, and those can only be read from the checkout,
which is now inside the grain. That is an ordering inversion, and the fix
is poll-native:

1. `Create(Spec)` — no prompt. The grain boots, places, runs `Spec.Setup`
   (which the controller composed, clone included), and reports
   `PhaseProvisioned` with that script's exit code and output in
   `Status.Setup`.
2. The controller polls, assembles the prompt — folding in anything a
   human added since dispatch — and `Signal`s it. It wrote that setup
   script, so it can end the script with whatever the prompt needs read
   back (`git rev-parse HEAD`, the commits earlier attempts pushed) and
   parse its own output. The shim stays ignorant of all of it.
3. `PhaseRunning`.

It costs one tick on an hour-long run. It buys two things: a checkout
failure is diagnosed before a single model token is spent, and
addenda-since-dispatch fold in for free instead of waiting for the next
attempt.

## What a grain does not know

The `Spec` carries four things — `framework`, `shape`, `setup` and
`placements` — and reaches the container before it starts, in two halves:
scalars in the environment (kontur's own convention) and material as files
under `/grain`. There is no configure step to wait in, and on Kubernetes
the file half is a Secret or ConfigMap volume unchanged. There is no id — the container is the identity, and a
controller execs into one specific container, so a grain is never told a
name it makes no use of. There is no task in it either, no
repository, no branch, no git credential and no capability model —
because **a grain knows how to run an agent in a sandbox, and nothing
about why.**

Everything task-shaped reaches it in one of three shapes instead:

- **in the prompt**, assembled by the controller from its store and
  delivered by `Signal` once the sandbox is real;
- **in `setup`**, a script the controller composes — the clone included;
- **in a `placement`**, which is where a credential the work needs goes,
  git's among them — all guest-side, since that is the only side any of
  grain's capabilities has ever used. The agent's own credential is the
  exception and rides with the framework name, because it has no path the
  controller could name.

The boundary is worth more than the fields it saves. A shim that
understood repositories would have to agree with the controller about
branch naming, proxy URLs, what to do with a half-made checkout and what
a task is — an interface between two separately released artifacts,
carrying grain's whole task model across it. A shim that runs a script
and places files has no opinions to keep in sync, and `wire_test.go`
asserts on the marshalled document that none of it has crept back.

**`framework` is a name, not a configuration.** How a CLI is launched,
which flags it takes, where its MCP config must live and whether it needs
a private HOME are facts about that binary — and the binary ships in this
image. Today the daemon owns all of it (`pkg/agent/claude`,
`/antigravity`, `/codex`, ~5,700 lines; see antigravity's own doc comment
on `agy` having no `--mcp-config`, so each run gets a private HOME holding
one file). That knowledge sits in a different artifact from the CLI it
describes, so upgrading the CLI can require upgrading the daemon. Moving
it into the image versions the two together and makes adding a framework
an image change rather than a controller release.
`VersionReport.Frameworks` is how a controller checks before dispatching,
so a task naming a framework the image lacks fails at create rather than
inside a guest nobody is watching yet.

Two things fell out of drawing it this way:

- **`grants` is gone.** Both capabilities it carried only ever made
  content readable — grain's own source for `self-debug`, the embedded
  runbooks for `bootstrap-playbooks`. The sandbox image is built from that
  source and that binary, so both are already in it: what is left is a
  line in the prompt saying where, and no tool registration, no flag and
  no Spec field.
- **`limits` collapsed to one field.** Turns are a framework's own flag,
  and `Config.MaxAgentTurns`' doc comment already concedes both default to
  no cap and that "what actually bounds a runaway run is MaxRunRuntime".
  Rebuilds belong to `Policy.MaxRebuilds` alone — the controller has the
  view of whether repair is converging. `maxRuntime` survives, and is
  discussed below.

### Who enforces a deadline

`maxRuntime` is decided by the controller and enforced by the grain, which
looks inconsistent beside `Policy.ProvisionBudget` — the same kind of
bound, enforced from the other side — and is not.

**Before there is an agent, only the controller can act.** A grain wedged
in provisioning is precisely the one that cannot report being wedged, so
that budget has to be enforced from outside.

**Once there is an agent, the grain can stop it without depending on
anybody.** That is worth having on its own: a running agent is spending
money, and money should not keep leaving while a controller is down. The
two budgets sit at opposite ends of the same run for that reason.

The controller does not also enforce it — one rule, one enforcement point.
The concern `Config.MaxRunRuntime`'s own doc comment names, a stuck run
"tying up its share of the concurrency limit", is served anyway: the grain
goes terminal, the next poll sees it, and the ordinary finish path frees
the slot. A stopped run reports `cancelled` with the limit named, which is
what `run.go` already records for a timed-out run
(`model.RuntimeCapDetail`, "the run did not fail").

**What it does not cover**, recorded so nobody later assumes it does: a
controller that dies five minutes into a two-hour budget still leaves an
hour fifty-five of spending. Only a lease — the grain stopping if nobody
has polled it in a while — bounds that under *any* controller failure, and
it was considered and declined: more mechanism than the failure justifies,
and with a failure mode of its own in killing healthy grains over a slow
controller restart. Worth revisiting only if unattended controller death
turns out to be a real operational event rather than a hypothetical one.

## The interface

See `pkg/grain` for the real thing with its reasoning. In outline:

```go
type Grains interface {
	Create(ctx context.Context, spec Spec) (Grain, error)
	List(ctx context.Context) ([]Status, error)
	Get(ctx context.Context, id ID) (Grain, error)
}

type Grain interface {
	ID() ID
	Observe(ctx context.Context) (Status, error)
	Answer(ctx context.Context, req RequestID, ans Answer) error
	Signal(ctx context.Context, sig Signal) error
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)
	Release(ctx context.Context) error
}
```

There is deliberately **no `Rebuild`**. Rebuilding the guest is internal to
the grain; the controller learns of it only as `Status.Rebuilds` going up.
What the controller keeps is the policy needing a view the grain does not
have: a grain rebuilding in a loop is one to kill (`Policy.MaxRebuilds`,
backstopping the shim's own `Limits.MaxRebuilds` — both exist because they
fail differently, and the controller's is the one that still works when
the shim is what is wrong).

`Status` is fat by design: the poll is the only read, so a field split out
is a second exec per grain per tick.

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
whole of the per-grain policy is a table test rather than something that
needs a VM. Its ordering is the decision, not an implementation detail:

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
| 7a | `provisioned` | prompt not sent | `send-prompt` |
| 7b | `running` / `blocked` | live | `answer` the one forwarded call; `signal(addenda)`; mirror activity |

Note what row 2 does *not* have: a repair path. A wedged guest never
reaches the controller — the shim rebuilds it — so `PhaseLost` means the
whole grain is gone and there is nothing left to ask.

The `pkg/orchestrator` equivalent is `runOne` plus `RunDispatch`, ~730
lines whose behaviour can only be observed by dispatching a real run.

## The network: NAT or flat, by what the container layer needs

**Decision: both modes, selected by a property of the deployment rather
than by preference.**

> **Flat where nothing at the container layer needs network. NAT where
> something does.**

Flat is the better mode wherever it applies, and it stays kontur's
default. It puts *zero* netfilter in the guest's path, keeps the guest an
ordinary endpoint on the segment with the pod's own address and MAC, and
has no state to exhaust. A plain kontur VM — anything whose container is
just a VMM wrapper — should keep it, and nothing in this proposal asks
those deployments to change.

NAT is what a grain needs *because* it puts an agent in the container, and
that agent needs the model API. Of the ways to give the container a
working stack it is the only one with a single story in every environment
— docker, kontur's standalone kubelet, and a managed cluster alike —
needing no cluster provisioning and no bespoke component beside it. That
uniformity is what selects it over the alternatives below, which are each
cheaper in one environment and absent in another.

Making this a mode rather than a migration matters for what the costs
below actually mean: they are paid only by deployments that select NAT.
A kontur user running a VM keeps the spliced datapath exactly as it is,
with no conntrack, no nftables and no lost pod identity. It also keeps the
ask of kontur honest — restoring a mode beside the default, not reversing
a simplification for everybody.

One pairing worth noting up front, because it is not obvious: **the exec
tunnel below is the option that lets a grain keep flat mode.** If the
agent's egress goes over the controller's exec channel, nothing at the
container layer needs network, the rule at the top selects flat, and
neither cost in "What NAT costs" is paid at all. That is a real argument
for the tunnel beyond portability, and the reason it is kept rather than
merely recorded.

### Why the container has no network today

`internal/netshim/setup.go:29`:

> The external interface keeps its address: **the splice steals the
> interface's ingress**, so the namespace's own stack can never receive a
> reply and cannot hold a connection over it…

`splice.go` does that with a tc ingress qdisc plus a match-everything
filter with `mirred` egress-redirect, so frames arriving on `eth0` reach
the tap at L2 and the namespace's IP stack never sees them. The container
keeps its address cosmetically; egress goes to the veth peer and every
reply goes to the guest.

That is harmless while nothing in the container needs network. The moment
the agent CLI moves in, it needs the model API and cannot reach it.

There is no cheap carve-out. Discriminating replies-for-the-namespace from
replies-for-the-guest would need L3/conntrack-aware classification inside
what is deliberately a match-all L2 wire — and the two ends share a MAC by
design ("both ends may carry the same MAC address — which is the entire
point"), so there is nothing to match on at L2 either.

### What NAT mode is

Inside the pod's netns:

- a bridge (`10.0.2.1/24`, say) with the guest's tap enslaved, the guest
  on `10.0.2.2/24` default-via-`.1`
- **`eth0` left unspliced**, so the namespace keeps its address *and* its
  ingress — which is the whole point
- `net.ipv4.ip_forward=1`
- nftables: `postrouting … oifname eth0 ip saddr 10.0.2.0/24 masquerade`

After that the data path is entirely kernel — netfilter hooks rewrite,
`nf_conntrack` tracks, no userspace in the path. netshim programs it once
and exits, the same lifecycle its splice already has.

**Note this mode does not currently exist.** kontur deleted it:
`internal/cli/vm.go:245` rejects `-net nat` outright, and `-ip`, `-port`,
`-guest-port` and `-bridge-cidr` are deprecated-and-ignored beside it. So
this is a kontur feature request, not a flag — see "Asks of kontur".

### Why it works everywhere

The requirement that looks like it should block on a managed cluster is
writing `net.ipv4.ip_forward`: Kubernetes classes it unsafe and gates
`securityContext.sysctls` behind a kubelet flag you cannot set on a
managed cluster. **netshim is already `privileged: true` in every
manifest**, for an unrelated reason —
`deploy/k8s/gke-pod-exec-example.yaml:52`, "the netlink library creates a
tap by opening `/dev/net/tun` and a pod has no per-device grant to hand
it" — so it has a writable `/proc/sys` in the pod's own netns and can
write it directly.

| requirement | docker | static kubelet | managed cluster |
| --- | --- | --- | --- |
| `ip_forward` in the pod netns | `--sysctl` on the netns-holder | kubelet-config (kontur's own) | privileged netshim writes `/proc/sys` |
| nftables in the pod netns | CAP_NET_ADMIN | privileged | privileged |
| netfilter modules on the host | yes | yes | yes — kube-proxy needs them |
| **cluster-level provisioning** | none | none | **none** |

### What NAT costs

**1. No infrastructure-level differentiation.** Agent traffic and sandbox
traffic leave with the same source address, so a cloud firewall, VPC flow
logs and NetworkPolicy cannot tell them apart. Enforcement is still
possible with in-namespace nftables keyed on `ip saddr 10.0.2.0/24` versus
locally-generated, and that enforcement is as strong as the VM boundary —
the rules sit outside the VM, and subverting them means escaping
cloud-hypervisor into the container, at which point the agent's credential
is available anyway. What is genuinely lost is defence in depth and the
audit trail, and that the separation becomes something configured rather
than structural. **Writing those egress rules is part of the work, not a
follow-up:** without them the sandbox inherits the agent's egress.

**2. Conntrack as a new failure mode.** Not a performance cost — the
per-packet price is a hash lookup and a header rewrite, the same one every
container network already pays. The change is that flat mode has *zero*
netfilter in the path (tc ingress runs before netfilter's hooks, and the
frame never enters the IP stack) while NAT introduces a finite, stateful
table.

When it fills the kernel drops: `nf_conntrack: table full, dropping
packet`. Steady-state occupancy is roughly connections/second × how long
entries linger (`nf_conntrack_tcp_timeout_time_wait`, 120s by default), so
an ordinary clone-and-build is nowhere near it and a test suite opening a
thousand outbound connections a second is in range. Traffic that stays
inside the guest is never tracked, which cuts the exposure considerably.

What makes it worth naming despite being a tail risk is **who has to
diagnose it**. Inside the guest it reads as connection timeouts, TLS
handshake failures and hanging fetches — indistinguishable from a flaky
test or a registry having a bad day — and the conntrack table is in the
pod's namespace, outside the VM, so nothing the agent can run will show
it. The agent forms the wrong hypothesis and burns turns on it, and the
transcript gives a human the same misleading evidence.

Mitigations, all part of the work:

- set `nf_conntrack_max` explicitly per netns rather than inheriting a
  memory-derived default that has nothing to do with this workload
- `notrack` locally-generated traffic: once any conntrack-using rule
  exists everything traversing netfilter is tracked, including the
  container's own connections, which need no NAT since they already carry
  the right source address
- **report `nf_conntrack_count` against `nf_conntrack_max` in
  `GuestHealth`**, beside load, memory and disk. This is the one that
  fixes the diagnosis problem: it makes an invisible failure a reported
  one, and lets the agent be told rather than left to guess.

**3. It is net-new packet-path code in kontur, reversing a deliberate
deletion.** The bridge and tap primitives exist already (`ensureBridge`,
`ensureTap`, used for the control link). What is new is nftables
programming — a dependency kontur does not have today — with idempotent
teardown matching netshim's existing "a retried init container converges
on the same end state" discipline, plus revising several load-bearing doc
comments and the README.

**4. netshim loses its minimal-capability property under docker.**
`internal/staticpod/manifest.go:101` currently says "netshim writes no
sysctl and installs no nftables rules, so under docker it runs with
CAP_NET_ADMIN and an explicitly granted `/dev/net/tun`". That stops being
true. No change under Kubernetes, where it is privileged already.

### What NAT does not cost

- **Guest egress is unaffected** — git proxy, package registries and
  module proxies all work identically through masquerade.
- **Attribution survives.** The git proxy identifies callers by bearer
  token, not source IP; there is no `RemoteAddr` or `X-Forwarded-For`
  anywhere in `pkg/gitproxy`.
- **`kontur exec` is unaffected** — vsock, not networking. grain never
  needs inbound to a guest for the same reason.
- **The control link and memory agent are unaffected** — a separate NIC
  in either mode.
- **grain resolves no VM address at all** (`pkg/kontur`'s doc comment: the
  `PodIP`/`DockerPodIP` fields "went away" with the SSH transport), so
  nothing breaks for want of one.
- **Address allocation is close to free.** The old `-ip`/`-port` flags
  existed for a topology with several VMs sharing a namespace. kontur is
  one VM per pod, so every guest can use the same private subnet with no
  collision, and port allocation only matters for inbound, which grain
  does not need.
- **PMTU improves.** Flat is explicit that a splice has "no bridge or
  router in between to fragment an oversized frame or to answer with an
  ICMP 'fragmentation needed', so a mismatch here silently blackholes
  large packets". NAT has a router in the path.

## Alternatives, kept for the future

Neither is chosen, and both remain viable if NAT proves wrong.

### A second NIC on the netns-holder

Attach a second network to the netns-holder container; `eth0` is spliced
to the guest and `eth1` stays the container's own. netshim splices exactly
`NETSHIM_EXTERNAL_IFACE` (default `eth0`, settable per VM), and tc filters
are per-device, so the second interface is untouched.

Under docker this is a few lines in `KonturGrains.Acquire` and **no kontur
change at all** — `-docker-run-opt` exists because the holder "is the only
place a caller's own docker options can go"
(`internal/dockervm/docker.go:175`). Prefer `docker network connect` after
netshim has run over a second `--network` at create: with two networks at
create time docker does not guarantee which becomes `eth0`, and splicing
the wrong one hands the guest the wrong network *and* leaves the container
spliced.

It keeps what NAT gives up — two interfaces means two addresses, so
infrastructure-level policy can distinguish agent from sandbox — and it
adds no conntrack.

Why it is not the choice: it does not generalise. On kontur's standalone
kubelet the CNI conflist is kontur's own, but a conflist is a chain over
*one* interface, so a second NIC needs either a small custom chained
plugin (~200 lines; kontur already vendors `vishvananda/netlink`) or
Multus. On a managed cluster it is cluster provisioning — additional VPC
subnets, node pool configuration, Dataplane V2 — which is exactly the
"something to set up beside it" the decision above rejects.

**Take this if grain stays on the docker backend permanently**, where it
is strictly cheaper and strictly better-separated than NAT.

### An exec tunnel

The shim binds a loopback listener (loopback is untouched by the splice);
the agent's framework is pointed at it with a base-URL override. The
controller attaches with `docker exec -i` — never `-t`, which would break
8-bit cleanliness — and multiplexes streams over that pipe with yamux or
similar, the in-container side opening and the controller accepting.

Two variants, and the difference matters:

- **A dumb TCP tunnel** forwards bytes; the agent does TLS end to end and
  holds the credential itself. Solves connectivity only.
- **A terminating proxy** takes plain HTTP on loopback, and the controller
  adds the `Authorization` header from its own store before re-issuing
  upstream. **The credential never enters the container** — strictly
  better than any other option here. It needs the CLI to accept a base-URL
  override (`claude` and `codex` have the standard env vars; `agy` needs
  checking), so it degrades to the dumb variant per framework.

It mirrors `pkg/gitproxy`, with one simplification and one complication:
no token is needed, because the exec's far end is unforgeable — the
controller chose which container to exec into, so the pipe *is* the
authentication. But `gitproxy` buffers `UpstreamResponse.Body []byte`,
which is right for git and fatal for SSE, so the forwarder needs streaming
`io.Copy` through a flushing writer in both directions.

Why it is not the choice: it is a bespoke data plane, and on a managed
cluster every model call — full prompts and streamed responses, for every
grain at once — traverses the API server, which has its own timeouts and
connection limits. That is the opposite of simple to consume.

**Take this if NAT's conntrack behaviour bites in practice**, wherever a
cluster's CNI cannot be modified and NAT's privileged path is unavailable,
or wherever keeping flat mode is worth a bespoke data plane. That last is
the strongest case for it: a tunnelled grain needs no network at the
container layer at all, so the rule at the top of the network section
selects flat, and neither of NAT's two costs is paid.

### Routing the container's egress through the guest

Rejected, and worth recording as rejected on purpose rather than
overlooked. The guest has working network and the control link already
exists, so a route would need no new machinery, and TLS protects the
traffic end to end. But it puts the sandboxed thing in the path of the
thing sandboxing it: guest-side code could observe or block the agent's
own control channel.

## Costs and open items

1. **The trajectory rides the container's log stream**, so it needs
   tagged records to share stdout with the guest console, and the
   controller must persist what it reads — container logs rotate, so they
   carry a transcript and never store one. It also puts prompts and model
   output wherever that deployment ships container logs, which is a
   per-deployment decision rather than a detail. See "The trajectory goes
   to stdout".
2. **grain needs its own image.** kontur's final stage is `FROM scratch`
   with `ENTRYPOINT ["/usr/local/bin/kontur"]` — a node-based CLI cannot
   run there. grain ships a sandbox image: a real base, `COPY --from=kontur`
   for the binaries and guest artifacts, the agent CLIs, and `grain run`
   as entrypoint. kontur keeps its scratch image; this is grain's
   Dockerfile, not a kontur change.
3. **Verify kontur tolerates not being PID 1.** Its run mode currently
   boots the VMM as PID 1 of the container. As a child of the shim, signal
   forwarding and zombie reaping become the shim's job. Check, do not
   assume.
4. **The whole wire is a versioned contract**, not just the Spec. The
   in-container CLI, the documents crossing it and the trajectory records
   all ship in the sandbox image while the controller ships in the daemon
   binary, so a deployment can genuinely run two versions. `grain.Version`
   is stamped on every document in both directions, and a receiver that
   does not recognise one must refuse it naming both versions rather than
   interpret it on a best effort. See `docs/grain-cli.md`.
5. **`HostGrains` is not optional.** Without a backend that runs the agent
   as a plain subprocess against a directory, every test needs a VM.
6. **The mode is derived, not configured.** With both modes supported,
   which one a grain gets follows from the rule at the top of the network
   section: `KonturGrains` selects NAT when the shim needs its own egress
   and flat when it does not — so a deployment running the exec tunnel, or
   any future shape with nothing at the container layer needing network,
   gets flat without anyone choosing it. `-kontur-net` stays as an
   override for the case the derivation is wrong, not as the thing a
   deployment is expected to set.
7. **grain's own `-kontur-net` handling is broken today.**
   `cmd/grain/daemon.go:310` still offers the flag and `createArgs` passes
   it through, so `-kontur-net nat` would make *every* `vm create` fail
   against current kontur; `-kontur-base-ip` and `-kontur-base-port` feed
   flags that are now silently ignored. Worth fixing regardless of any of
   this — and the base-ip/base-port pair stays unnecessary even once NAT
   returns (one VM per namespace, so every guest can share a private
   subnet).

### Asks of kontur

The first blocks the sandbox image; the other two are small and do not.

- **Restore NAT as a selectable mode, beside flat rather than instead of
  it.** This is the network decision above, and the one piece of this
  proposal that cannot be built entirely inside grain. Flat stays the
  default and stays unchanged: a VM whose container needs no network of
  its own should keep the spliced datapath, and this asks nothing of those
  deployments. `-net` already exists and already rejects `nat`
  (`internal/cli/vm.go:245`), so this restores meaning to a flag rather
  than adding one.

  Scope: bridge and tap (primitives exist in `ensureBridge`/`ensureTap`),
  the `ip_forward` write, nftables masquerade with idempotent teardown
  matching netshim's existing convergence discipline, and the egress rules
  that keep the sandbox from inheriting the agent's reach. See "What NAT
  costs" for what to get right.
- Promote `internal/execwire` and a thin client to `pkg/`, so a
  co-located shim can dial the guest without forking `kontur exec`.
- Document whether the VMM run mode is PID-1-agnostic (item 3 above).

## Migration

`pkg/model`, `pkg/dispatch`, `pkg/github`, `pkg/gitproxy`, `pkg/ui` and
every non-dispatch reconciler are untouched. This is a refactor of the run
path and the agent's location, not of the task model.

1. `pkg/grain` — the interface and `Reconcile`. **Done**, in this commit.
2. `HostGrains` — agent as a local subprocess, no VM. Proves the interface
   and the decision table against the existing suite.
3. The controller loop — `Tick` over `List` + `Reconcile`, alongside the
   existing dispatch path behind a flag.
4. kontur's NAT mode as a second selectable mode (the blocking ask
   above), then the in-container `grain` CLI and the sandbox image. Steps 1–3 do not wait
   on it: `HostGrains` needs no network of its own, so the interface and
   the controller loop can be proven while that lands.
5. `KonturGrains`.
6. Delete: `recreate.go`, `orphan.go`, `recover.go`, `InFlight`,
   `runOne`, `RunDispatch`'s sandbox half, `pkg/ui/sandbox_recreate.go`.
