# Grains: options considered

Companion to [`grain.md`](grain.md), which says what was decided. This
holds what was weighed against it: the paths not taken, the conditions
that would reopen them, and the places the reasoning changed on contact
with the code.

It exists so that a decision is not re-litigated from scratch, and so that
"we should have done X" has an entry to argue with.

## Poll versus push

Four options for how the controller learns a grain's state:

1. **Poll via exec** — `grain status` per grain per tick. *Chosen.*
2. **Push with a credential** — the shim holds a token and posts to the
   daemon's REST API. This is what grain does today for `update_status`,
   `open_pull_request` and `recreate_sandbox`.
3. **Attached stream over exec** — a persistent exec per grain, exec
   transport with push semantics.
4. **Poll via shared volume** — the shim writes `status.json` to a
   hostPath the controller reads directly.

**Push forces NAT, and the network decision deliberately keeps flat
available.** Under flat the splice steals the container's ingress, so it
can send but never establish a connection — there is no HTTP push from a
flat-mode grain at all. Choosing push would close a door the network
section leaves open. This is the argument specific to this design rather
than to polling in general.

**Silence is information under poll and ambiguous under push.** An
unreachable grain is `PhaseLost` with a rule for it; a grain that stopped
pushing might be dead, wedged or idle with nothing to tell them apart. So
push systems grow a heartbeat, which is a poll rebuilt with the failure
detector inside the thing it monitors.

Beneath that: **poll is level-triggered and push is edge-triggered, and
edges get lost.** A push landing while the controller restarts is gone, so
push needs retry, at-least-once, idempotency keys and dedupe. `Reconcile`
rests on level-triggering entirely.

### What push would have bought

Recorded so neither is rediscovered as an argument.

**Latency**, in exactly one place. `ask_question` waits on a human;
`open_pull_request` and `wait_for_checks` are fine at tick granularity;
cancellation is controller-to-grain and push-shaped already. The one
signal that suffers is the live trajectory — handled by container logs
instead.

**Scale.** Poll is O(grains × ticks), push is O(events). This does not
bite at a single-operator cluster's size. **If it ever does, the answer is
a node-local aggregator the controller polls once per tick, making it
O(nodes) — not push.** Written down so "polling does not scale" is not
later read as "we should have pushed": the aggregator keeps every property
above, and push forfeits all of them.

### The credential objection, disclaimed

It would be easy to argue against push because it needs a credential in
the container. That argument is weak. The container is the *trusted* side —
untrusted code is in the guest, behind vsock — so a token there sits beside
the model credential already present.

The real cost is the **authorization surface**: what a token may do, scoped
to which grain, minted when, revoked when, and what happens when a grain
claims to be a run it is not. `pkg/gitproxy` is ~1,900 lines and much of it
is exactly that. Not danger; work, and a place bugs live. Push also
reinstates the daemon hops the local-versus-forwarded rule had just
finished deleting.

### Transport is not the interface

`Observe` says nothing about how a status is fetched, which leaves option 4
available as an implementation detail. On the docker backend the controller
and its grains share a host, so reading a hostPath is strictly cheaper than
a `docker exec` with identical semantics; it stops working across nodes.
`KonturGrains` may choose it; nothing above it needs to know.

## Why the control channel is not itself MCP

The traffic is MCP tool calls and grain already speaks MCP, so it is fair
to ask why this is a CLI.

**MCP assumes a session; `Reconcile` assumes there is not one.** MCP's
transports — stdio and Streamable HTTP — open with an `initialize`
handshake negotiating protocol version and capabilities before any other
request. Per poll that is three messages before the controller learns
anything, on a channel where each round trip is a container exec, against
one exec returning one document. Held open instead, it is the persistent
connection "poll, not push" rejected.

Level-triggering is the deeper mismatch: a session is state shared between
two parties, which is what that decision removed.

**There are two relationships and they are not the same shape:**

| | shape | protocol |
| --- | --- | --- |
| agent ↔ shim | one session for the run's life | **MCP** (`pkg/mcp`, unchanged) |
| controller ↔ shim | independent level-triggered calls | the CLI |

What MCP has that we would otherwise have invented is already taken from
elsewhere or better as it is: version negotiation is the wire version plus
image labels (Kubernetes' string form rather than `initialize`'s); error
codes are exit codes, which `docker exec` propagates for free and which we
need anyway to tell exec-failed from shim-failed; progress notifications
are the container log stream, which survives a disconnected controller and
replays from `--since`.

> Checked against the spec was not possible from this session —
> `modelcontextprotocol.io` is blocked by the egress proxy — so the
> transport details above are from training and worth confirming.

### Open option: MCP frames as the trajectory

`src: "agent"` records have no defined vocabulary, and each of
`pkg/agent/claude`, `/antigravity`, `/codex` owns a transcript reader
because "the two event vocabularies differ".

The shim sits between the agent's MCP client and its own server, so it
could mirror that JSON-RPC into the trajectory verbatim as the `agent`
records — one well-specified vocabulary across every framework, at no cost
to produce. It covers tool calls but not the model's prose, so the
per-framework readers do not fully disappear.

## The network

The container needs egress because the agent moved into it. Four ways,
and one rejected on purpose.

### B. NAT — *chosen*

Bridge plus masquerade in the pod netns, `eth0` unspliced. Works in every
environment with no cluster provisioning, which is the deciding property.
Costs are in [`grain.md`](grain.md); the two that matter are the loss of
infrastructure-level differentiation and conntrack as a new failure class.

Also true, and worth recording so it is not rediscovered as an objection:
**guest egress is unaffected** (git proxy, registries, all work through
masquerade); **attribution survives**, since the git proxy identifies
callers by bearer token, not source IP — there is no `RemoteAddr` anywhere
in `pkg/gitproxy`; **`kontur exec` is unaffected**, being vsock; **grain
resolves no VM address at all**, so nothing breaks for want of one; and
**address allocation is close to free**, because kontur is one VM per pod
so every guest can share a private subnet — the old `-ip`/`-port` flags
were for a topology with several VMs per namespace. **PMTU improves**: a
splice has "no bridge or router in between to fragment an oversized frame",
where NAT has a router in the path.

### C. A second NIC on the netns-holder

Attach a second network to the netns-holder container; `eth0` is spliced to
the guest and `eth1` stays the container's. netshim splices exactly
`NETSHIM_EXTERNAL_IFACE` and tc filters are per-device, so the second
interface is untouched.

Under docker this is a few lines in `KonturGrains.Acquire` and **no kontur
change at all** — `-docker-run-opt` exists because the netns-holder "is the
only place a caller's own docker options can go"
(`internal/dockervm/docker.go:175`). Prefer `docker network connect` after
netshim has run over a second `--network` at create: with two networks at
create time docker does not guarantee which becomes `eth0`, and splicing
the wrong one hands the guest the wrong network *and* leaves the container
spliced.

It keeps what NAT gives up — two interfaces means two addresses, so
infrastructure policy can distinguish agent from sandbox — and adds no
conntrack.

**Why not:** it does not generalise. On kontur's standalone kubelet the CNI
conflist is kontur's own, but a conflist is a chain over *one* interface, so
a second NIC needs a small custom chained plugin (~200 lines; kontur already
vendors `vishvananda/netlink`) or Multus. On a managed cluster it is cluster
provisioning — additional VPC subnets, node pool configuration, Dataplane
V2.

**Take this if grain stays on the docker backend permanently**, where it is
strictly cheaper and strictly better-separated than NAT.

### A. An exec tunnel

The shim binds a loopback listener (loopback is untouched by the splice);
the agent's framework is pointed at it with a base-URL override. The
controller attaches with `docker exec -i` — never `-t`, which would break
8-bit cleanliness — and multiplexes streams over that pipe.

Two variants, and the difference matters:

- **A dumb TCP tunnel** forwards bytes; the agent does TLS end to end and
  holds the credential itself. Solves connectivity only.
- **A terminating proxy** takes plain HTTP on loopback and the controller
  adds the `Authorization` header before re-issuing upstream. **The
  credential never enters the container** — strictly better than any other
  option. It needs the CLI to accept a base-URL override (`claude` and
  `codex` have the standard env vars; `agy` needs checking), so it degrades
  to the dumb variant per framework.

It mirrors `pkg/gitproxy` with one simplification and one complication: no
token is needed, because the exec's far end is unforgeable — the controller
chose which container to exec into, so the pipe *is* the authentication. But
`gitproxy` buffers `UpstreamResponse.Body []byte`, which is right for git
and fatal for SSE, so the forwarder needs streaming `io.Copy` through a
flushing writer both ways.

**Why not:** a bespoke data plane, and on a managed cluster every model call
— full prompts and streamed responses, for every grain at once — traverses
the API server, which has its own timeouts and connection limits.

**Take this if NAT's conntrack behaviour bites**, wherever a cluster's CNI
cannot be modified, or **wherever keeping flat mode is worth a bespoke data
plane** — a tunnelled grain needs no network at the container layer at all,
so the rule selects flat and neither of NAT's costs is paid. That last is
the strongest case for it.

### D. Routing container egress through the guest

Rejected, and recorded as rejected on purpose rather than overlooked. The
guest has working network and the control link exists, so a route would
need no new machinery, and TLS protects the traffic end to end. But it puts
the sandboxed thing in the path of the thing sandboxing it: guest-side code
could observe or block the agent's own control channel.

### Why there is no carve-out within flat

`splice.go`: each direction is a tc ingress qdisc plus a match-everything
filter with `mirred` egress-redirect, so frames reach the tap at L2 and the
namespace's IP stack never sees them. Discriminating replies-for-the-namespace
from replies-for-the-guest would need L3/conntrack-aware classification
inside what is deliberately a match-all L2 wire — and the two ends share a
MAC by design, so there is nothing to match on at L2 either.

## Configuration delivery

**Where it landed:** scalars in the environment, everything else as files
under `/grain`, all before the container starts.

**A correction on the way.** An earlier draft delivered the credential and
placements over an exec's stdin, on the grounds that an environment
variable shows up in a Kubernetes pod spec. That was wrong about how
Kubernetes does this: `valueFrom.secretKeyRef` puts a *reference* in the
pod spec while the value stays in a Secret, with its own RBAC — `get
secrets` being distinctly more privileged than `get pods` — and its own
encryption at rest. Under docker the argument was always weak: reading the
environment needs the docker socket, which is root-equivalent and can read
the process's memory regardless.

**Then files won anyway, for a different reason.** A Kubernetes Secret or
ConfigMap volume *is* the placement model — `items: [{key, path, mode}]`
gives files at chosen paths with chosen modes — so there is no encoding to
invent, no ARG_MAX ceiling, nothing in `/proc/1/environ`, and a non-secret
placement can come from a ConfigMap where an environment blob could not
have told it apart from a secret.

**What stayed in the environment** is what has no shape: the wire version,
the framework name, the max runtime, and kontur's own `CHV_*`.

**Setup as a file** rather than a string, though kontur's own
`CHV_SETUP_SCRIPT` carries its script's text — fine for a line or two, and
grain's is composed from a clone, a branch checkout, the repo's setup
command and whatever the prompt needs read back. As a file it has a
shebang, an executable bit, and something a human can `cat`. (kontur gained
`CHV_SETUP_SCRIPT_PATH` for the same reason; see its branch.)

## Placements: the destination field, removed

An earlier draft had `Placement` carry a destination so model-facing keys
could land container-side. Checking rather than assuming: `githubsandbox`,
`gcpkey` and `geminikey` are the three capabilities that place anything and
**all three place `model.SideSandbox`**. `model.SideController` exists and
nothing produces one — `orchestrator/run.go:1832` skips it, "not written
anywhere".

`geminikey` looks like a counterexample and is not: it mints a key for a
task and names the path in the prompt so the *work* can find it.

So a placement has no side to choose, and a discriminator whose second
value has never occurred is not worth carrying across a versioned wire. The
agent's own credential is deployment-shaped rather than per-run and is not a
placement at all — it has no path the controller could name, which is
exactly what distinguishes it.

## Deadlines: the lease, declined

`GRAIN_MAX_RUNTIME` is enforced by the grain. The argument that survives is
spending: a running agent costs money, and money should not keep leaving
while a controller is down. (The argument that did not: "ends with a
`Result` rather than being destroyed mid-thought" — cancellation already
provides exactly that.)

What it does not cover: a controller dying five minutes into a two-hour
budget still leaves an hour fifty-five of spending. **A lease** — the grain
stopping if nobody has polled it in a while — bounds that under *any*
controller failure, and needs no policy in the grain at all ("nobody has
asked how I am doing" rather than "my deadline is 14:32").

**Declined** as more mechanism than the failure justifies, and with a
failure mode of its own: a controller restart longer than the lease would
kill healthy in-flight grains. **Worth revisiting if unattended controller
death turns out to be a real operational event rather than a hypothetical.**

## What the Spec shed

It began with a task, a repository, a branch, a git credential, a capability
grant list and three limits. All of it went, on one principle: a grain runs
an agent in a sandbox and knows nothing about why.

- **task, repo, branch** → the prompt and the setup script. A shim that
  understood repositories would have to agree with the controller about
  branch naming, proxy URLs and half-made checkouts.
- **gitToken** → a placement. The same work `ConfigureGitCredentials` does,
  said uniformly.
- **grants** → nothing. Both capabilities only ever made content readable —
  grain's own source, the embedded runbooks — and the sandbox image is built
  from that source and that binary, so what remains is a line in the prompt.
- **maxTurns** → a framework's own flag. `Config.MaxAgentTurns`' doc comment
  already concedes both frameworks default to no cap and "what actually
  bounds a runaway run is MaxRunRuntime".
- **maxRebuilds** → `Policy.MaxRebuilds` alone; the controller has the view
  of whether repair is converging.
- **id** → the container is the identity. A controller execs into one
  specific container, so a grain is never told a name it makes no use of.
- **the tool vocabulary** → the controller's own MCP server.
  `ToolOpenPullRequest` and friends were grain-the-product's names sitting
  inside grain-the-sandbox-runner; the mounted declarations that briefly
  replaced them went too (see "Forwarding calls, replaced by a real MCP
  connection").

### The two-phase start, retired

For a while the prompt was delivered by signal once the grain reported
`PhaseProvisioned`, because `previousAttemptsSection` names the commits
earlier attempts pushed and those were read from the checkout.

Retired once it was clear the controller can ask **GitHub** for a branch's
commits, which it already does every cycle. That deleted `PhaseProvisioned`,
`SignalPrompt`, `RunRow.PromptSent` and a reconcile row. What the two-phase
start was really protecting is untouched: `Setup`'s exit code still gates
starting the agent, so a failed checkout still spends no model tokens.

### The deferred category, retired

`comment_on_issue` and friends were to be answered locally with a canned
acknowledgement and reported at the end. The shim cannot know which tools
those are without understanding them — and forwarding them is better
anyway: the agent waits one tick and gets the real result instead of a
confirmation for an effect that happens later or not at all. Today's mocked
tools essentially lie to the agent; this stops.

## Versioning: what was taken from Kubernetes

`apiVersion` + `kind` on every object, forming the group/version/kind an API
server dispatches decoding on, with grade-carrying string versions.

**Taken: the string.** It lets a wire that is still a proposal say
`v1alpha1`, and it matches the comparison this needs — "refuse what you do
not recognise" is set membership, where an integer invites `>=`, which is
exactly the best-effort interpretation the rule forbids.

**Not taken: `kind`.** Kubernetes needs it because an API server decodes an
object without having been told what it asked for; here the subcommand is
the kind. The one channel carrying mixed documents is the trajectory
stream, where `src` already says which is which.

**Not taken: the group prefix, or per-kind versions.** A group routes
between vendors and there is one of those here; and Kubernetes versions per
kind because its kinds belong to different API groups on different release
cycles, where all of these ship in one binary.

**The `version` subcommand, retired** in favour of image labels. A
controller wants to know what an image can do *before* it creates a grain —
to refuse a task naming a framework the image lacks — and asking a grain
requires a grain to exist.

## The trajectory: from held-open exec to logs

An earlier draft gave the trajectory an exec held open for as long as
somebody watched. Container logs are better on four counts: the runtime
buffers, so a controller restarting mid-run resumes from `--since` rather
than losing what it missed; nothing backpressures the agent, where a
controller that stopped reading a held-open exec would eventually block the
shim's write and let an open UI tab stall a run; replay is the same call as
the tail; and wherever container logs are already collected, the trajectory
arrives with them.

Sharing the stream with the guest console makes tagged records mandatory —
`kubectl logs` merges stdout and stderr, so splitting by fd works under
docker and nowhere else — which turns the collision into something useful:
a run killed by the provisioning budget can quote the last console lines.

## Removing the status exec: what was checked

Once status was the only thing costing an exec per grain per tick, the
question was whether a native runtime path could carry it instead.

**Docker `HEALTHCHECK`** is the real candidate: the daemon runs a command
on its own schedule and `docker inspect` exposes `.State.Health.Log[]` with
each probe's exit code and output, so a healthcheck running `grain status`
would put the document where a plain inspect reads it, with the daemon
doing the exec. **Rejected**: docker-only, output capped at a few KB — which
a status carrying a call's arguments can approach — and a schedule fixed at
create rather than chosen by the shim.

**Kubernetes has no equivalent.** Probes are boolean; a probe's output
surfaces only in an Event, only on failure, truncated. Pod annotations
would need the container to call the API, which is push and needs a
credential. The termination message is terminal-only, and already taken.
That asymmetry decided it, the same way it decided the second-NIC option
for the network.

**What was taken instead: the log stream**, which works identically on both
and which the controller already tails. Status snapshots as `kind: "status"`
records make `List` exec-free in the steady state, stay level-triggered
because each is a full snapshot, and keep absence meaningful because
container state still comes from the runtime listing.

`grain status` stays as the fallback rather than being deleted — it costs
one subcommand of a binary that already exists and buys the ability to ask
when a stream has gone stale. Honestly, it is less independent than it
looks: on Kubernetes exec and logs both go through the API server and
largely fail together, so it is a genuine second route only under docker.

## Can stdout and stderr be told apart?

**Docker: yes.** Each entry is tagged with its stream and the API can
return them separately. **Kubernetes: no.** The CRI log file format carries
the stream per line, but the pod log API returns only the message; a
node-local log agent could see it, nothing through the API server can.

So fd-splitting works under one backend and not the other, and the source
tag lives in the record instead. The discipline it *does* buy is worth
keeping regardless: **stdout is records only, stderr is the shim's
human-facing diagnostics** — otherwise a stray warning or panic trace is
indistinguishable from a damaged record.

This also corrected an earlier description. The guest console does not
share the stream raw: kontur is the shim's child, so its console output is
captured and re-emitted as `src: "console"` records. Wrapping is what makes
a console line addressable at all, which is what a `setup-failed` detail
quoting the last few lines depends on.

## Cancellation, and the signal verb that went with it

Cancelling was a `Signal` the grain was expected to act on, after which
the next poll would see a terminal phase and release it. **Killing the
container does the same thing in one tick**, because stopping a container
SIGTERMs the shim and waits out a grace period — so the graceful ending is
what killing already does, and kontur's own `ShutdownTimeout` plus
`terminationGracePeriodSeconds` already establish the pattern.

That took `SignalCancel` and `SignalPause` with it, leaving `addenda` as
the only signal — and `addenda` has no consumer.

### How addenda would reach an agent, if it ever did

Worth keeping, because the mechanism is not obvious and the analysis was
done. A comment added mid-run has to become a new user turn, and there are
three ways:

1. **Piggyback on the next tool result.** The shim intercepts every MCP
   call, so it can append a delimited note to the next result the agent
   receives. Works with *every* CLI, needs no capability from any of them,
   and arrives within seconds for an active agent. Out-of-band content on
   an in-band channel, which is a hack — but a well-understood one, and it
   crosses no new trust boundary, since that human's comments already
   reach the agent through the prompt.
2. **A built-in tool the agent calls** (`check_messages`). Universal, but
   depends on the agent choosing to call it, and one deep in a task often
   will not.
3. **Streaming stdin.** Semantically cleanest — a real user turn. `claude`
   supports it (`--input-format stream-json`, pairing with the
   `--output-format stream-json` already at `claude.go:574`); today the
   prompt goes in as `cmd.Stdin = strings.NewReader(stdin)`, which hits
   EOF immediately, so grain is one-shot by construction rather than by
   limitation. Unknown for `codex`, and unlikely for `agy`, which already
   lacks `--mcp-config` and `--max-turns`.

The shape would be (1) as the baseline with (3) as a per-framework
upgrade, which is exactly what the framework profile is for — it already
knows how to launch its CLI, so "and this one takes streaming input" is a
fact of the same kind.

**Not built**, because nothing consumes addenda today:
`agent.RunConfig.Addenda`'s own doc comment says neither framework calls
it, and a comment posted mid-run waits for the next dispatch. A verb for a
capability nobody uses is dead surface on a contract between two
separately released artifacts.

**Cheap to reverse.** A new subcommand is additive: an old shim returns
exit 2, which the controller already reads as version skew. Worth
revisiting if a run ever needs to hear something without being restarted —
and note it is *more* feasible in a grain than today, since the shim owns
both the agent's stdin and the MCP channel, where today the MCP server is
a forked subprocess on the controller and nothing owns stdin at all.

## What Kubernetes gives natively, and what it does not

Checked when asking whether any of this duplicates something k8s already
does.

**`.status.containerStatuses[]`** — `state`, `ready`, `restartCount`, and
for terminated ones `exitCode`/`reason`/`message`. This is what `List`
already reads; on docker it is `docker ps`/`docker inspect`, and the
information is the same.

**The termination message** is the one genuine addition: a container
writes `/dev/termination-log` and Kubernetes surfaces it in the pod
status, so a finished grain's outcome arrives with the listing rather than
needing an exec. Taken.

**Probes are the wrong tool.** They are binary, they exist for the kubelet
to act on rather than for an external reader, and liveness in particular
*restarts* — which a grain must never be. A readiness probe could surface
"provisioned" in the pod listing for free, letting `List` skip the exec for
still-provisioning grains; marginal, and recorded rather than built.

**What Kubernetes does not give** is mid-run rich state — activity, the
outstanding call, `seq`. There is no native "ask the container what it is
doing" short of exec, so the poll stays.

## How the agent reaches the controller: three designs

**1. Forwarding over a poll.** The shim held a tool call it could not
serve, surfaced it as `status.call`, and a `grain answer` verb settled it.
MCP over a poll. It re-implemented request/response over a channel not
built for it, and needed a spool, ids, acknowledgement and an
at-most-one-outstanding rule.

**2. An exec-attached MCP server.** The controller attached by exec'ing
`grain proxy` and served MCP over that pipe; the shim terminated the
agent's session, merged tool lists, and held calls while detached.

Better, but the failure mode shaped it and was not cheap. A shim that
merely piped bytes would lose the agent those tools **for the rest of the
run** on the first drop — MCP is session-oriented, a dead transport takes
the session with it, and clients generally mark such a server failed
rather than re-initializing. Terminating the session insulated against
that, at the cost of: a hold buffer, replayed calls on reattach (so
upstream servers had to tolerate seeing one twice), and a wait for the
first attach before the agent could start, because a tool list is fixed at
`initialize`.

**3. Streamable HTTP, direct.** *Chosen.* The container has a working
stack under NAT, so the agent reaches the controller's endpoint itself and
the shim takes no part at all.

MCP's remote transport already solves what design 2 was hand-rolling: a
session id keeps state across requests, and resumable SSE (`Last-Event-ID`)
replays what a client missed. So a drop is the protocol's problem. Gone
with it: `grain proxy`, `SocketUpstream`, `Grain.Attach`,
`Status.Upstream`, `ActionAttach`, the hold, the replay hazard, the
wait-for-first-attach rule, the merging server, and `PhaseBlocked` — the
shim cannot see an agent waiting on an HTTP request it made itself, and
does not need to, since the controller is the far end and knows better.

### What made it work: the token already exists

An exec pipe authenticates by construction — the controller chose which
container to exec into. An address does not, and needing "which grain is
calling" is exactly the authorization surface argued against for push.

It is already built. `gitproxy.SandboxTokenStore.EnsureToken` mints a
per-grain token, `Revoke` drops it at reap, and `model.Store.GitScope`
resolves it to a live run. The MCP endpoint is one more consumer, not a
second surface. The same secret lands twice — container-side at
`/grain/token` for the agent, guest-side as a placement for git — which is
one value with two consumers on two sides of the vsock boundary.

### What it costs

**A network dependency between grain and controller** that exec did not
have. Under NAT the container has a stack anyway (that is why NAT was
chosen), and under flat the model-API tunnel carries TCP, so the same
local listener serves both — it does not force the network decision.

**Per-framework HTTP MCP support.** `claude` takes URL-type servers; agy
and codex want verifying. A CLI that speaks only stdio would need the shim
to bridge for that framework alone, which is design 2 for one case.

### And the message-tool design, considered

Briefly: drop MCP entirely and give the agent one built-in that leaves a
message with an id, optional JSON and freeform text, then exits — the
controller reads it and decides what to do, retrying or parking by id. It
would have made a grain a single-shot function needing no controller at
all while it runs.

Rejected because it loses per-tool schemas (models produce better-formed
arguments given one) and, more concretely, because reacting to CI would
cost a full redispatch — VM boot, clone, re-derivation — where
`open_pull_request` plus `wait_for_checks` do it inside one run today,
deliberately: "instead of exiting blind and leaving the pull request to
orchestrator's finish path." MCP exists for this, and reinventing a
narrower version of it is not a saving.

## What Kubernetes gives natively, and what it does not

Checked when asking whether any of this duplicates something k8s already
does.

**`.status.containerStatuses[]`** — `state`, `ready`, `restartCount`, and
for terminated ones `exitCode`/`reason`/`message`. This is what `List`
already reads; on docker it is `docker ps`/`docker inspect`, and the
information is the same.

**The termination message** is the one genuine addition: a container
writes `/dev/termination-log` and Kubernetes surfaces it in the pod
status, so a finished grain's outcome arrives with the listing rather than
needing an exec. Taken.

**Probes are the wrong tool.** They are binary, they exist for the kubelet
to act on rather than for an external reader, and liveness in particular
*restarts* — which a grain must never be. A readiness probe could surface
"provisioned" in the pod listing for free, letting `List` skip the exec for
still-provisioning grains; marginal, and recorded rather than built.

**What Kubernetes does not give** is mid-run rich state — activity, the
outstanding call, `seq`. There is no native "ask the container what it is
doing" short of exec, so the poll stays.

## Forwarding calls, replaced by a real MCP connection

For a while the controller was an out-of-band executor: the shim held a
tool call it could not serve, surfaced it as `status.call`, and a `grain
answer` verb settled it. **MCP over a poll.**

Replaced by the controller attaching an actual MCP server, because the
poll version was re-implementing request/response over a channel that was
not built for it. What that deleted: `Status.Call`, `Call`, `CallID`,
`Answer`, the "at most one outstanding" serialisation, `Status.Consumed`
and the spool, the `answer` verb, `ActionAnswer` — and `/grain/tools/`,
`ToolDecl` and `checkTools` with them, since a server advertises its own
`tools/list` and a mounted schema was only ever a second copy to keep in
sync.

### The failure mode that shaped it

The obvious version — the shim as a dumb byte pipe with a tee into the
trajectory — is wrong, and badly. MCP is session-oriented: a client
initializes once and a dead transport takes the session with it, and
clients generally mark such a server failed rather than re-initializing.
So a dropped pipe would cost the agent those tools **for the rest of the
run**, an hour in and silently, rather than for a tick.

Hence the shim terminates the agent's session and merges tool lists,
holding upstream calls while detached. The agent's connection is to the
shim, and the shim never closes it.

That recovers a small part of what forwarding was — an in-memory hold —
but not the expensive part: no spool, no status surfacing, no answer verb,
no ids to acknowledge.

### What it costs

**A grain cannot start without a controller.** A tool list is fixed at
`initialize`, so the shim must know the upstream tools before the agent
runs, which means waiting for the first attach. The alternative was
keeping mounted declarations so the shim is independent at start; waiting
was preferred because the controller has just created the grain (the
window is milliseconds), a grain that cannot reach a controller cannot do
its job anyway, and a controller dying before attach leaves the grain in
provisioning until `ProvisionBudget` — an ending that already has a rule.
A grain *does* survive a controller dying mid-run, on the cached list.

**At-least-once did not disappear, it moved.** A connection can drop after
an upstream server acted and before its answer arrived, so replaying a
held call can double-execute. That machinery moved from the wire into the
connection, where it is cheaper — in-memory, no `status.consumed` — but it
is not free. Naturally idempotent tools are better where achievable, and
grain's pull request path already is (`EnsurePullRequest`, find-or-create).

**Forwarding survived an absent controller and a connection does not.**
Under forwarding a grain blocked and was answered whenever *a* controller
returned, even a different process — the reattach property. Now calls
merely wait for the next tick's reattach, which is nearly the same thing,
but not identically so.

### What it bought

Real MCP, end to end, with immediate latency — `wait_for_checks` and
`ask_question` work by the server simply not answering yet, with no policy
in the wire. And an extension story that is one sentence: **write a plain
stdio MCP server, list it in the controller's config, and its tools appear
to every agent.** That server is unaware of the proxy, the container or
the VM; per-run context reaches it as launch arguments, because the
controller starts one instance per grain — which is how MCP servers are
scoped anyway.
