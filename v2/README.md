# v2

The rewrite, in Go. `grain/` is v1 and still the thing that runs; nothing
here is wired into it.

```
pkg/model/      the task model of ../docs/data-model.md
pkg/model/dolt/ opening the Dolt database — the only package that imports
                a driver. Open is the embedded, single-writer one;
                Connect dials a Dolt SQL server, which is what makes
                graind, the UI and a CLI writing at once a supported case
                (see "Single writer" below); OpenOrConnect picks between
                them from a deployment's flags
pkg/dispatch/   which task takes which slot: what one cycle decides to
                do with the store, with no side effect beyond that
                decision. It does not loop itself -- cmd/grain's "daemon"
                subcommand's timer does, through pkg/orchestrator -- and
                it carries no
                scheduling policy: it drains task_ready into free slots
pkg/mcp/        a port of grain/automation/mcp_server.py: a newline-
                delimited JSON-RPC server exposing the sandbox tools
                (run_command, read_file, edit_file, write_file) and the
                escape-hatch tools (ask_question, comment_on_issue,
                propose_task, add_review_comment). NewSandboxTools runs
                those four locally, confined to a directory; NewSSHSandboxTools
                (SSHRunner) runs the same four tools over SSH against a
                real remote host instead, the transport a kontur-managed
                sandbox VM needs
pkg/kontur/     resolves a bwsalmon/kontur-managed VM's SSH endpoint: the
                external port kontur itself persisted at "kontur vm
                create" time, plus the pod IP that port answers on, asked
                of containerd directly via crictl since kontur has no
                apiserver to have recorded it anywhere itself
pkg/agent/      the Framework interface an agent driver implements
pkg/agent/gemini/  Framework via the Gemini API, talking to its own
                in-process pkg/mcp/ server
pkg/agent/claude/  Framework via the real `claude` CLI, run as a
                subprocess on the controller (bwsalmon/agents#255) --
                unlike agent/gemini there is no in-process API to drive, so
                this points --mcp-config at this same grain binary's own
                "mcpserver" subcommand (cmd/grain/mcpserver.go), the same
                way v1's dispatch.py pointed it at
                `python3 -m grain.automation.mcp_server`, and parses the
                resulting --output-format stream-json transcript back into
                an agent.Result
pkg/capability/geminikey/  a MINT model.CapabilityProvider: mints, places
                and revokes a Gemini API key, direct against the API Keys
                API
pkg/capability/gcpkey/  the gcp-key capability: a real MINT
                model.CapabilityProvider that mints/revokes a per-task GCP
                service-account key against the IAM API directly
                (google.golang.org/api/iam/v1, no gcloud subprocess), plus
                Reap, a standalone safety net that deletes anything GCP
                itself reports as older than 24h regardless of whether a
                Lease survived to say so
pkg/secrets/    a model.CredentialResolver reading a directory shaped like
                a Kubernetes Secret volume mount (<dir>/<secret>/<key>) --
                the production implementation CapabilityContext.Credentials
                had none of until now
pkg/gitproxy/   a port of grain/proxy: the only path from a sandbox to
                GitHub. Authorizes by asking model.Store what the calling
                sandbox's live task may touch (its Target and Reads)
                instead of a hand-edited allowlist file; credential
                selection and sandbox identity are still the same
                file-based ladders grain/proxy uses. live_test.go proves
                the whole thing end to end against a local git server —
                see "What this actually verifies" below.
pkg/github/     a port of grain/automation/github.go: the GitHub REST
                calls a deployment needs (list/label issues, branches,
                pull requests, review comments, draft reviews) behind a
                Transport seam, the one layer up from gitproxy's own git
                transport.
pkg/github/githubsim/  a port of tests/test_live_issue_to_pr.py's
                RealGitHubMock -- a stateful github.Transport backed by a
                real bare git repo, for a live end-to-end test to wire a
                real github.RESTClient against instead of the real
                network.
pkg/orchestrator/  v1's core.py/Orchestrator equivalent: runs
                dispatch.Cycle's own dispatches (resolving and
                materializing each one's capabilities first, and revoking
                what was minted once it finishes), turns a finished run's
                tool calls into effects (a comment on the task, a pull
                request, a filed follow-up task), and closes out a pull
                request once GitHub reports it merged or closed. It no
                longer polls anything: tasks arrive by being written
                (see "Input is a model update, not a GitHub issue").
                RunCycle runs the two halves as independent reconcilers
                rather than one pipeline -- see "Reconcilers, not a
                pipeline" below.
e2e/            tasks filed the way a user would, carried through
                dispatch.Cycle, a real agent/gemini run, and a real
                gitproxy push, against a real embedded Dolt store and a
                local git
                server standing in for GitHub — fixed scenarios plus a
                randomized multi-user simulation (bwsalmon/agents#233).
                random_test.go (bwsalmon/agents#338) is a second, higher-
                layer randomized cluster test: the real grain CLI binary
                (an operator), a real orchestrator.RunCycle against a real
                githubsim.Sim (GitHub) and a scripted agent each choosing
                among their own valid moves every round, checking after
                each one that no slot stays stuck occupied and that
                nothing pushed or merged ever silently disappears.
                TestRandomizedClusterEndToEnd is the short, fixed-seed
                version `go test ./...` always runs;
                TestRandomizedClusterLong is the same driver run for much
                longer by hand (its own doc comment says how) and does
                nothing unless asked to. See "What this does not have
                yet" below for where it stops.
pkg/ui/         a JSON API, and the static frontend it serves, for
                creating and managing tasks and their capability grants
                by hand (bwsalmon/agents#237). It reads and writes
                model.Store: creating a task here IS filing it, with no
                GitHub issue and no poll in between -- see "Input is a
                model update, not a GitHub issue" below
cmd/grain/      the one binary this repo builds (bwsalmon/agents#313
                combined what used to be four): with no subcommand, or
                one of the task-management verbs, main.go is a CLI over
                pkg/ui.Client -- the same model code the "ui" subcommand's
                Server wraps in JSON and HTTP, driven from a terminal
                instead: list/get/create/update a task, approve, attach
                or detach a capability, comment (which also answers a
                parked question), close ("delete" -- a task that ran is a
                record of a dispatch that happened) or reopen one
                (bwsalmon/agents#271). "daemon" (daemon.go, formerly
                cmd/graind) runs pkg/orchestrator's RunCycle on a timer
                against one real Dolt store (embedded, or a SQL server
                via -store-addr so a UI and a CLI can write it too),
                until SIGINT/SIGTERM, with an in-process gitproxy and a
                real github.RESTClient wired in. "ui" (ui.go, formerly
                cmd/ui) serves pkg/ui.Server behind a local HTTP
                listener, opening the system's default browser -- same
                store flags as the CLI, and no GitHub credentials at
                all. "mcpserver" (mcpserver.go, formerly cmd/mcpserver)
                is the server as a standalone stdio mode -- -sandbox-root
                for NewSandboxTools, or -kontur-vm (plus pkg/kontur,
                above) for NewSSHSandboxTools against a real kontur-
                managed VM -- what a running daemon (via pkg/agent/claude)
                forks *this same binary* to get, rather than needing a
                second one on disk
```

`pkg/` holds every package here that a `cmd/` binary or another package
imports; `cmd/` holds `main` packages only, per the standard Go project
layout. `capability/` is the folder every model.CapabilityProvider lives
under, `gcpkey` included — before this rename it sat at the top level
instead, which is exactly the inconsistency bwsalmon/agents#248 asked to
fix.

```sh
cd v2 && go test ./...
```

## Why Go

Every substrate this design chose is Go, and one of them decides it:
**Dolt embeds only in Go.** A Python controller had to reach it by
shelling out to the `dolt` CLI, and a CLI has no bind parameters — so the
Python version carried a module whose whole job was rendering untrusted
issue titles and comment bodies into statements safely, by hand, against
MySQL escaping rules it could not test. That module does not exist here.
`database/sql` has parameters, and writes are real transactions rather
than a best-effort batch.

The rest follows: Incus ships a Go client, so the host adapter becomes API
calls rather than shelling to `virsh` and parsing output.

## What this actually verifies

The store's tests run against a **real embedded Dolt database** in a temp
directory — not a fake, not a mock. They prove the DDL is valid, the
views answer, the state machine walks every transition, a blocked task
unblocks itself when its dependency closes, and a Dolt commit succeeds.
The equivalent Python tests could only check the SQL grain *generated*,
because there was no `dolt` binary to run it against.

`gitproxy`'s `live_test.go` is the same discipline applied one layer up:
a real bare git repo, served over real smart-HTTP by a real `git
http-backend` process standing in for GitHub, behind a real `GitProxy`
whose `Authorizer` reads a real embedded Dolt-backed `model.Store`, driven
by a scripted (not live-API) `gemini.Framework.Run` calling `run_command`
the same way an agent would. It proves a task's `Target`/`Reads` are
enough on their own to let a sandboxed `git clone`/`commit`/`push` reach
the right repo and nothing else — no allowlist file exists anywhere in
that test.

## Two things the port corrected

**Embedded Dolt needs cgo, and the binary is not static.** It pulls in
`go-icu-regex` and `gozstd`; `CGO_ENABLED=0` does not build, and the
result dynamically links `libicu`, `libstdc++` and `libgcc` at ~145 MB.
An earlier claim in this project's notes — that Go would take the
controller's package list to zero — was wrong. It shrinks (no `python3`,
and the GCP Go SDK would retire the `gcloud` exception) but the C++
runtime takes its place.

Building therefore needs ICU's *headers*, not just the runtime library:
`libicu-dev` on Debian/Ubuntu (what `tests.yml` installs), `libicu-devel`
on Fedora, `brew install icu4c` on macOS — where it is keg-only, so
`CGO_CFLAGS=-I$(brew --prefix icu4c)/include` and the matching
`CGO_LDFLAGS=-L.../lib` are needed too. Without them the build dies in
cgo on `fatal error: unicode/uregex.h: No such file or directory`, some
way down a wall of `go: downloading` lines.

ICU itself is linked **statically** by the Makefile's binary targets.
Dynamically linked, the binary records versioned SONAMEs
(`libicui18n.so.74`) and will not start against a host whose ICU major
differs — Bookworm ships 72, Trixie and Ubuntu 24.04 ship 74 — which
couples the machine that builds to the machine that runs, and fails at
exec time on the target rather than at build time where someone would
see it. Static ICU costs about +31 MB (`grain`: 148 MB → 179 MB, nearly
all of it `libicudata.a` — unchanged since bwsalmon/agents#313 folded
`graind`, the UI and `mcpserver` into this one binary) and leaves
`libstdc++`/`libgcc`/`libc` dynamic, which a Debian host has anyway.
`make test`/`make vet` deliberately stay dynamic so they keep mirroring
`tests.yml`; `ICU_STATIC=0` opts the binary out, and a machine without
the static archives gets a warning and a dynamic link rather than a
failure. Verified by linking the binary, confirming `ldd` reports no
`libicu*.so` on it, and running the ICU regex engine (including
case-insensitive matching, which needs ICU's data) out of the resulting
binary.

**The machine that links the binary is still in it, unless you box it
in.** Static ICU removes the coupling to the *runtime's* ICU; what it
leaves is the toolchain doing the linking, in two places. A host whose
GCC writes `.sframe` unwind sections into objects its `ld` is too old to
read reports every member of the distribution's prebuilt `libicuuc.a`
with a "section ignored" warning -- one per object, ahead of any real
diagnostic. (SFrame arrives in GCC 14 and binutils 2.41, so it is a
mixed toolchain -- new compiler, older linker -- that hits this, not an
old one or a new one.) And `libstdc++`, `libgcc` and `libc` stay
dynamic, so the binary records the symbol versions of whichever glibc
linked it: link against one newer than the controller's and it dies at
exec time there, which is the failure static ICU was adopted to end,
reappearing one layer down.

`make container-build` runs the same `make build`, out of this same
Makefile, inside `Dockerfile.build`'s pinned Debian 12 toolchain -- the
release `packer/kontur/image.pkr.hcl` and `terraform/gcp/variables.tf`
both deploy to. Bookworm's GCC 12.2 and binutils 2.40 predate SFrame, so the
first cannot arise; its glibc 2.36 and GCC 12's `GLIBCXX_3.4.30` bound
the second, and are what the target already ships. The Go version is
read back out of `go.mod` rather than written down twice, and
`GOTOOLCHAIN=local` in the image turns a stale image into an error
naming both versions instead of a silent toolchain download. The tree is
bind-mounted rather than copied in, so both paths build from one copy of
the rules; `.container-cache/` keeps the module and build caches, so only
the first run is cold, and `make clean` removes it with `bin/`. The whole
checkout is mounted, not just `v2/`, because `go build` stamps the binary
with the commit and reads that from the `.git` at the root -- and it is
mounted with git's `safe.directory` set for it, because the uid inside
the container need not own the tree (rootless podman maps the invoking
user to the container's root; Docker with userns-remap remaps it too),
and git refusing a repository it reads as someone else's stops the build
outright with `error obtaining VCS status: exit status 128` rather than
merely leaving the stamp off. Go reports every failure of the git it
shells out to under that one message, so if it appears anyway the cause
is something else git cannot get past -- an unreadable index, a worktree
or submodule whose real gitdir is outside the mount -- and
`make container-build BUILDVCS=false` (or `make build BUILDVCS=false`,
which it forwards) gets the build moving at the cost of the stamp, and
of nothing else. It is
not the default -- `make build` needs no container engine, is what
`tests.yml` runs, and on a host that agrees with itself produces the same
binary.

What comes out is portable across mainstream x86-64 Linux, not across
every Linux, and is not meant to be: it needs glibc -- musl is not
glibc, so Alpine will not run it -- no older than the builder's (2.36 on
bookworm; today's link only reaches for 2.34), a `libstdc++` from GCC 12
or newer, and x86-64. Debian 12, Ubuntu 22.04 and anything newer than
either clear both; RHEL 9, whose `libstdc++` is GCC 11's, does not. A
binary that runs anywhere regardless would have to link `libstdc++` and
libc statically too, which this has not needed: every machine it is
deployed to is the Debian 12 above.

**Embedded Dolt serves one database per directory**, so naming it in the
DSN before it exists fails with "database not found". `Open` therefore
connects twice: once with no database selected purely to create it, then
again for real. Not a `CREATE`-then-`USE` on one connection, which would
be correct only while `MaxOpenConns` is 1 and silently wrong afterwards.

## Input is a model update, not a GitHub issue

**Done — all three stages have landed.**

A task used to begin its life as a GitHub issue. `PollIssues` listed the
task repo's labelled issues and turned each into a `model.Task`; the CLI
and the UI created tasks by creating issues, approved them by swapping
one label for another, attached a capability by adding a label, and
carried the conversation in the issue's comment thread. GitHub was the
input, and the store was a projection of it.

That is being inverted. The CLI and the UI push model updates directly,
the store is the record, and GitHub keeps exactly one artifact: the pull
request a run produces, which tasks are still synced against
(`SyncPullRequests`, the merge queue, check runs — all unchanged). Issues
go away entirely rather than staying on as a mirror, because a mirror is
just the two-writer problem in a new shape: a second place a task's state
lives, with nothing reconciling the two (the same objection the "The UI"
section below already raises against `pkg/ui` and the daemon both writing
the same issue today).

What that costs, stated plainly: the conversation, the audit trail and
the "somewhere a human can watch this" surface all stop being GitHub's
and become grain's, which means grain has to render them. That is what
`task_comment` is for.

**Landed (stage 1) — identity and the conversation, in `pkg/model`:**

- `Store.NewTaskID` allocates from `task_sequence` instead of a task
  being named after the issue it came from. Ids are decimal (`"42"`), so
  the branch is `grain/task-42` where a GitHub-derived id put a whole
  repo path inside the branch name. `AUTO_INCREMENT` rather than a
  counter read and written back, because allocation has to stay correct
  with a controller, a UI and a CLI all writing at once.
- `model.Comment` plus `Store.AddComment`/`Store.Comments` are the
  conversation as grain's own rows. The author is an `Attribution`, not a
  bare `Principal`, because the distinction is load-bearing exactly here:
  grain relaying an agent's question is (automation, on behalf of agent)
  and a human answering is (human, nil) — the difference a signature
  substring in a comment body used to gesture at with one bit.
- `Observation.PendingQuestionCommentID` needs no schema change: it was
  always a `BIGINT`, and it now names a `task_comment.id` instead of a
  GitHub comment id. `TestAPendingQuestionNamesAStoredComment` pins that
  down end to end, and
  `TestATaskCanBeFiledWithNoGitHubIssueAtAll` proves the point of the
  whole stage — an approved task with no `ExternalRef` reads `queued` and
  is dispatchable.

**Landed (stage 2) — the CLI and the UI write the store:**

- `pkg/ui.Client` is store-backed. Creating a task *is* filing it:
  `Store.NewTaskID` allocates, `PutTask` writes, and the task is in
  `task_ready` before the call returns — where before it opened a GitHub
  issue and waited for a poll to notice, which meant no task could be
  created without GitHub reachable, or dispatched until the next tick.
  `pkg/ui` imports nothing from `pkg/github` at all now.
- **The two state vocabularies are one.** This package used to derive
  state from labels with its own set, which had drifted: `needs_approval`
  for what the store calls `proposed`, an `untracked` the store has no
  notion of, and no `closed` that it does. State now comes from the
  `task_state` view, so there is one derivation, not two.
- `/repo`, `/base` and `/auto-merge` stop being directive lines parsed
  out of an issue body and become what they always were in the store:
  columns. `pkg/ui/directives.go` is deleted — a form edits fields.
- Capability grants are `model.Grant` rows rather than GitHub labels, so
  `Capability.Label` is gone from the wire shape.
- **Replying resumes a parked task, in one act.** `AddComment` clears
  `Observation.PendingQuestionCommentID`. Answering a question used to
  take two — post a comment *and* re-apply the trigger label so the next
  poll would notice — and forgetting the second left the task parked
  forever.
- `dolt.Connect`/`dolt.OpenOrConnect` and the `-store-addr`/`-data-dir`
  flags on both the CLI and the "ui" subcommand of `cmd/grain`; see
  "Single writer" below, which stops being a caveat and becomes the
  deployment.
- `Store` grew `ListTasks`, `States` and `ObserveField`. The last is
  `pkg/orchestrator`'s own `observeField` promoted: `Observe` REPLACEs the
  whole observation row, so changing one field means reading it first,
  and that stopped being one package's business once a person closing a
  task from a CLI needed it too.

**Landed (stage 3) — the orchestrator stops reading and writing issues:**

- `PollIssues`, `pollIssue`, `fileTask`, `parkIssue`,
  `requeueIfAwaitingReply` and `orchestrator.TaskID` are deleted, along
  with the `poll` reconciler. `RunCycle` has two reconcilers now, not
  three: there is no outside source of tasks left to reconcile against,
  because a task filed by the CLI or the UI is in `task_ready` the moment
  it is written rather than on whichever tick polls next.
- Everything a run says lands in the store. `ask_question` and
  `comment_on_issue` become `model.Comment`s attributed as grain relaying
  an agent — (automation, on behalf of agent), the distinction v1 could
  only gesture at by looking for a signature substring in a comment body.
  `propose_task` files a real `model.Task` with no `Approval`, so
  `proposeTaskTool`'s "a human must accept it first" contract is enforced
  by the state machine rather than by withholding a label, and
  `model.LinkProposedBy` records which task proposed it — something the
  issue version had no way to say.
- The merge queue's own two voices moved too: `fileFixTask` files a store
  task instead of an issue, and both it and `escalateToUser` comment
  through `Store.AddComment` as the `merge-queue` principal, so a human
  reading a task's conversation can tell the queue's remarks from a
  relayed agent's.
- **Closing out is one write.** It used to be two — close the task's
  GitHub issue, then record the closure — with the issue closed first and
  the store told second, so a crash in between left a closed issue that
  grain still believed was open.
- `Task.ExternalRef`, `model.ExternalRef`, `model.ParseExternalRef` and
  the `external_ref` column are gone (schema version 4).
  `orchestrator.Config` loses `TaskRepo`, `TriggerLabel` and
  `DefaultTarget`: there is no task repo to list, no label to look for,
  and a task arrives with its `Target` already set because whatever wrote
  it set one. The daemon loses the matching flags and gains the
  `-store-addr` family.

GitHub is still reached, for exactly what is genuinely GitHub's: the
branch a run pushed, the pull request opened for it, its checks, and its
merge. `pkg/github` keeps its full client — `ListIssues` and friends are
simply no longer called by anything outside their own tests.

`pkg/orchestrator/live_test.go` is the proof, and it now drives the real
path: a task filed through `pkg/ui.Client` exactly as a person at the CLI
would, dispatched by `RunCycle` in the same cycle, pushed, opened as a
pull request, and closed out once GitHub merges it — asserting at the end
that `sim.Issues` is empty, because the only thing the whole run put on
GitHub is the pull request. Its sibling proves the question path: a run
parks, a human replies with one `AddComment`, and the next cycle resumes
it.

## Every write is a commit

`Store.write` makes a Dolt commit after each successful write, naming
what it was: `grain: approve task 2`, `grain: comment on task 1`,
`grain: update task 1`. Nothing committed before this — however much
grain had done, `dolt_log` showed "Initialize data repository" and
nothing else, and the store kept only a current state.

It now keeps a history, which is what choosing a versioned database was
for. `dolt_log` is what grain did and when; `dolt_diff_task` answers what
*changed*, with the old and new value side by side; and every commit is a
point the deployment can be reset to.

**Commits are attributed to grain, via an explicit `--author`.** Without
it Dolt credits the connected database user, so an embedded deployment's
history reads as having been done by `root` — whoever started the
process. The connection's configured author applies only to creating the
database, which is measured rather than assumed
(`TestCommitsAreAttributedToGrain`). The author says *grain* rather than
which principal asked, because `Store.write` does not know that and the
message already carries the interesting half; `historyAuthor` is where
per-principal attribution would hook in.

**The commit runs after the transaction, not inside it.** The transaction
is what makes a change atomic; the commit is what makes it a named point
in history. Two different boundaries, and conflating them would put a
commit in the path of the write's own success.

**A failed commit is dropped on purpose, and that is safe rather than
sloppy.** By then the write has landed, so failing the call would tell a
caller their change did not happen when it did — and a retry of, say,
`AddComment` would post it twice. The next write stages everything
outstanding (`-A`), so a missed commit costs a coarser history and
nothing else; the gap closes itself, which
`TestAMissedCommitIsRecoveredByTheNextWrite` pins down.

The same `-A` means a commit under concurrent writers can contain more
than the change its message names — another writer's transaction that
landed in between is swept in. Nothing is ever lost and the boundaries
are approximate; with one writer, which is what an embedded deployment
has by construction, they are exact.

Commits are proportional to real activity rather than to time: an idle
`graind` writes nothing, so it commits nothing. `dolt.Commit` is gone —
there is no separate "commit the cycle" step left for a caller to
remember or forget.

## One stamp, and start over on conflict

Every mutation runs in a transaction that also rewrites a single shared
row, `grain_write`. Two operations that overlap therefore disagree about
what that one cell should say, the database refuses the second commit, and
`Store.write` runs the whole operation again on fresh state.

That is the entirety of grain's concurrency control. There is no lock, no
per-row version, and nothing a caller has to carry or remember — the four
mutating call sites go through `Store.UpdateTask` or `Store.ObserveField`
and never mention any of it.

**Why one shared row rather than a version per task.** Per-task versioning
sounds better and is much harder to be sure of. Dolt merges concurrent
writes *cell by cell*, so whether two overlapping operations are safe
depends on which columns and which child tables each touched — and the
answer is genuinely surprising in places. Two writers that each rewrite a
task's tags as delete-all-then-reinsert had their sets silently **unioned**
into one neither asked for, with both commits reporting success. A single
shared cell makes every overlap a conflict, so that question never has to
be answered. All writes serialising is the cost, and for a deployment with
one developer and occasional edits it is not a cost.

**The stamp must be unique per operation, never a counter.** This is the
one sharp edge, and it is sharp: Dolt reports a conflict only when two
writers *disagree* about a cell. Two writers that both read version N and
both write N+1 agree — so the merge succeeds, both commit, and the
child-row union above happens anyway. A counter is the obvious
implementation and it is the broken one.
`TestACounterStampWouldNotConflict` pins that down so nobody optimises the
random token into a counter later.

**Failure needs no cleanup.** An operation that does not reach its commit
leaves nothing behind: no lock to release, no version to reconcile, no
half-written row. A process that dies mid-write leaves an aborted
transaction and the store immediately usable —
`TestAFailedWriteLeavesNothingBehind` asserts both halves of that. This is
the property that ruled out the alternative: `GET_LOCK` works on Dolt, but
a lock returned to `database/sql`'s pool without an explicit release stays
held, so any path that skips the release — a panic past a `defer`, a
cancelled context — wedges the process against itself. Measured, not
feared.

**Retries are bounded and rare.** Five attempts, then `model.ErrConflict`,
which `pkg/ui` maps to a 409 meaning plainly that the change did not land.
Reaching that needs another writer to win five times in a row.

Two things this leans on that were measured rather than assumed, both
pinned by tests so a Dolt change breaks them loudly: that a lost race is
reported at COMMIT as `Error 1213: serialization failure` (matched by
message text, because this package imports no driver on purpose), and that
the conflict aborts the whole transaction rather than just the stamp —
`TestAConflictRollsBackChildRowsToo`.

Verifying any of this against a real `dolt sql-server` still needs an
environment that can install one; everything above is measured against the
embedded engine with its connection pool widened to let two writers race.

## Reconcilers, not a pipeline

`RunCycle` runs three independent reconcilers — `poll`, `dispatch`,
`sync` — and every one of them runs whatever the ones before it did.

It used to be a pipeline: poll, and return on error; dispatch, and return
on error; sync. That reads naturally and is wrong for the same reason
edge-triggered controllers are wrong. Intake talks to GitHub's issues
API, and a cycle that could not reach it also refused to advance a merge
queue, close out a pull request GitHub had already merged, or run a
dispatch whose task and slot were both sitting right there in the store.
None of that work depended on the poll; it was just standing behind it.
A GitHub blip during intake became a cycle in which grain did nothing at
all.

The reason those three can be reordered or skipped freely is that each is
level-triggered and idempotent — it re-reads the store and GitHub every
tick rather than acting on a change it was handed. Nothing is delivered
to a reconciler, so nothing is lost by not running one: skipping costs a
tick of latency and never correctness. Their order in `Reconcilers()` is
a latency preference (an issue filed since the last tick dispatches on
this one; a pull request opened moments ago by this very cycle is picked
up without waiting), not a dependency.

The same argument applies one level down, to the items inside each
reconciler, and that is where it bites hardest in practice. One
unparseable issue used to leave every issue behind it in the batch
labelled and unfiled. One pull request GitHub would not answer for used
to strand every other task's close-out. One slot whose sandbox failed to
build used to abandon the dispatches `dispatch.Cycle` had already
durably recorded runs for, idling those slots for a tick over a failure
that was not theirs. Each of those loops now collects its failures and
keeps going, and `RunCycle` joins the lot (`errors.Join`, so `errors.Is`
still answers for any one of them) into the single line the daemon logs
per tick.

Two places deliberately do **not** isolate, and the reasoning is worth
keeping:

- **Inside one issue, intake still stops at the first error.** The
  trigger label comes off last and only if the store write succeeded, so
  a failure leaves the issue exactly as it was found — labelled, and
  retried next tick. Isolating *within* an issue would mean removing the
  label for work that did not land, which is the
  persistence-before-irreversible-effect ordering `docs/next-session.md`
  records finding a real bug from getting backwards once already.
- **`SyncPullRequests`' gather loop still returns early on a store
  error.** `queueHeads` decides which task is at the front of each repo's
  merge queue by comparing entries against each other, so acting on a set
  with one silently missing could promote the task behind the real head
  and merge two changes in the wrong order. A store read failing there is
  systemic anyway. Its *act* loop is isolated, because head-of-queue was
  already settled against the complete set — an entry failing there
  cannot make another entry merge that would not have merged regardless.

`isolation_test.go` pins all of it down: each test fails one specific
thing and asserts the unrelated work still landed. All five fail against
the pipeline version, with the state assertion naming what it stranded.

This is the first of the three steps toward the Kubernetes-shaped model
the design is converging on. The other two are not built: optimistic
concurrency on `Store`'s mutators (`task` has no version column, and
`PutTask` is last-write-wins — fine for one writer, and "Single writer"
below is already honest that there is more than one), and a real watch,
for which Dolt is an unusually good substrate and currently an unused
one — a commit hash is a `resourceVersion` and `dolt_diff` is a change
feed with history, but `dolt.Commit` has no caller outside its own test
and the daemon never commits. Note the ordering: a watch is a latency
optimization over level-triggered reconciliation, never a replacement
for it, so it is worth having only once the reconcilers it would wake are
independent and safe to run concurrently.

## What this does not have yet

A real host adapter, primarily. There used to be a second gap here too:
two independent packages, `pkg/orchestrate` (bwsalmon/agents#254) and
`pkg/orchestrator` (bwsalmon/agents#249), each decided *when* to call
GitHub's REST API from a running `dispatch.Cycle`, built in parallel
without either knowing about the other. bwsalmon/agents#263 reconciled
them — `pkg/orchestrator` kept its own name and its more complete
GitHub-facing
half (issue intake via `PollIssues` — since deleted, see "Input is a
model update, not a GitHub issue" above — a finished run's tool calls
turned into a comment/PR/follow-up task via `ProcessResult`, and closing
out a merged or closed PR via
`SyncPullRequests`/`Store.OpenPullRequestLinks`),
and gained what only `pkg/orchestrate` had: `RunDispatch` now resolves and
materializes a dispatched task's capabilities, applies every placement
under its sandbox root, and revokes what was minted once the run
finishes, the same as `pkg/orchestrate`'s own `runDispatch` did — ported
onto `orchestrator.Config`'s new `Capabilities`/`Credentials`/
`MaxAgentTurns` fields. `cmd/grain`'s daemon subcommand now drives
`pkg/orchestrator` instead (a small non-overlapping ticker around
`RunCycle`, the same discipline
`pkg/orchestrate`'s own `Reconciler.Run` held to), and `pkg/orchestrate`
itself, along with the `model.TrackedTarget`/`Store.TrackedTargets` it
alone used, is deleted — `Store.OpenPullRequestLinks` (below) already
covered the same "which pull request should grain still be watching"
question, more precisely, so keeping both was pure duplication.

The daemon still defaults to the same "no host adapter" stand-in every
other package here does: one local directory per slot doing sandbox duty
(`orchestrator.HostSandboxes`). `orchestrator.KonturSandboxes`
(bwsalmon/agents#262) is the real alternative `Deps.Sandboxes` also
accepts — one bwsalmon/kontur-managed VM per dispatch slot, reached over
SSH via `mcp.NewSSHSandboxTools` instead of a local directory, created
via `kontur.Create` on first use and reused across cycles the same way
`HostSandboxes` reuses its directories — and the daemon can now be
pointed at it for real: `-kontur-vm-name-prefix` opts a deployment in,
with `-kontur-ssh-user`/`-kontur-ssh-key`/`-kontur-workspace` for the SSH
side and repeatable `-kontur-create-arg` flags building
`KonturConfig.CreateArgs` (bwsalmon/agents#274) — a deployment's own
`kontur vm create -h` decides what those are, most importantly whichever
flag points at a built guest image (`../packer/kontur/`, below), since
that flag's name is owned by bwsalmon/kontur's own CLI and still hasn't
been reachable to confirm from this repo. `KonturSandboxes.
ConfigureGitCredentials` (new alongside the flags) is the SSH equivalent
of the `mcp.ConfigureGitCredentials` call the daemon already made once
per slot for `HostSandboxes` — over `mcp.ConfigureGitCredentialsOverSSH`
instead of `os.WriteFile`, since an SSH-backed slot has no local directory
for the daemon to write into. A kontur VM's own guest image is still
expected to arrive already carrying the operator's SSH key and a running
sshd, the same assumption v1's own sandbox provisioning stood in for —
`../packer/kontur/` is that successor (bwsalmon/agents#267). The
`mcp.NewMockTools` escape hatches (`ask_question`, `comment_on_issue`,
`propose_task`, `add_review_comment`) `agent/gemini.Framework.Run` wires
internally are still discarded rather than posted anywhere real while a
run is live — `ProcessResult` only ever inspects `agent.Result.ToolCalls`
after a run finishes, and relays `ask_question`/`comment_on_issue`/
`propose_task` for real at that point (see the package tree entry
above); giving `Framework.Run` (or its caller) a way to inject a live
sink instead is still open, and `add_review_comment` calls are still
just recorded and nothing more, since nothing yet dispatches with review
intent for one to attach to. Neither sandbox stand-in carries any real
isolation: a real deployment still needs the actual host adapter
(creating a real VM/container per task and running commands in it over
something better than "this process's own filesystem," or an SSH hop to
a VM with no other tenancy boundary of its own), which remains v1
Python — 15,903 lines of it, with 1,239 tests. Those tests are the asset
in a rewrite; the assertions port, the harness does not.
`orchestrator.Deps.Sandboxes`/`.Framework` are exactly the two seams a
real host adapter and a real dispatched-agent connection would replace,
without changing anything about `RunCycle`'s own shape.

`TrackedPullRequest` (`model.PullRequestRef`/`model.PrHealth`/
`model.TrackedPullRequest`, `pkg/model/pullrequest.go`) turned out not to
need a table of its own: `model.Task`'s existing `LinkFixes` link already
records which PR a task's push produced, `task_observation` already
records completion/closure, and `Store.OpenPullRequestLinks` is the one
new read `pkg/orchestrator/sync.go`'s `SyncPullRequests` needed against
those two tables — a `TrackedPullRequest` value is assembled fresh from a
`GetPullRequest`/`ListCheckRuns` read each cycle rather than cached
anywhere, which is what its own `ObservedAt` field is for. Folders are
still unbuilt; nothing here needed them.

`orchestrator`'s own directive parser (`ParseDirectives`) is deliberately
narrower than `grain/automation/directives.py`: `/repo`, `/base` and
`/auto-merge` only. `/pr` (continue an existing PR), `/review` (post a
review instead of pushing) and `/depends` (cross-task ordering) all need a
dispatch shape `RunDispatch`/`BuildPrompt` don't build yet — every task
today is `IntentImplement`, fresh branch, no continuation — and are listed
in `directives.go`'s own doc comment as exactly that, not silently
dropped. `add_review_comment` calls from a run are recorded (`agent.
Result.ToolCalls` carries them, the same seam `ProcessResult` reads
`ask_question`/`comment_on_issue`/`propose_task` off of) but never turned
into a real `CreateReview` call for the same reason: nothing yet dispatches
with review intent for one to attach to. `propose_task`'s `depends_on`
also files today without resolving a same-run local `id` to the real issue
number GitHub assigned it — each proposal lands as its own issue, with
`depends_on` printed into nothing yet, since resolving it needs holding a
whole batch open and rewriting cross-references after every one is filed.

Filing a fix task when a PR goes red is built now (bwsalmon/agents#283):
`SyncPullRequests` runs a merge queue, one per target repo, over every
task that asked for `/auto-merge` and still has a PR open. Only the
queue's head — the earliest submitted, per repo — is ever acted on in a
cycle; a fix filed for the second task while the first is still being
repaired would likely need refiling the moment the first merges and
changes what the second is based against, so everything behind the head
just waits. A conflicted or failing head gets a fix task filed straight
into the store already approved (`Task.Approval` set by
`PrincipalAutomation`, `LinkFixTask` recording which one) rather than
`core.py`'s own `_suggest_fix`, which filed a `needs_approval_label`
issue and waited for a human to apply the trigger label or comment
`/lgtm` — bwsalmon/agents#283 asked for exactly that human step to go
away. The fix task carries `/base` the original PR's own branch and
`/auto-merge true`, the same stacked-branch trick `_suggest_fix` used, so
it dispatches on the very next `dispatch.Cycle` with no approval in
between and, once clean, merges straight back into the branch it
repairs. If it finishes and the original PR is still broken,
`SyncPullRequests` gives up
automatically rather than refiling: it comments explaining why, sets
`Observation.MergeQueueBlockedAt`, and the queue moves on to the next
task in that repo — a blocked task still merges the moment a human's own
push makes it clean, it just stops being anyone's queue head, so it can
no longer hold up what's behind it. No new record was needed for the
queue itself: `queueHeads` derives head-of-queue from `Task.CreatedAt`
and `Task.AutoMerge` fresh every cycle, the same "derive it, don't store
it" discipline `TaskState` already holds to.

The git proxy has moved, though (`gitproxy/`, above) — it is the one
piece of "actually dispatching" v2 now owns outright, credential ladder
and sandbox-token identity included. `grain/proxy`'s
`SandboxCredentialOverrides` (bwsalmon/agents#52's per-task
`grain-github-<name>` label) is a `Task.Grants` entry here instead of a
second sandbox-keyed file: `model.GitCredentialGrant` is the Grant the
label produces, `Store.GitCredentialOverride` resolves a sandbox's live
task down to the name it names, and `GitProxy.Handle` uses that name
against `CredentialSet.Get` in place of the owner/repo ladder whenever
one is present — no override outlives the task, because it is stored
with every other Grant the task carries rather than written and cleared
around dispatch. `dispatch.Cycle` also mints no leases yet — a run's
`Leases` field exists in the schema and `gitproxy` never reads it; the
git proxy authorizes straight off `Task.Target`/`Reads` instead, which
serves the same fail-closed purpose without depending on that field being
populated first.

The GitHub client has moved too (`pkg/github/`, above) — a straight port
of `grain/automation/github.py`'s `GitHubClient`: every method (list/get/
close/reopen an issue, add/remove a label, branch existence and its head
commit, create/find a pull request, `default_branch`, review comments,
check runs, the plain comment thread, `create_comment`, and
`create_review`'s always-draft PR review) behind the same `Transport`
seam the Python version uses, so `github_test.go` proves the same path
building, pagination, and status handling against `FakeTransport` a unit
test would, and `DryRunClient` makes the same "reads pass through,
mutations print" split `gitproxy`'s dry-run tooling and `run.py`'s
`DryRunRunner` both make elsewhere in this project. `pkg/github/githubsim/`
is the "replicate v1's simulator" half: a port of
`tests/test_live_issue_to_pr.py`'s `RealGitHubMock` as a stateful
`github.Transport`, so a live end-to-end test gets the same trick that
file's own docstring describes — every real `GitHubClient` behaviour
(path building, JSON field extraction) still runs, with only the network
call underneath swapped for an in-memory stand-in — and `BranchExists`
answers from a real bare git repo via `git show-ref` rather than its own
bookkeeping, since that check is the one a live test can't afford to
fake. `pkg/orchestrator` decides when to call any of it: it dispatches a
task through `dispatch.Cycle`, opens or reuses a pull request once a run
pushes, and closes one out once GitHub reports it merged or closed. Its
`live_test.go` drives the same two scenarios `e2e/e2e_test.go` already
proved by hand (a push that becomes a merged, closed PR; a question that
parks a task and a reply that resumes it) through
`orchestrator.RunCycle` and a real `github.Client` against `githubsim`
instead — starting, since the inversion, from a task filed through
`pkg/ui.Client` the way a person at the CLI files one. This absorbed a second, independently-built
package that once did a narrower version of the same job — see "What this
does not have yet" below.

The capability provider contract exists now too (`pkg/model/capability.go`),
though nothing here ported it — `docs/data-model.md`'s design was never
built in v1 either. `CapabilityProvider`'s four methods — `Resolve`,
`Materialize`, `PromptSection`, `Revoke` — plus `Placement`, the
vocabulary a provider returns rather than performs so material moves
declaratively and never through a shell or a prompt. A provider here is
handed no Runner at all, unlike the Python contract `docs/data-model.md`
describes — v2 has no host adapter yet for one to run commands against,
so starting from the declarative-only half of that design (the half a
containerised provider is restricted to) costs nothing now and stays
correct once a Runner exists. `ResolveGrants`, `MaterializeGrants` and
`PromptSections` walk a task's `Grant`s against a `CapabilityRegistry`
in registration order, honouring `docs/data-model.md`'s rule that a
half-materialized capability is never described to the agent as
present.

`capability/geminikey/` is now a real `MINT` provider
(bwsalmon/agents#239), porting `grain/automation/gemini_keys.py`'s
`gemini-key` capability: it mints a Gemini API key scoped to the
Generative Language API, places it at `/home/debian/.gemini-api-key`,
and revokes it, calling the API Keys API directly through its Go client
library rather than shelling out to `gcloud` the way the Python version
has to — one of the two things `google.golang.org/api` was expected to
correct, per "Two things the port corrected" above. `DeleteExpired` is
the "clean up after 24 hours if leaked" safety net, mirroring
`delete_expired_keys`.

`gcpkey.Provider` (`pkg/capability/gcpkey/`) is now a real `MINT` provider too —
`gcp-key`, ported from `grain/automation/gcp_keys.py` but talking to the
IAM API directly rather than shelling to `gcloud`, and authenticated
through `CapabilityContext.Credentials` rather than a Runner, so it needed
no controller of its own to build. `model.Reaper` (`pkg/model/capability.go`)
is new alongside it: an optional interface for a provider whose minted
resource can outlive the `Lease` that recorded it, matching
`docs/data-model.md`'s description of `gcp_keys.py`'s own
`delete_expired_keys` as a backstop independent of any task record —
`gcpkey.Provider.Reap` implements it by asking GCP's own key listing for
the answer, not grain's store. `geminikey.DeleteExpired` plays the same
backstop role for Gemini keys but stays a free function rather than a
`model.Reaper` implementation, since an API key carries no service
account of its own for a `ListKeys` call to scope to the way
`gcpkey.Provider.Reap`'s does.

Both providers can now point at the same standing credential —
`geminikey.Capability.Credential` and `gcpkey.Provider.Config.
MinterCredential` are each just a name resolved through
`CapabilityContext.Credentials`, so an operator wires them to the same
one to get bwsalmon/agents#239's "This can share the same account from
the gcp capability" — the daemon is now the executor that does that
wiring (`-gcp-project`/`-gcp-agent-service-account`, bwsalmon/agents#254,
now driving `pkg/orchestrator.RunDispatch` rather than the original
`pkg/orchestrate` package it shipped against, per bwsalmon/agents#263):
it constructs a real `CapabilityContext` per dispatch, applies every
`SideSandbox` `Placement` under the dispatch's sandbox root, and calls
`Revoke` once the run finishes. `Reap` is still uncalled from any binary
here — a standalone sweep independent of any one dispatch, matching
`gcp_keys.py`'s own `delete_expired_keys` cron job rather than something
a reconcile cycle runs itself, and is still open.

`CapabilityContext.Credentials` (`model.CredentialResolver`) had no
production implementation until `pkg/secrets/`: a `Store` reading a
directory shaped like a Kubernetes Secret volume mount, so
`gcp-key`'s minter credential (and any future capability's) resolves
against real material rather than only the fakes `gcpkey_test.go` and
`model/capability_test.go` supply. `CapabilitySpec` grew a `Requires`
field alongside it -- the names, never the values, a capability
resolves through that resolver -- which `gcpkey.Provider.Spec()` and
`geminikey.Capability.Spec()` both set now, and which
`docs/data-model.md`'s new "secret store is a folder, not a table"
section is the checked-in listing of, per capability.

`agent/gemini` can run an agent end to end against `mcp/`'s tools today,
and the daemon now calls it for real from `pkg/orchestrator`'s dispatch
loop rather than only from a test — `orchestrator.HostSandboxes` is the
only other thing `dispatch.Cycle`'s own dispatch path drives, and
neither
hands it more than a local directory to confine itself to yet
(`mcp.ConfigureGitCredentials` sets that directory's git credentials up
the same way v1's `configure_git_credentials` sets a real sandbox's up,
once per slot at daemon startup). The `mcpserver` subcommand itself can
now be pointed at a real remote VM instead — `-kontur-vm` resolves a
bwsalmon/kontur-managed VM's SSH endpoint (`pkg/kontur`: the external port
kontur persisted at `kontur vm create` time, plus the pod IP that port
answers on, asked of containerd via `crictl` since kontur has no
apiserver to have recorded it anywhere itself), `mcp.NewSSHSandboxTools`
runs the same four tools — `run_command`/`read_file`/`edit_file`/
`write_file` — against it instead of a local directory (`grain mcpserver`
can already be pointed at one by hand via `-kontur-vm`,
bwsalmon/agents#256), and `orchestrator.KonturSandboxes`
(bwsalmon/agents#262) is `Deps.Sandboxes`' real alternative to
`HostSandboxes`: one kontur VM per dispatch slot, created via
`kontur.Create` on first use and reused across cycles the same way
`HostSandboxes` reuses its directories, torn down by nothing here (see
that type's own doc comment). A kontur VM's own image is still expected to
arrive already carrying the operator's SSH key and a running sshd, the
same assumption v1's sandbox image build stood in for — `../packer/kontur/`
is now that successor (bwsalmon/agents#267): a Packer template producing
a qcow2 pre-baked with the operator's SSH key, a running sshd, and the
same package list `provision/sandbox.sh` gives v1's own sandbox base —
see that directory's README.md for why the key and sshd are baked in at
build time rather than injected per-VM the way `LibvirtAdapter.create()`
does it for v1, and for what's still unresolved there (the exact `kontur
vm create` flag a deployment's own `KonturConfig.CreateArgs` above would
pass the built image's location through as, owned by bwsalmon/kontur's
own CLI and still not confirmed from this repo — bwsalmon/agents#274).
The daemon still defaults `Deps.Sandboxes` to `HostSandboxes`, but
`-kontur-vm-name-prefix` (and the rest of its `-kontur-*`/
`-cri-runtime-endpoint` flags, see "What this does not have yet" above)
now opts a real deployment into `KonturSandboxes` instead — the flag that
picks the image lives in `-kontur-create-arg`, repeated once per
`kontur vm create` flag/value pair a deployment's own `kontur vm create
-h` calls for, rather than a name this repo guesses at.

A real `github.RESTClient` exists and is wired into the daemon too, driving
every call `pkg/orchestrator` makes (issue listing/labelling, branch and
pull-request state, check runs, comments) — but not the agent's own
`ask_question`/`comment_on_issue`/`propose_task`/`add_review_comment`
calls: `gemini.Framework.Run` still wires those to a `mcp.MockSink` it
builds and discards internally on every call, so they still just record
what they were asked to do rather than posting it anywhere real.
`ProcessResult` only sees them after the fact, through the `agent.Result`
`Run` returns, not while the run is live. Giving `Framework.Run` (or its
caller) a way to inject a real sink is still open.

`e2e/` is that whole chain driven by hand, in a test, rather than by
`dispatch.Cycle` itself: it calls `dispatch.Cycle` to decide what runs,
then
drives `agent/gemini` (scripted in most tests; the real API in
`live_test.go`, gated on `GEMINI_API_KEY`) through a sandbox-stand-in
directory against a real `gitproxy` in front of a local git server, and
plays the part of "the PR opened," "the PR merged" and "a human replied"
with the same `store.Observe` calls a real GitHub-sync component would
make. It proves the pieces already built compose correctly; it does not
close the gap above, since nothing there is wired to run on its own yet.

## The UI

`pkg/ui`/`cmd/grain`'s "ui" subcommand (bwsalmon/agents#237) is
[`docs/data-model.md`'s "first-party UI"
direction](../docs/data-model.md#direction-a-first-party-ui): create a
task, approve a proposed one, attach or remove a capability, comment,
close/reopen — everything a human used to do by hand to a task issue,
from a form instead of a body of directive lines and a label picker.

**It talks to the store, not to GitHub.** That direction's own "the UI is
not a fourth record" rule is now satisfied outright rather than argued
around: there is one record, and this reads and writes it. The two
independent paths that used to touch the same GitHub issue — an operator
working through `pkg/ui`, and the daemon's own reconcile loop — are one
path to one store. `State` still comes from a derivation rather than a
column, but it is `model.StateOf`'s own view now instead of a second
label-shaped copy.

**No OAuth, and now nothing to authenticate to.** The direction document
calls for GitHub OAuth plus `author_association` as the permission gate.
The "ui" subcommand takes no GitHub credential at all any more — it
takes `-as`, naming the principal it acts as, defaulting to the OS user.
This is a
single-operator tool run locally against a store that operator already
reaches; a real permission gate is worth building the day this runs
anywhere other than one person's own machine (bwsalmon/agents#237's
follow-up), and it would gate store writes rather than API calls.

**Why a local web server, not Electron/Tauri/a native app.** `go build`
already produces one dependency-free binary per OS `cmd/grain` runs on
(Mac, Linux today); a `net/http` server that opens the system's default
browser gets "runs standalone on Mac and Linux" for free, in the one language
every other substrate here already commits to (see "Why Go" above), with
no second toolchain (Node, Rust, Xcode) for this repo to carry. "Set up
to run on iOS/Android in the future" is what shapes `pkg/ui` into an
HTTP+JSON API in the first place rather than server-rendered pages: a
future mobile client — native, or a thin webview shell — is just another
caller of the same `/api/*` surface the "ui" subcommand's own frontend
(`pkg/ui/static/`, plain HTML/CSS/JS, no build step) already uses, with
nothing about the server to rewrite.

**`-demo` (bwsalmon/agents#276) for trying out the frontend on its own.**
`grain ui` normally needs a real store — embedded or a Dolt SQL server —
and a real deployment's tasks to look at anything. `-demo` opens a
throwaway embedded store in a fresh temp directory instead and seeds it
with fake tasks, one in each `model.State` (`cmd/grain/demo.go`), through
the same `ui.Client`/`model.Store` writes a human clicking through the UI
would make — no fake `Store` standing in, matching the "real embedded
Dolt, not a fake" discipline every test in this repo already holds to
(`pkg/ui/client_test.go`). That makes it a real server exercising the real
frontend code, with fake data as the only difference from a real
deployment — useful for checking a frontend change renders every state
correctly without an orchestrator, a sandbox, a Gemini key, or a git repo
anywhere behind it. `-store-addr`/`-data-dir` are rejected alongside it,
since a throwaway store and a real one talking to the same flags would be
a UI showing one deployment's fake tasks in some other deployment's data
directory.

**Freshness, not a cache.** Every mutation in the frontend
(`pkg/ui/static/app.js`'s `act`) re-fetches the task afterward rather
than assuming its own optimistic update is now true, matching the
direction document's "it shows freshness for anything" — read live from
the store rather than presenting a stale value as current. There is
nowhere here for staleness to hide since nothing is ever cached across
one request.

**And it refreshes itself.** A task changes state when `graind`
dispatches it, when a run finishes, and when a pull request merges —
none of which the browser is told about, so without a poll the screen
only moves when somebody clicks. `app.js` re-reads every three seconds,
skipping the tick entirely while the tab is hidden and never overlapping
itself.

Two details keep that useful rather than annoying. A poll that finds the
payload unchanged renders *nothing*, so an idle screen never flickers,
loses focus, or resets its scroll; and when the open task does change, an
unsent comment is carried across the re-render, because `renderDetail`
rebuilds the whole pane including the textarea somebody may be halfway
through typing into. Both were checked by driving the real UI in a
browser, which is also how the second one was found.

Polling rather than a change feed is deliberate, and it is the one place
this project declines something the substrate offers. The history is
there now (see "Every write is a commit" above): `dolt_log` hands out
commit hashes that would serve as a `resourceVersion`, and
`dolt_diff_task` reports `added`/`modified`/`removed` with
before-and-after values. What a feed would still need is a diff joined
across six tables to answer "what changed about this task" (a capability
toggle changes `task_grant`, a comment changes `task_comment`), a story
for history that grows without bound, and handling for a cursor that has
aged out. That is a real feature; this is fifteen lines with nothing to
get wrong, and for one operator watching a handful of tasks on the same
machine the two are indistinguishable.

## Deployment configuration lives in the store too

bwsalmon/agents#320 asked the same "the store is the record" question
"Input is a model update, not a GitHub issue" (above) already answered
for tasks, aimed at the daemon's own flags this time: `-slots`,
`-poll-interval`, `-gemini-model`, `-max-agent-turns`, `-github-host`,
`-github-insecure-http`, `-gcp-project` and `-gcp-agent-service-account`
used to be the only way to set any of these, which meant changing one
meant restarting the daemon with a different command line, and there was
nothing a UI could show a human short of re-parsing that command line
somehow.

`model.Config` (`pkg/model/config.go`) and `Store.GetConfig`/`PutConfig`
are the store-backed answer: one row in `grain_config`, the same
one-row-per-deployment shape `grain_write` and `grain_schema` already
use. `cmd/grain`'s "daemon" subcommand's own `loadConfig` (`daemon.go`)
reads it once at startup — before `RunCycle` starts, never again while
running, since bwsalmon/agents#320 explicitly did not ask for graceful
in-flight reloading — and writes those flags into it as a one-time seed
the first time a deployment's store has no row yet, so a fresh
`-data-dir` still starts from a real command line and a UI or a CLI
always has something to read from its very first request. Every start
after that reads the stored row back instead, discarding whatever the
flags on that particular invocation said: the same "a flag that silently
matters differently depending on how many times this has already run
would be a worse surprise than one that is simply ignored after the
first" call every store-backed field elsewhere in this project already
makes. What stays flags-only either has to be reachable before there is
a store to read from at all (`-data-dir`, the `-store-*` family) or
names secret material rather than being configuration itself
(`-gemini-api-key-file`, `-kontur-ssh-key`) — bwsalmon/agents#320's own
"but not the secrets."

`pkg/ui.Settings`/`UpdateSettingsRequest` (`pkg/ui/settings.go`) and
`GET`/`PUT /api/settings` are what actually let something change it:
partial updates, the same nil-means-leave-this-one-alone convention
`UpdateTaskRequest` already uses for a task's own fields, applied as a
read-modify-write against whatever `grain_config` currently holds (or
the zero `model.Config`, the first time). `grain settings` is the CLI
side of the same `Client` methods — no flags prints what is stored (or
that nothing is, yet); any flags apply just those, the way `grain
update` already treats a task's own flags.

`pkg/ui/static/`'s frontend (bwsalmon/agents#333) now has a settings
panel too — the topbar's "Settings" button opens a form reading `GET
/api/settings`, distinguishing `configured: false` (nothing saved yet,
before any daemon has started or any value set) from a populated one
the same way `grain settings` (no flags) already does. Saving sends
only the fields an operator actually changed via `PUT`, leaving the
rest out of the request entirely so they can't clobber what's already
stored — the same partial-update contract `UpdateSettingsRequest`'s
pointer fields already give a CLI caller. A 400's `ValidationError`
message (a bad duration string, an empty required field the first
time) surfaces through the same error banner task creation's own
validation errors already use.

## Single writer

Embedded Dolt permits one writer, which suited a cron-driven controller
and does not suit a controller plus a UI plus a human at a CLI. That
became real the moment the CLI and the UI started writing the store
instead of GitHub, so the answer this section always named is now built:
`dolt.Connect` dials a Dolt SQL server, `dolt.OpenOrConnect` picks
between it and the embedded database from a deployment's flags, and
nothing above `pkg/model/dolt` changed — which is exactly why `Store`
takes a `*sql.DB` and imports no driver.

Both ends stay supported on purpose. Embedded is still right for a
one-process deployment and for every test in this repo, which is why the
store's own tests run against it. `-store-addr` opts a deployment into
the server; `-data-dir` is the embedded fallback, and its flag help says
plainly that nothing else may be running against it.

Two settings on the server DSN are load-bearing rather than defaults
restated, and both would otherwise show up only when run against a real
server: `parseTime`, because the wire protocol hands `DATETIME` back as
bytes otherwise and every `time.Time` on `model.Task`/`model.Observation`
would fail to scan; and `loc=UTC`, because the store writes UTC and a
driver left on `Local` hands it back shifted — a wrong timestamp rather
than an error, and the merge queue orders by `Task.CreatedAt`.

`MaxOpenConns` is deliberately *not* pinned to one on the server path.
That pin exists in the embedded case because a pool there produces lock
contention that reads as a deadlock; a server is the thing that makes
concurrent writers supported rather than hazardous, so pinning it there
would throw away the whole reason to run one.
