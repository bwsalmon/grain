# Grains: moving the agent next to its sandbox

> **Proposal.** Nothing in this document ships yet. `pkg/grain` carries
> the interface and the controller's decision table (`Reconcile`) as
> compiling, tested Go; everything else here is the argument for them and
> the work they imply. The open question in "The network problem" gates
> the rest.

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
│  grain-shim (PID 1)                                         │
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

It also makes a distinction the current code cannot express. `PlaceFile`
has exactly one destination, so every capability a task is granted lands
in the sandbox — including the model-facing keys the agent itself needs.
`grain.Placement` carries a `Dest`, and those go container-side.

The residual: the agent process can read its own token and could write it
into the guest. That is unchanged from today, and it is not what the VM
boundary defends against.

## The line: local versus controller

**A tool is local to the grain iff it touches only the sandbox. It is a
controller request iff it touches the store, GitHub, or a human.**

| tool | today | as a grain |
| --- | --- | --- |
| `run_command`, `read_file`, `write_file`, `edit_file` | `docker exec` → `kontur exec` | local, vsock |
| `recreate_sandbox` | MCP → daemon REST → registry → four `restore*` | local kontur call |
| `update_status` | MCP → daemon REST → store write | `status.json`, read on poll |
| `open_pull_request` | MCP → daemon REST | `Request` / `Answer` |
| `pull_request_status`, `wait_for_checks` | controller's GitHub client | `Request` / `Answer` |
| `ask_question`, `request_secret` | deferred into the result | `Request` / `Answer` |
| `comment_on_issue`, `propose_task`, `add_review_comment` | deferred into the result | `Request` / `Answer` |

Two consequences worth stating separately.

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
`Spec.Placements[DestGuest]`, redo the checkout". No registry, no re-mint,
no cross-goroutine coordination.

Two more things move inside by the same rule:

- **`ConfigureGitCredentials`** comes off the sandbox interface and
  becomes `Spec.GitToken` plus the shim. The controller still mints it —
  it is the proxy's token — and revokes it at reap.
- **`prepareCheckout`** (~500 lines of `checkout.go`, currently cloning
  through MCP round trips) moves into the shim, driven by the Spec.

## Why polling

Every method on `Grain` is idempotent, none blocks on the work, and
`Observe` returns the whole of what can be seen rather than a delta. The
controller compares that answer to what it wants and issues at most one
round of actions per tick. Level-triggered — the same discipline
`orchestrator.Reconciler` already states: running one is always safe, and
skipping one costs latency rather than correctness.

The direction matters as much as the shape. **The controller reaches in;
the grain never reaches out.** A grain that cannot be polled is a grain
that has failed, and that is a state the controller can act on. A grain
whose push failed is silence, which it cannot tell from health.

Three things fall out that are not obvious:

1. **Reattach stops being a special case.** Identity is derivable
   (`dispatch.RunID`), state lives in the container, and `List` runs every
   tick — so controller restart is the ordinary path. `orphan.go`,
   `recover.go`, `InFlight` and `drainInFlight` all go, along with
   `runOne`'s detached-context cleanup.
2. **Tool calls get an order of magnitude faster.** Per `read_file`, today
   is *fork docker CLI → dockerd RPC → `kontur exec` → vsock → guest*. In
   the container it is *fork `kontur exec` → vsock → guest*, and a bare
   socket dial if kontur promotes `internal/execwire` out of `internal/`.
   The docker CLI spawn and daemon round trip were the expensive part.
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

1. `Create(Spec)` — no prompt. The grain boots, places, clones, runs the
   repo's setup command, and reports `PhaseProvisioned` with the checkout
   facts in `Status.Checkout`.
2. The controller polls, assembles the prompt — folding in anything a
   human added since dispatch — and `Signal`s it.
3. `PhaseRunning`.

It costs one tick on an hour-long run. It buys two things: a checkout
failure is diagnosed before a single model token is spent, and
addenda-since-dispatch fold in for free instead of waiting for the next
attempt.

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
| 7b | `running` / `blocked` | live | `answer` each request; `signal(addenda)`; mirror activity |

Note what row 2 does *not* have: a repair path. A wedged guest never
reaches the controller — the shim rebuilds it — so `PhaseLost` means the
whole grain is gone and there is nothing left to ask.

The `pkg/orchestrator` equivalent is `runOne` plus `RunDispatch`, ~730
lines whose behaviour can only be observed by dispatching a real run.

## The network problem

**This gates everything above.** `internal/netshim/setup.go` in kontur:

> The external interface keeps its address: **the splice steals the
> interface's ingress**, so the namespace's own stack can never receive a
> reply and cannot hold a connection over it…

In flat mode the container keeps its address cosmetically only. Egress
goes to the veth peer; replies go to the tap, and thence to the guest. And
grain defaults to flat — `cmd/grain/daemon.go:310`.

That is harmless today, because nothing in the container needs network.
The moment the agent CLI moves in, it needs `api.anthropic.com` and cannot
reach it. Four ways out:

**A. Tunnel the model API over the exec channel.** *(recommended)* The
shim dials a container-local listener; the controller attaches over
`docker exec` stdio and proxies out. Exactly the `pkg/gitproxy` pattern,
already proven here for git. Keeps flat mode, keeps container egress at
zero, and the model credential never leaves the controller — per-grain and
revocable, strictly better than shipping a copy into each container. Cost:
a long-lived exec per running grain. That is a data plane alongside the
polled control plane, reconnectable on any tick, but it is the one piece
that is not poll-shaped.

**B. Switch to NAT mode.** The namespace keeps its own stack and the guest
sits behind DNAT/masquerade, so container egress just works.
`-kontur-base-ip` and `-kontur-base-port` (`daemon.go:318`) already exist
for the per-VM allocation NAT needs. Simplest change; loses flat mode's
"the guest is an ordinary container on the segment" property, which grain
chose deliberately.

**C. A second NIC on the pod** — container keeps one, guest takes the
other. Correct, but Multus/CNI chaining on the Kubernetes side and nothing
equivalent under plain docker.

**D. Leave the agent on the controller.** The null option, worth keeping
on the table: the win here is latency and isolation, not correctness.

Recommendation: **A**, falling back to **B** if the long-lived exec proves
fragile.

## Costs and open items

1. **Live transcript costs a tick.** Poll-tail is one exec per watched
   grain per poll. Tail only grains a UI has open, on a faster tick; leave
   the rest alone. (It reads a container-local file, so it does not touch
   the sandbox.)
2. **grain needs its own image.** kontur's final stage is `FROM scratch`
   with `ENTRYPOINT ["/usr/local/bin/kontur"]` — a node-based CLI cannot
   run there. grain ships a sandbox image: a real base, `COPY --from=kontur`
   for the binaries and guest artifacts, the agent CLIs, and `grain-shim`
   as entrypoint. kontur keeps its scratch image; this is grain's
   Dockerfile, not a kontur change.
3. **Verify kontur tolerates not being PID 1.** Its run mode currently
   boots the VMM as PID 1 of the container. As a child of the shim, signal
   forwarding and zombie reaping become the shim's job. Check, do not
   assume.
4. **The Spec is now a versioned contract.** The shim ships in the image
   rather than the daemon's binary, so a deployment can genuinely run a
   controller and an image that disagree. `SpecVersion` exists for that: a
   shim handed a version it does not know must refuse and report
   `setup-failed` naming both, never interpret it on a best effort.
5. **`HostGrains` is not optional.** Without a backend that runs the agent
   as a plain subprocess against a directory, every test needs a VM.

### Asks of kontur

Neither blocks starting; both are small.

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
4. Settle the network question, then `grain-shim` and the sandbox image.
5. `KonturGrains`.
6. Delete: `recreate.go`, `orphan.go`, `recover.go`, `InFlight`,
   `runOne`, `RunDispatch`'s sandbox half, `pkg/ui/sandbox_recreate.go`.
