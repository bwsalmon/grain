# Rebuilding grain on beads and incus

## Verdict

Two independent decisions that happen to have been asked together.

- **Incus: adopt.** It is a third `HostAdapter` driver behind an interface
  built for exactly this, and it answers an objection this project has
  already run into — the cost of [a sandbox per
  task](data-model.md#direction-a-sandbox-per-task) — with sub-second
  snapshot restore. It also collapses the "container runtime on the
  controller" question, because containers and VMs are one API. The real
  cost is portability: it forecloses the macOS future `docs/design.md`
  revision 5 was arranged around.
- **Beads: take the ideas, not the store — for now.** It is a genuinely
  good fit for the half of the model this document already converged on
  independently (dependency graph, hierarchical sub-tasks, derived
  ready-work). The open risk is whether its source of truth is reviewable
  as a git diff, which is the property that motivated moving the
  declaration into a repo at all. That has changed between versions and
  needs verifying against whatever release would be adopted.

**Confidence.** The incus assessment rests on published behaviour plus
grain's own adapter interface, which is in this repo. The beads
assessment rests on documentation that **disagrees across versions** —
see [what needs verifying](#what-needs-verifying-before-committing).
Nothing here has been run.

## Incus

### What it is, against what grain already has

`HostAdapter` (`grain/adapter/base.py`) is an ABC with six abstract
methods — `create`, `start`, `stop`, `destroy`, `state`, `list_vms` —
plus `network_up`, `egress_policy`, `address` and `recreate` supplied
concretely. There are two drivers today: `libvirt.py` (454 lines) and
`lima.py` (141).

So "rebuild grain on incus" is, in scope terms, **a third file
implementing six methods**. That is not luck; `docs/design.md`'s central
argument for revision 5 was confining the platform-specific surface to
one module so a substrate change costs a driver rather than a rewrite.
This is the first real test of that claim, and the claim holds.

### Why it is a good fit

**One API for containers and VMs.** Incus manages system containers and
virtual machines through a single REST API, over a Unix socket locally.
That matters more than it first appears, because this project has two
separate needs that currently point at two runtimes:

- sandbox **VMs**, which must be VMs — `docs/design.md` is explicit that
  each agent gets a whole VM "because the workload runs `docker` and
  `kind`, which do not nest into containers";
- capability **containers**, the
  [proposal](data-model.md#capabilities-as-containers) whose main
  objection was that it puts a container runtime on the VM holding every
  credential.

With incus that objection largely dissolves. The runtime is not a *new*
dependency if it is already the thing managing the VMs — one daemon, one
API, one set of images, rather than QEMU plus Docker.

**Sub-second snapshot restore answers the sandbox-per-task cost.** On
btrfs or ZFS storage backends, incus snapshots are cheap to take and
restore in under a second with near-zero initial space. That is precisely
the mitigation [a sandbox per
task](data-model.md#direction-a-sandbox-per-task) needs: provision a
golden image once, snapshot it, and restore per task instead of paying a
boot and a provision on every dispatch. It makes the strongest argument
against per-task recreation — cost on the critical path — mostly go away,
with a mechanism rather than a workaround.

On `dir` storage the property is absent: snapshots are full copies. So
the storage backend is not an implementation detail here, it is the
feature.

**Same substrate, no new isolation story.** Incus VMs are QEMU/KVM, which
is what the libvirt driver already drives. The trust boundaries in
`docs/design.md` — separate kernels, separate Docker daemons, separate
port spaces — are unchanged. Nothing about the credential model, the git
proxy, or the sandbox token needs rethinking.

**Packaging fits the target.** Native `incus` packages ship in Debian 13
(trixie), tracking the Incus LTS releases; Zabbly, maintained by the
project lead, covers Debian 12 and 11. grain's host and guests are
Debian. Apache 2.0, no CLA.

### What it costs

**It forecloses macOS, which was the point of revision 5.**
`docs/design.md` chose Lima specifically because it "runs on Linux as
well as macOS, so the same templates, the same provisioning scripts, and
the same `limactl` calls serve both — which shrinks the port to
*networking alone*." Incus is Linux-only. Adopting it as the primary
driver trades that away.

Two things soften it and neither erases it. The adapter interface means
Lima can stay as a second driver rather than being deleted, so macOS
degrades from "the plan" to "a driver somebody has to maintain." And
revision 5 already ranks Lima-on-Linux as "less travelled" with libvirt
as the Linux-native fallback — so a Linux-native driver was already
anticipated. But if the macOS future is still wanted, this is the
decision that quietly ends it, and it should be made deliberately rather
than as a side effect of liking snapshots.

**Networking is where the work actually is.** The six lifecycle methods
are the easy part. `net_linux.py` (236 lines) and the `Network` protocol
handle a bridge, a tap per VM, `nftables` egress policy, and source
pinning for [sandbox
identity](design.md#sandbox-identity). Incus has its own managed-bridge
and NIC-device model, so the question is whether grain keeps driving the
network itself and hands incus pre-made interfaces, or delegates to
incus's networking and rewrites `apply_rules`/`install_boot_unit` around
it. The second is cleaner and is the larger piece of work — bigger than
the driver itself.

## Beads

### Where it fits well, which is more than expected

Beads is a graph issue tracker for coding agents: a Go CLI (`bd`),
JSON-first output, dependencies as first-class data, and `bd ready` to
return unblocked work.

The overlap with the [task data model](data-model.md) is close enough to
be worth noting as evidence rather than coincidence:

| grain's model | beads |
|---|---|
| `LinkKind.DEPENDS_ON`, blocking, re-evaluated each cycle | dependency edges; `bd ready` computes unblocked work |
| `CHILD_OF` sub-tasks | hierarchical IDs — `bd-a3f8` → `bd-a3f8.1` → `bd-a3f8.1.1` |
| identity opaque, stable, **not comparable** | hash-based ids (`bd-a1b2`) |
| `Assignment` / claiming a slot | `bd update <id> --claim`, atomic |
| declaration vs. observation | persistent `issues` vs. ephemeral `wisps`, split at the table level |
| `Task.tags` | labels — `bd create --label`, `bd label add` |

Two of those are striking. **`bd ready` is the same derivation** this
model argues for in stating that blocked is computed from links rather
than stored as a state. And the **persistent/ephemeral table split** is
the same instinct as separating what a human authors from what grain
observes. Converging independently on both is reasonable evidence the
model is not idiosyncratic.

Beads is also extensible in the ways the model needs: labels exist, and
custom tables can sit alongside the core schema — so `folder`, `reads`,
capability grants and the rest have somewhere to live.

And it ships bidirectional sync with GitHub, Jira, Linear and others,
which is interesting precisely because it suggests a shape this project
has been circling: **beads as the graph, GitHub as the human surface**,
rather than one replacing the other.

### The risk, and why it is version-dependent

The reason to move the declaration into a repo was never storage. It was
**review**: a declaration change becomes a diff, approval becomes merging
it, and `needs_approval_label` plus `/lgtm` collapse into machinery
GitHub already has. That is [the decided
shape](data-model.md#taskstate-is-derived-not-stored) — approval is
declaration, and it carries an `Attribution`.

Whether beads preserves that depends on which version is adopted, and the
documentation genuinely disagrees:

- The current upstream repo describes **Dolt** as the store — embedded in
  `.beads/embeddeddolt/`, synced by `bd dolt push` / `bd dolt pull`
  against `refs/dolt/data` — and says `.beads/issues.jsonl` is "an export
  for viewers and interchange, **not the source of truth** or a backup."
- Other write-ups, and at least one port, describe beads as git-native
  with JSONL in the repo as the collaboration format.

These are most likely different points in the project's history rather
than contradictions. But the difference is decisive here: a Dolt commit
pushed to `refs/dolt/data` is not a pull request anybody reviews, and if
JSONL is an export, then editing it is not how a change is proposed. The
approval mechanism would have to be rebuilt outside beads.

**Two smaller frictions**, both real:

- **Stored status versus derived state.** Beads stores `status` (ready,
  in_progress, closed). This model decided `TaskState` is *derived* and
  never written, which is what makes exactly-one-state structural rather
  than a rule each finish path upholds. Adopting beads' status field
  means either writing state back — fighting the tool and losing the
  invariant — or keeping grain's derivation and treating beads' status as
  a projection, which is workable but means the tracker's own `bd ready`
  is no longer the authority grain uses.
- **Single writer.** Embedded mode is single-writer. The controller, a
  UI, and a human at a CLI are three. Concurrency means running a Dolt
  SQL server — another daemon on the VM that holds every credential,
  which is the same category of cost as the container runtime, arriving
  from a different direction.

### The shape that probably does work

Not "rebuild grain on beads," but **beads as the task graph behind
GitHub as the human surface** — which is what its own external-tracker
sync is for. grain keeps issues as the place humans read, comment,
approve and get notified (which also answers the open [notification
reach](data-model.md#open-questions) question for free), and beads holds
the dependency graph, the sub-task hierarchy, and ready-work computation
that GitHub represents badly.

That is a smaller, reversible bet. It also keeps the approval gate where
the trust model already puts it.

## Beads, or grain's own schema on Dolt?

Beads runs on Dolt. So the fair comparison is not beads-versus-files but
**beads-on-Dolt versus grain-on-Dolt**: same substrate, same operational
risk, differing only in whose schema sits on top. That isolates the
question usefully, because the Dolt risk then cancels out and stops being
the deciding factor.

### What Dolt itself gives, either way

- **Cell-level diff and merge.** Branches, `dolt_diff()`, `dolt_merge()`,
  with schema and data conflicts detected independently and data
  conflicts resolved per cell rather than per line. For a structured
  declaration that is strictly better than a text diff.
- **Version control reachable from SQL** — system tables, functions and
  procedures — so history is queryable rather than only inspectable.
- **JSON column support**, which matters for the parts of the model that
  are genuinely open-ended: capability grants, tags, placements.
- Apache 2.0.

And one constraint that shapes both options equally: **Dolt embeds only
in Go.** grain's controller is Python. So either it shells out to the
`dolt` CLI — which is exactly the idiom `gcloud` already established
here, for reasons `gcp_keys.py` documents — or it runs `dolt sql-server`
and adds a MySQL driver, meaning a new daemon *and* a non-stdlib
dependency on the credential-holding VM. With beads the same shelling
happens through `bd` instead. Roughly a wash.

### What changes if the schema is grain's own

**The model becomes DDL, with no impedance.** `Task`, `Run`, `Lease`,
`Grant`, `TaskLink`, `TrackedPullRequest` are already tables in all but
name.

**Derived state becomes a view, which is as structural as it gets.**
This model decided [`TaskState` is derived, never
stored](data-model.md#taskstate-is-derived-not-stored), and made
exactly-one-state a property of the design rather than a rule each finish
path upholds. As a SQL view over approval and observations, that stops
being a discipline entirely: there is no column to write, so no code path
can write one.

**Approval could be a Dolt merge.** The [repo
direction](data-model.md#direction-the-declaration-moves-into-a-repo)
wanted declaration changes to be reviewable and approval to be merging
one. Dolt gives that natively — declaration changes land on a branch,
review is a data diff, approval is `dolt_merge()` with cell-level
conflicts — provided something renders it. Which
[a first-party UI](data-model.md#direction-a-first-party-ui) already
assumes.

**Several things this document calls newly-answerable become one query.**
"Which live leases came from the credential I am about to rotate?" is a
join. "How many attempts has this task had?" is a count. "What is ready
to dispatch?" is a recursive CTE over `TaskLink`.

### What beads gives that would have to be rebuilt

Honestly: less than it first appears, *for this system specifically*.

- **The graph queries** — transitive blockers, cycle detection,
  ready-work. Real, but a recursive CTE over a few hundred nodes, not a
  research problem. And grain's model already specifies these completely.
- **A JSON-first CLI**, which grain would build as its own surface anyway.
- **External tracker sync** with GitHub, Jira, Linear. The largest genuine
  gift — except grain already has extensive GitHub integration of its
  own, so it is duplicating something rather than supplying something
  missing.
- **Someone else's maintenance**, which is the real argument and should
  not be dismissed.

### What beads would impose

- **A vocabulary that is not grain's.** Core fields are title, priority
  P0–P3, type (bug/task/message), status, assignee. grain's task carries
  intent, origin-as-attribution, folder, one write target and many read
  targets, base, auto-merge, capability grants and their leases. Labels
  and custom tables can hold all of it — but then beads supplies an ID
  scheme and a graph engine while grain's actual model lives in the
  extension points, which is a thin return for a dependency.
- **Stored status against derived state.** Beads stores `status`; this
  model derives it. The workable compromise is to let beads answer the
  *blocked* half and grain derive the rest — but that splits one
  derivation across two stores, which is a fresh instance of exactly the
  disease [this
  model opens by describing](data-model.md#the-problem-the-model-is-real-but-it-is-not-written-down):
  parallel records that can disagree.
- **Upgrade coupling.** Beads enforces schema-version matching at open. A
  beads release could require migrating grain's runtime authority on
  beads' schedule.

### The verdict between them

**If Dolt is chosen at all, choose grain's own schema.** Beads' core
carries an issue tracker's shape, and the overlap with grain is precisely
*the graph* — which is the part this model has already specified in full
and can express in SQL. It would supply what grain has designed and
constrain what grain found hard.

**But the more useful comparison is three-way, not two**, because
"beads or custom" quietly assumes Dolt:

| | Review | Queries | New machinery on the controller |
|---|---|---|---|
| **Files in git** | free — GitHub PRs, the property the repo direction wanted | manual; load and index in memory | none |
| **Grain on Dolt** | a data diff, if the UI renders it | native and cheap | Dolt, plus a daemon or CLI shelling |
| **Beads on Dolt** | same, minus schema control | native, in someone else's vocabulary | Dolt, plus beads' release cadence |

The deciding question is **whether the declaration is something to query
or something to review.** At a few hundred tasks, files in git plus an
in-memory index answers every query this model poses, and review is free
and familiar. Dolt earns its place when the query load or concurrent
writes become real — which is a scale grain does not have yet, and the
[store-size question](data-model.md#open-questions) is deliberately
parked until sub-tasks make it concrete.

So: **files in git now; grain-on-Dolt if scale forces it; beads-on-Dolt
in neither case** — while still taking beads' `ready` framing, which this
model reached independently.

## What a combined rebuild would actually cost

| | Scope | Risk | Reversible |
|---|---|---|---|
| **Incus driver** | one file, six methods | low | yes — a driver among drivers |
| **Incus networking** | rewrite `net_linux.py` around incus networking | medium | yes, but not cheaply |
| **Incus as primary** | ends the macOS plan | strategic, not technical | in principle |
| **Beads as the graph** | a sync layer beside GitHub | medium | yes |
| **Beads as the store** | replaces the declaration record | high — conflicts with approval-as-review and derived state | no |

The two are independent. Nothing about adopting incus depends on beads,
or the reverse.

## Recommendation

1. **Write the incus driver.** It is bounded by an interface designed for
   it, and it can be evaluated beside libvirt on the same host without
   committing to anything. The snapshot behaviour alone is worth
   measuring, because it changes what a sandbox per task costs.
2. **Decide macOS explicitly, not implicitly.** If it still matters, keep
   Lima and treat incus as the Linux driver. If it does not, say so in
   `docs/design.md` and delete a constraint that is shaping the design.
3. **Do not move the declaration into beads yet.** Verify its current
   source of truth first (below). And if Dolt turns out to be the right
   substrate, [put grain's own schema on
   it](#beads-or-grains-own-schema-on-dolt) rather than beads' — beads
   would supply the graph this model has already specified and constrain
   the parts it found hard.
4. **Steal `bd ready` regardless.** The derived-ready-work framing is
   right, and this model has already converged on it — see the
   [invariants](data-model.md#invariants) on derived state.

## What needs verifying before committing

Everything below is unconfirmed. `linuxcontainers.org` is unreachable
from this environment, so incus's primary documentation was not read
directly; the beads findings come from documentation describing more than
one version.

- **Incus:** snapshot restore time for a *VM* (not a container) on btrfs
  and ZFS, at grain's sandbox disk sizes; whether `projects` gives useful
  isolation between the controller and the sandbox pool; how per-instance
  NICs interact with grain's existing bridge/tap and `nftables` source
  pinning; whether nested virtualisation on the GCP host behaves as it
  does under libvirt today.
- **Beads:** which store is authoritative in the release under
  consideration, and whether a task's declaration can be proposed and
  reviewed as a diff; whether custom tables survive `bd dolt pull`
  cleanly; what concurrent access actually requires; whether the GitHub
  sync is rich enough to carry approval both ways.
- **Dolt, if it is considered:** whether recursive CTEs are supported
  well enough for the blocker graph; what `dolt sql` costs per
  invocation when shelled to on a cron loop; what remotes are available
  for a single-host deployment; and the honest cost list, which is
  published as "When NOT to Use Dolt" and was unreachable from here —
  `dolthub.com` is blocked by this environment's egress proxy, so no
  performance or maturity figures in this document come from the
  project's own accounting.
- **Both:** that neither introduces a dependency the controller's
  deliberately short package list should not carry — `python3 git
  openssh-client curl ca-certificates gnupg`, plus `google-cloud-cli` as
  a noted exception.
