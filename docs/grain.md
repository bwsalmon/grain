# Grains: moving the agent next to its sandbox

> **Proposal.** Nothing in this document ships yet. `pkg/grain` carries
> the interface and the controller's decision table (`Reconcile`) as
> compiling, tested Go; everything else here is the argument for them and
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
  that keep the sandbox from inheriting the agent's reach. See "What it
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
   above), then `grain-shim` and the sandbox image. Steps 1–3 do not wait
   on it: `HostGrains` needs no network of its own, so the interface and
   the controller loop can be proven while that lands.
5. `KonturGrains`.
6. Delete: `recreate.go`, `orphan.go`, `recover.go`, `InFlight`,
   `runOne`, `RunDispatch`'s sandbox half, `pkg/ui/sandbox_recreate.go`.
