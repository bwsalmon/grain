# grain

A single-operator agent cluster, in Go. A daemon holds the task model,
decides what runs now, dispatches each run to an agent framework in a
sandbox, and opens the pull requests that come out of it -- driven from
its own UI and CLI over REST, deployed as one container onto one VM.

This is the Go rewrite that replaced v1's Python (a controller VM plus a
pool of libvirt sandbox guests). v1's code is gone; `docs/design.md` is
kept as the design several packages here still implement and cite.

Paths like `grain/automation/mcp_server.py` and `provision/sandbox.sh` in
the notes below name v1's own files, for provenance -- what a package was
ported from and what it deliberately changed. They are not in this
repository; read them as history, not as somewhere to look.

The map below is the whole of it, package by package.

```
pkg/model/      the task model of docs/data-model.md
pkg/model/sqlite/  opening the embedded SQLite database (modernc.org/sqlite,
                pure Go, no cgo) — the only package that imports a driver.
                Open is the one constructor there is: SQLite has no wire
                protocol to dial, unlike the Dolt this replaced, and there
                is only one writer to serialise now that the daemon is the
                only thing that ever opens the store directly (see "The UI
                and the CLI talk to the daemon over REST" below)
pkg/dispatch/   which tasks run now: what one cycle decides to
                do with the store, with no side effect beyond that
                decision. It does not loop itself -- cmd/grain's "daemon"
                subcommand's timer does, through pkg/orchestrator -- and
                it carries no
                scheduling policy: it drains task_ready until
                max_workers ordinary runs, or max_workers+max_mergers
                runs of any kind, are live
pkg/mcp/        a port of grain/automation/mcp_server.py: a newline-
                delimited JSON-RPC server exposing the sandbox tools
                (run_command, read_file, edit_file, write_file) and the
                escape-hatch tools (ask_question, comment_on_issue,
                propose_task, add_review_comment) -- plus two tools whose
                effect is real and immediate rather than mocked and
                deferred. open_pull_request
                (NewOpenPullRequestTools): a run
                that has pushed its branch can have grain open its pull
                request there and then and read back what the repo's own
                CI makes of it, instead of exiting blind and leaving the
                pull request to orchestrator's finish path. Opening one
                is a write, and writes stay grain's: it asks the daemon
                (pkg/ui's POST /api/tasks/{id}/pull-request) rather than
                holding a credential of its own -- see "A run can open
                its own pull request" below. And recreate_sandbox
                (NewRecreateSandboxTools): a run whose sandbox has become
                unusable can have grain destroy it and build a clean one,
                with the checkout, credentials and placements grain put
                there restored, rather than spending its remaining turns
                failing in a sandbox no tool it holds can repair -- the
                same hop, to pkg/ui's
                POST /api/tasks/{id}/sandbox/recreate, and see "A run can
                rebuild its own sandbox" below. NewSandboxTools runs
                those four locally, confined to a directory; NewSSHSandboxTools
                (DockerExecRunner) runs the same four tools inside a
                kontur-managed sandbox VM's guest instead, by exec'ing
                into that VM's own container -- see "Reaching a sandbox
                guest without a route into it" below.
                NewPullRequestTools adds pull_request_status: the one
                tool here that really reads GitHub, from the controller,
                so a run can see CI's verdict on the commits it pushed
                and repair a red build inside its own turn budget -- see
                "Letting a run watch its own CI" below
pkg/kontur/     drives the `konturctl` binary: create/list/delete for a
                run's VM, the container names kontur derives from a VM
                name, and the one `docker inspect` that tells a VM whose
                container died from one still on its way up. It resolves
                no address for a VM, because nothing needs one
pkg/agent/      the Framework interface an agent driver implements
pkg/agent/antigravity/  Framework via the Antigravity CLI -- Google's
                `agy` binary, the one that replaced Gemini CLI -- run as a
                subprocess on the controller. It replaced this repo's own
                home-grown Gemini runtime, which drove the Gemini API's
                function calling directly and looped tool calls in-process
                against its own pkg/mcp/ registry; agy owns that loop now.
                Two things agy lacks shape this package: there is no
                --mcp-config, so each run gets a private HOME holding just
                the settings file naming its own "mcpserver" server (a
                per-user `agy mcp add` registration cannot express a
                per-run sandbox binding); and there is no --max-turns, so
                RunConfig.MaxTurns is enforced here, by counting completed
                agent_response steps on the live stream and cancelling the
                subprocess
pkg/agent/claude/  Framework via the real `claude` CLI, run as a
                subprocess on the controller (bwsalmon/agents#255) --
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
pkg/secrets/    a model.CredentialResolver backed by its own embedded
                SQLite database (<dir>/secrets.db), kept deliberately
                separate from the task/config store's own database file
                (bwsalmon/agents#366: "put secrets in a separate db,
                config and tasks in a common db") -- the production
                implementation CapabilityContext.Credentials had none of
                until now
pkg/gitproxy/   a port of grain/proxy: the only path from a sandbox to
                GitHub. Authorizes by asking model.Store what the calling
                sandbox's live task may touch (its Target and Reads)
                instead of a hand-edited allowlist file; credential
                selection and sandbox identity are still the same
                file-based ladders grain/proxy uses. live_test.go proves
                the whole thing end to end against a local git server —
                see "What this actually verifies" below.
pkg/github/     a port of grain/automation/github.py: the GitHub REST
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
                pipeline" below. It also times itself
                (orchestrator.CycleTimes): a bounded in-memory ring of how
                long recent ticks took and how far into each one the
                dispatch decision was reached, which is the one thing a
                deployment measures about itself rather than derives from
                a row -- see "Measuring the daemon's own tick" below
pkg/metrics/    what the deployment actually delivers: tasks completed per
                day (throughput) and where a task's wall-clock time goes
                (latency), computed from rows that already exist -- filed,
                approved, dispatched, agent-started, finished, completed --
                with nothing stored, nothing counted on a hot path and no
                way for a number to disagree with the task it describes.
                It holds no state and opens no database: pkg/model reads
                the rows (Store.TaskTimings/RunTimings), this decides what
                they mean, pkg/ui serves it as GET /api/metrics and
                `grain metrics` prints it. Its one non-derived input is
                the daemon's own RunCycle tick, which leaves no row to
                derive anything from -- see "Measuring the daemon's own
                tick" below. See "Measuring throughput and latency" below
tests/e2e/      tasks filed the way a user would, carried through
                dispatch.Cycle, a real agent/antigravity run, and a real
                gitproxy push, against a real embedded SQLite store and a
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
                yet" below for where it stops. loadtest_test.go
                (bwsalmon/agents#416) is a third: many tasks, across many
                repos, many slots dispatching at once, several goroutines
                writing to the same on-disk store concurrently with a
                live RunCycle, to catch scheduling starvation, sqlite
                contention and a capability leak at a scale none of the
                above reach -- `make loadtest`, or that file's own doc
                comment for how to size it up to an actual host.
tests/deploy/   the cross-file agreements no package's own tests can see:
                the Dockerfile, scripts/setup.sh, build-artifacts.yml, the
                Makefile and terraform/gcp/files/deploy.sh all naming the
                same image, the same unit and the same paths. Content
                checks only -- it runs none of it -- so it costs nothing
                and runs on every commit.
tests/container/  the same claims driven for real, against a built image:
                that the store survives the container it was written from,
                that files come out owned by the host account rather than
                root, that a non-root process reaches port 80 through a
                file capability, that a control file written inside reaches
                a systemd unit outside, and that an upgrade pulls a real
                tag from a real registry. Every test skips without
                GRAIN_TEST_IMAGE, so `go test ./...` on a laptop stays a
                unit run; build-artifacts.yml is what hands it an image.
tests/installer/  scripts/setup.sh itself, run as root against the machine
                running the tests -- a real system user, real units, a real
                service -- because the deploy that came up with the image
                pulled and no grain-daemon.service was invisible to
                everything that only drove the image. Destructive, so it
                additionally needs GRAIN_INSTALLER_E2E=1 and skips
                everywhere it is not asked for explicitly.
pkg/ui/         a JSON API, and the static frontend it serves, for
                creating and managing tasks and their capability grants
                by hand (bwsalmon/agents#237). The Go half only: the
                frontend's source is ui/ below, built into pkg/ui/static
                (which server.go go:embeds) rather than checked in
                itself. It reads and writes model.Store: creating a task
                here IS filing it, with no GitHub issue and no poll in
                between -- see "Input is a model update, not a GitHub
                issue" below. Client is that code directly, over a
                *model.Store the caller already has open; HTTPClient
                (bwsalmon/agents#363) is the same method surface spoken
                over HTTP instead, against whichever pkg/ui.Server a
                "grain daemon" is serving -- see "The UI and the CLI talk
                to the daemon over REST" below
cmd/grain/      the one binary this repo builds (bwsalmon/agents#313
                combined what used to be four, #363 folded a fifth --
                the standalone "ui" subcommand -- into "daemon"): with no
                subcommand, or one of the task-management verbs, main.go
                is a CLI over pkg/ui.HTTPClient -- a REST client of
                whichever "grain daemon" -server names, driven from a
                terminal instead of a browser: list/get/create/update a
                task, approve (and withdraw that approval again, which
                puts a queued task back among the proposals rather than
                closing it), attach or detach a capability, comment
                (which also answers a parked question), close ("delete"
                -- a task that ran is a record of a dispatch that
                happened) or reopen one (bwsalmon/agents#271). "daemon"
                (daemon.go, formerly cmd/graind) runs pkg/orchestrator's
                RunCycle on a timer against one real embedded SQLite
                store, until SIGINT/SIGTERM, with an in-process gitproxy,
                a real github.RESTClient, and -- unless -ui-addr is
                emptied out -- an in-process pkg/ui.Server over that same
                store, all wired in. "mcpserver" (mcpserver.go, formerly
                cmd/mcpserver) is the server as a standalone stdio mode
                -- -sandbox-root for NewSandboxTools, or -kontur-vm (plus
                pkg/kontur, above) for NewSSHSandboxTools against a real
                kontur-managed VM -- what a running daemon (via
                pkg/agent/claude) forks *this same binary* to get, rather
                than needing a second one on disk. -server plus -task adds
                open_pull_request to that roster: the daemon to ask and
                the task to ask about, so a run can have its own pull
                request opened while it still has turns left to react to
                what CI says about it (see "A run can open its own pull
                request" below). "demo" (demo.go,
                formerly `grain ui -demo`) is a fifth, smaller mode: a
                throwaway pkg/ui.Server over fake data and a temp-directory
                store, for trying out the frontend with no daemon, no
                store and no deployment behind it at all
ui/             the React+Vite frontend pkg/ui.Server serves
                (bwsalmon/agents#356): the one non-Go tree here, which is
                why it sits beside pkg/ rather than under it. `npm run
                build` writes it into pkg/ui/static, so `make frontend`
                has to run before `go build`/`go vet`/`go test` -- see
                "The UI" below. e2e/ here is its own Playwright suite
                (`make test-e2e`), separate from the repository-root
                e2e/ above
```

`pkg/` holds every package here that a `cmd/` binary or another package
imports; `cmd/` holds `main` packages only, per the standard Go project
layout. `ui/` is outside both because nothing in it is a Go package at
all -- it is an npm workspace with its own toolchain, dependencies and
test runner, and burying that under `pkg/` only made it look like one
more importable package. `capability/` is the folder every
model.CapabilityProvider lives under, `gcpkey` included — before this
rename it sat at the top level instead, which is exactly the
inconsistency bwsalmon/agents#248 asked to fix.

```sh
cd v2 && go test ./...
```

## Why Go

Every substrate this design chose is Go, and one of them decided it:
**v1's Dolt store embedded only in Go.** A Python controller had to reach
it by shelling out to the `dolt` CLI, and a CLI has no bind parameters —
so the Python version carried a module whose whole job was rendering
untrusted issue titles and comment bodies into statements safely, by
hand, against MySQL escaping rules it could not test. That module does
not exist here. `database/sql` has parameters, and writes are real
transactions rather than a best-effort batch — true when this store was
Dolt and unchanged now that bwsalmon/agents#366 has replaced it with
embedded SQLite (`pkg/model/sqlite`).

The rest follows: Incus ships a Go client, so the host adapter becomes API
calls rather than shelling to `virsh` and parsing output.

## What this actually verifies

The store's tests run against a **real embedded SQLite database** in a
temp directory — not a fake, not a mock. They prove the DDL is valid, the
views answer, the state machine walks every transition, and a blocked
task unblocks itself when its dependency closes. The equivalent Python
tests could only check the SQL grain *generated*, because there was no
database engine to run it against without shelling out to a CLI.

`gitproxy`'s `live_test.go` is the same discipline applied one layer up:
a real bare git repo, served over real smart-HTTP by a real `git
http-backend` process standing in for GitHub, behind a real `GitProxy`
whose `Authorizer` reads a real embedded SQLite-backed `model.Store`,
driven by a scripted (not live-CLI) `antigravity.Framework.Run` calling `run_command`
the same way an agent would. It proves a task's `Target`/`Reads` are
enough on their own to let a sandboxed `git clone`/`commit`/`push` reach
the right repo and nothing else — no allowlist file exists anywhere in
that test.

## No more cgo

Embedded Dolt needed cgo, and the binary was not static. It pulled in
`go-icu-regex` and `gozstd`, so `CGO_ENABLED=0` did not build; what came
out instead dynamically linked `libicu`, `libstdc++` and `libgcc`, and
building it at all needed ICU's *headers* (`libicu-dev` on Debian/Ubuntu,
`libicu-devel` on Fedora, a keg-only `brew install icu4c` on macOS with
its own `CGO_CFLAGS`/`CGO_LDFLAGS`). The Makefile went on to link ICU
*statically* into the binary targets on top of that, purely so the
result would not refuse to start against a host whose ICU major version
did not match the one it was built against — dynamically linked, it
would have recorded a versioned SONAME (`libicui18n.so.74`) and died at
exec time on a target shipping a different major, rather than at build
time where someone would see it.

bwsalmon/agents#366 removed all of it by removing Dolt. `modernc.org/sqlite`
is a pure-Go transpilation of SQLite with no cgo anywhere in it, so none
of the above — ICU headers, static linking, SONAME coupling — applies
anymore; there is nothing here to link against, statically or otherwise.
The Makefile's `$(CMDS)` target now sets `CGO_ENABLED=0` explicitly on
its own, but for a narrower and still-real reason: even with no cgo left
in this module's own dependency graph, `os/user` and `net`'s own
cgo-based lookups would otherwise still pull in a dynamic link against
libc, reintroducing the same "binary needs a newer glibc than the
controller has" coupling ICU used to cause one layer up. Forcing it off
produces a genuinely static binary with nothing left to carry to the
controller. `make test`/`make vet` deliberately leave `CGO_ENABLED`
alone, since `go test -race` needs cgo for the race detector and nothing
about testing this module ships anywhere.

`make container-build` still runs that same `make build`, out of this
same Makefile, inside `Dockerfile.build`'s pinned Debian 12 toolchain --
the release `scripts/kontur/build-guest.sh` builds its guest on (bookworm,
via the `debootstrap` in `third_party/kontur`'s own Dockerfile) and
`terraform/gcp/variables.tf` both deploy to -- but the image now exists
purely to pin the Go compiler
version, with no C toolchain or system library left for it to carry. The
Go version is read back out of `go.mod` rather than written down twice,
and `GOTOOLCHAIN=local` in the image turns a stale image into an error
naming both versions instead of a silent toolchain download. The tree is
bind-mounted rather than copied in, so both paths build from one copy of
the rules; `.container-cache/` keeps the module and build caches, so only
the first run is cold, and `make clean` removes it with `bin/`. The whole
checkout is mounted, `.git` included, because `go build` stamps the binary
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
- `-data-dir` and the `-store-addr` flag it briefly had on both the CLI
  and the "ui" subcommand of `cmd/grain` — landed here as a multi-writer
  deployment (a daemon, a UI and a CLI, each opening the store directly),
  and replaced by a single writer again once bwsalmon/agents#363 turned
  the CLI and the UI into REST clients of the daemon; see "The UI and the
  CLI talk to the daemon over REST" below.
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
  it set one. The daemon loses the matching flags.

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

## Grain no longer keeps a commit history

Embedded Dolt made every write a commit, named for what it was --
`grain: approve task 2`, `grain: comment on task 1`, `grain: update task
1` -- attributed to `grain` via an explicit `--author` so an embedded
deployment's history did not just read as `root`, whoever had started the
process. `dolt_log` was what grain had done and when; `dolt_diff_task`
answered what *changed*, old and new value side by side; and every
commit was a point the deployment could be reset to.

bwsalmon/agents#366 gave that up on purpose. SQLite has nothing that
plays the same role, and rebuilding one was never the point of the
migration -- the issue's own "put secrets in a separate db, config and
tasks in a common db" asked for a simpler store, not a versioned one.
`Store.write` makes no commit of any kind now; `historyAuthor`,
`commitHistory` and every test that once pinned this behaviour are gone
along with `pkg/model/dolt` itself. What `Store` keeps is current state
only -- a task's own `created_at`, a comment's `created_at`, a run's
`started_at`/`finished_at` are all still there, but there is no way to
ask "what did this task look like an hour ago" or "list everything grain
has ever done" the way `dolt_log` could. A deployment that needs that
kind of audit trail going forward has to build it as an explicit feature
-- an events table, most likely -- rather than get it for free from the
substrate; nothing here builds one yet.

## Locking, not merging

Every mutation runs in one transaction, and SQLite's own write lock is
the whole of grain's concurrency control now. `sqlite.Open`'s DSN puts
every transaction in immediate mode (`_txlock=immediate`), so the lock
is acquired at `BEGIN` rather than at a transaction's first write
statement: two overlapping mutations are serialised at that exact point
every time, before either has touched a row -- one proceeds, and the
other either waits out a five-second `busy_timeout` or fails outright
with SQLite's own `SQLITE_BUSY`/"database is locked". `Store.write`
retries a failed attempt from the top, re-reading whatever it needs
through the transaction it is handed rather than anything read before
the retry, up to five attempts, then `model.ErrConflict` — which
`pkg/ui` maps to a 409 meaning plainly that the change did not land.
There is no lock a caller has to remember to release, no per-row
version, and nothing the mutating call sites (`Store.UpdateTask`,
`Store.ObserveField` and the writes built on them) have to carry: it is
all inside `write`/`writeOnce` (store.go).

This replaces a mechanism that had to work much harder for a weaker
guarantee. Dolt merged concurrent writers *cell by cell*, and only
reported a conflict when two of them disagreed about the same cell — so
two writers that each rewrote a task's tags as delete-all-then-reinsert
could have their sets silently **unioned** into one neither asked for,
with both commits reporting success. Grain's answer was a single shared
row, `grain_write`, rewritten with a fresh random token on every
transaction purely to force every overlap to look like a disagreement
Dolt would refuse — and the token had to be random, never a counter: two
writers that both read version N and both wrote N+1 would have *agreed*,
so the merge would have succeeded and the same silent union would have
happened anyway (the deleted `dolt/store_test.go`'s
`TestACounterStampWouldNotConflict` pinned exactly that trap). None of
that exists anymore. SQLite admits only one writer at a time, full stop,
so there is nothing left for an artificial per-write marker to catch
that the lock itself does not already catch.

**Failure still needs no cleanup.** An operation that does not reach its
commit leaves nothing behind: SQLite releases the write lock when the
transaction ends, one way or the other, so there is no lock to release by
hand, no version to reconcile, no half-written row — a process that dies
mid-write leaves an aborted transaction and the store immediately usable
by the next one (`TestAFailedWriteLeavesNothingBehind`).

Two things this leans on were measured against the real engine rather
than assumed, each pinned by a test so a wording change in the driver
breaks it loudly: that `modernc.org/sqlite` reports a lost race as
`"database is locked"` or `"SQLITE_BUSY"` (`sqlite/store_test.go`'s
`TestSQLiteReportsABusyDatabase`, matched by message text because
`pkg/model` imports no driver on purpose — `isSerializationFailure`'s
own doc comment), and that a retried transaction rolls back its child
rows along with everything else rather than leaving a partial write
behind (`TestAConflictRollsBackChildRowsToo`).

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
  persistence-before-irreversible-effect ordering v1's own notes
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
`PutTask` is last-write-wins — there is only one process holding the
store open now ("The UI and the CLI talk to the daemon over REST",
below), but that process still serves concurrent requests: the frontend
and a CLI invocation racing each other on the same task is exactly the
same last-write-wins hazard, one layer up), and a real watch. Dolt would
have made one nearly free — a commit hash as a `resourceVersion`,
`dolt_diff` as a ready-made change feed with history — and bwsalmon/agents#366
traded that away along with the rest of the commit history (see "Grain no
longer keeps a commit history" above): SQLite has nothing built in that
plays the same role, so a watch here would mean a change table or
similar, built from scratch rather than read off the substrate for free.
Note the ordering regardless: a watch is a latency optimization over
level-triggered reconciliation, never a replacement for it, so it is
worth having only once the reconcilers it would wake are independent and
safe to run concurrently.

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
accepts — one bwsalmon/kontur-managed VM per dispatch slot, reached via
`mcp.NewSSHSandboxTools` instead of a local directory, created
via `kontur.Create` on first use and reused across cycles the same way
`HostSandboxes` reuses its directories — and the daemon can now be
pointed at it for real: `-kontur-sandboxes` opts a deployment in,
with `-kontur-ssh-user`/`-kontur-exec-key`/`-kontur-workspace` for
reaching the guest and repeatable `-kontur-create-arg` flags building
`KonturConfig.CreateArgs` (bwsalmon/agents#274) — a deployment's own
`konturctl vm create -h` decides what those are, most importantly whichever
flag points at a built guest image (`../scripts/kontur/`, below), since
that flag's name is owned by bwsalmon/kontur's own CLI and still hasn't
been reachable to confirm from this repo. `KonturSandboxes.
ConfigureGitCredentials` (new alongside the flags) is the SSH equivalent
of the `mcp.ConfigureGitCredentials` call the daemon already made once
per slot for `HostSandboxes` — over `mcp.ConfigureGitCredentialsOverSSH`
instead of `os.WriteFile`, since an SSH-backed slot has no local directory
for the daemon to write into. A kontur VM's own guest image is still
expected to arrive already carrying the operator's SSH key and a running
sshd, the same assumption v1's own sandbox provisioning stood in for —
`../scripts/kontur/` is that successor (bwsalmon/agents#267).

`konturSandbox.PlaceFile` is the second such equivalent, and it closes a
gap that made every capability worth having unusable on exactly the
deployments that run for real. A capability's `model.Placement`
(`gcpkey`'s minted service-account key, `geminikey`'s API key,
`githubsandbox`'s token) is delivered by `orchestrator.applyPlacements`,
which until now had one route: `os.MkdirAll`/`os.WriteFile` under the
local directory a `rootedSandbox` reports. A kontur VM has no such
directory — that is the whole point of `rootedSandbox` — so
`sandboxRoot` was empty, and any grant that actually materialized a
sandbox-side placement failed its run during preparation, before the
agent's first turn, with "this sandbox has no local directory to place it
in". Since `scripts/setup.sh` installs `-kontur-sandboxes` for any
host that can run a VM at all, the practical effect was that
`grain-gcp-key` never reached a sandbox on a real deployment: the key was
minted, the run failed, and `revokeAll` deleted it again. `orchestrator.
SandboxPlacer` is the third optional interface a `Sandbox` can answer
with, alongside `rootedSandbox` and `vmNamedSandbox`, and a kontur VM
answers it over the same runner its tool calls and git credentials
already use (`mcp.PlaceFileOverSSH`). `applyPlacements` prefers it
wherever it exists: it writes into the sandbox itself, where a local root
alongside it could only be a staging copy of the same credential on the
controller's own disk. The remote write applies the placement's mode with
`install -m` to an empty file *before* the content goes in, rather than
`chmod`-ing afterwards the way `ConfigureGitCredentialsOverSSH` does —
everything placed this way is credential material, and a `dd` that
creates the file under the login user's umask leaves it world-readable
until the next command runs.

Whichever backend a slot uses, `RunDispatch` now clones the task's target
into it before the agent's first turn (`orchestrator.prepareCheckout`):
`Config.GitRemoteBase` — the daemon's own git proxy URL, the same one the
credential files above are written for — plus the task's repo makes the
clone URL, and the sandbox is left holding a checkout in `./work` with the
branch the task will be pushed to already checked out (its `/base` when it
has one, or the previous attempt's branch when the remote already carries
it, so a redispatch fast-forwards its own work instead of colliding with
it). `BuildPrompt` says so, in place of the nothing it used to say. This
closes a gap live dispatch found the hard way: a sandbox starts empty, the
prompt named the repo and the branch but never said to clone, and the
proxy URL — the only address the sandbox can reach the repo through, since
`ConfigureGitCredentials` writes the host but never a URL — reached the
agent nowhere at all. An attempt's first tool call was a git command in
an empty directory, "not a git repository", and the agent gave up there;
only the redispatch behind it carried the task. It runs through the
sandbox's own `run_command` tool rather than a second path into the
sandbox, so one call covers a local directory and a kontur VM alike, and
an empty `GitRemoteBase` (every test, and any deployment running no proxy)
skips it and leaves the sandbox exactly as bare as it was before. What it
does make load-bearing is an assumption `ConfigureGitCredentials` has
always made quietly: that the sandbox can actually reach the proxy's
address. The daemon binds it to `127.0.0.1:0`, which a local directory
shares and a kontur VM does not — a slot that cannot reach it fails its
dispatch with a clone error naming the repo (`fatal: unable to access
'http://127.0.0.1:<port>/...': ... Couldn't connect to server`,
bwsalmon/agents#567), where before it failed later and less legibly, on
whatever the agent tried against a host its credential file matched but
nothing could route to.

bwsalmon/agents#567 closed that gap: `-kontur-git-proxy-host` (required
alongside `-kontur-ssh-user`/`-kontur-exec-key`/`-kontur-workspace`
whenever `-kontur-sandboxes` is set) names the address a kontur VM's
guest can actually reach this daemon at — typically the docker bridge
gateway its own outbound NAT (`third_party/kontur/internal/netshim`)
routes through, since the guest's `127.0.0.1` is its own, unrelated
loopback. Setting it makes `startGitProxy` bind every interface instead of
just loopback, and advertise that host to every slot's sandbox in
loopback's place. `scripts/setup.sh`'s `ensure_kontur_git_proxy_host`
defaults `GRAIN_KONTUR_GIT_PROXY_HOST` to that gateway address
automatically when an operator hasn't set one, preferring `docker network
inspect bridge`'s own `.IPAM.Config` but falling back to the bridge
device's own address (some docker builds, e.g. Debian's `docker.io`
package, never populate that field for the default bridge's
auto-allocated pool — bwsalmon/agents#572), the same "detect it, or
disable kontur sandboxing for this run rather than install a daemon that
would fail every task" shape `ensure_kontur_kvm_access` already uses for
`/dev/kvm`.

> **Superseded.** Everything in the next four paragraphs — `Recreate`, the
> startup reset pass, `ensure` adopting a VM that already exists — was
> removed when a sandbox stopped outliving the run that used it. It is
> kept here because the *problem* it describes is the one "Slots are gone;
> a sandbox belongs to one run" (below) solves differently, and that
> section reads better against this one. For what the code does now, read
> that.

bwsalmon/agents#353 added two more pieces to `KonturSandboxes`.
`KonturConfig.Backend` selects the value `konturctl vm create -backend`
builds each slot's VM with, and defaults (`-kontur-backend`'s own default)
to `kontur.BackendDocker`: run the VM directly against a local docker
daemon, needing neither `konturctl setup` nor containerd/CNI/kubelet on
the host, instead of kontur's own default static-pod backend under a
standalone kubelet. Since the docker backend has no CRI for `crictl` to
ask, `ToolsFor`/`ConfigureGitCredentials` resolve a docker-backed VM's
reachable address via the new `kontur.DockerPodIP` (`docker inspect` on
the otherwise-idle container `internal/dockervm` starts per VM purely to
hold its network namespace open) instead of `kontur.PodIP`. And
`KonturSandboxes.Recreate` — called by `cycle.go`'s `runOne` once a slot's
dispatch is done, success or failure — deletes and rebuilds that slot's VM
from scratch and reapplies `ConfigureGitCredentials` if it was ever called
for that slot, the isolation boundary v1's own `HostAdapter.recreate()`
gives a sandbox (`grain/adapter/base.py`), applied here per task instead
of on a schedule. `HostSandboxes` implements no such method: the local-
directory stand-in stays long-lived, resetting one between tasks still
being "the caller's job" the same way it always has been.

That per-task recreate covers every task that finishes, which is every
task except the one whose process didn't. A daemon killed mid-run — OOMed,
`SIGKILL`ed, a host that rebooted — never reaches `runOne`'s own recreate,
so that run's VM outlives it with its whole filesystem intact, and
`KonturSandboxes.ensure` deliberately adopts an existing VM rather than
rebuilding it ("the same 'reuse what's there' choice
`HostSandboxes.RootFor` makes"). The next task dispatched onto that slot
inherited the dead run's checkout, credentials and leftover processes.
`RecoverOrphanedRuns` was only ever the store-side half of recovering
from that death: it finishes the rows a killed process left live, and has
no sandbox to reach.

`runDaemon` closed that gap with a reset pass over every slot at
startup, before per-slot git credentials were configured and long before
`RunCycle` could dispatch: one `Recreate` per kontur-backed slot, retried
with the same capped backoff `configureSlotGitCredentials` uses
(`resetSlotSandbox`). A slot that cannot be rebuilt stalls rather than
being dispatched onto dirty — the isolation is the point of the pass, so
downgrading to a reused VM would defeat it. On a clean start there is
nothing to tear down, and `Recreate` skips the delete for a slot with no
saved state: the pass is then exactly the `konturctl vm create` the
credential loop would have made on its own, which is why a fresh
deployment's `konturctl` argv log is unchanged. That skip is load-bearing,
not cosmetic — with no saved state to read a backend off, `konturctl vm
delete` falls back to the static-pod backend and tries to unlink a
manifest path it never needed to touch, which fails outright when that
directory is a root-owned `/etc/kubernetes/manifests` and the daemon runs
as `grain`.

Rebuilding at startup rather than lazily at the next dispatch also kept
the VM boot off the critical path — it happened while the process was
still coming up, not while a ready task waited on it. Host-backed
deployments skipped the pass entirely, `HostSandboxes` having no
`Recreate` to call. That is the one property a sandbox per run gives up
rather than improves on, and "What this costs" at the end of the slots
section below is where that trade is written down.

bwsalmon/agents#466 ("Use kontur sandboxes") found and fixed three bugs
that every other kontur test in this repo, all built against hand-written
`kontur`/`docker`/`crictl` doubles, had no way to catch: `pkg/kontur`'s
own `Create`/`Delete` were exec'ing a binary literally named `kontur` for
`vm create`/`vm delete`, when that binary is `konturctl`'s job --
`kontur` is a different, container-facing program entirely (its own
`cmd/kontur/main.go` doc comment: "distinct from cmd/konturctl, which is
the operator-facing CLI"); `kontur.DockerPodIP`'s `docker inspect`
template dot-accessed `NetworkSettings.IPAddress` directly, which errors
("map has no entry for key") against a real, current docker daemon
(29.7.2) that omits the field from a container with no legacy
single-network attachment, rather than returning it empty; and
`KonturConfig.CreateArgs` had no way to give more than one slot's VM a
distinct `-ip`/`-port` (`konturctl` requires both, with no default and no
auto-allocation of its own), so any deployment with `-max-concurrent`
greater than 1 would have asked every slot's VM to share the exact same
address. The third is now `KonturConfig.BaseIP`/`BasePort`: set either
and `ensure` derives slot's own `-ip`/`-port` from it and the slot's own
number (`model.SlotNames`' own 1-based, all-numeric contract), rather
than repeating a literal `-ip`/`-port` in `-kontur-create-arg` that could
only ever be right for one slot. `pkg/orchestrator`'s
`TestKonturSandboxesAgainstARealDockerBackedVM` is the test that
found all three: it builds `konturctl` and bwsalmon/kontur's own OCI
image from the vendored source and drives `KonturSandboxes.Acquire`
against a real docker daemon and a real cloud-hypervisor VM under real
KVM, skipping outright on a host missing either (as of this writing, that
still stops short of a real dispatched tool call actually executing
inside the guest over SSH -- see the test's own doc comment for why:
scripts/kontur's own guest image, the one built to actually carry
`git`/build tooling and a working SSH login, is not yet published
anywhere a test could fetch it from).

bwsalmon/agents#478 closed that gap by deciding, and validating by hand
under real KVM, the two things #466 left open: `scripts/kontur/` no longer
uses Packer/QEMU at all (a plain `debootstrap`+`chroot` pipeline now
builds the guest directly, needing no VM boot and no cloud image to build
against — see that directory's README.md, "Why no VM boot to build
this"), and the guest's kernel is just Debian's own stock
`linux-image-amd64` direct-kernel-booted by cloud-hypervisor, not a
from-source PVH build — it already has `CONFIG_PVH` and working
virtio-pci/virtio-blk/virtio-net, confirmed by hand once the actual
blocker (`internal/hypervisor/args.go` needing `image_type=raw` on every
disk, for a while a one-line vendored patch, now upstream — see
`third_party/kontur/VENDORED.md`) and
two guest-side gaps (systemd renaming the NIC away from `eth0`, and no
`CONFIG_IP_PNP` to act on `konturctl`'s own `ip=` cmdline — both closed in
`provision.sh`) were found and fixed. `TestKonturSandboxesAgainstARealDockerBackedVM`
now builds that real guest image as part of the test itself and asserts a
`run_command` tool call actually executes inside it over SSH, closing the
gap this paragraph used to describe.

The `mcp.NewMockTools` escape hatches (`ask_question`, `comment_on_issue`,
`propose_task`, `add_review_comment`) a run's own MCP server wires
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

That relay only works because each framework's transcript parser puts a
call's name through `mcp.BareToolName` before recording it: both CLIs
report a tool loaded from their MCP config as
`mcp__grain-sandbox__<tool>`, and `ProcessResult` matches the bare names
`mcp/mock_tools.go` registered, so a parser recording the reported name
verbatim matched none of them on any real run. What let that ship is
worth naming, because the fix alone does not close it: the scripted agy
in `antigravity`'s `testing.go` emitted the bare registry name, a
spelling no real CLI produces, so every test standing on it -- the whole
of `tests/e2e` included -- exercised a shape that existed only in the
harness. The fake now emits the qualified name and calls the registry
with the bare one, which is what a real run does, so `tests/e2e`'s
propose-then-approve test covers the path an agent actually takes.

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
narrower than `grain/automation/directives.py`: `/repo`, `/base`,
`/auto-merge` and `/reads` only (bwsalmon/agents#352 added the last —
repeatable, adding a repo to `Task.Reads` per line rather than replacing
it, per docs/data-model.md's "One write target, many read targets"; a
`Reads` entry is cloned read-only alongside the target and mentioned in
`BuildPrompt`'s own prompt, but grants nothing — `gitproxy`'s authorizer,
not this package, is what actually refuses a push to one). `/pr`
(continue an existing PR), `/review` (post a
review instead of pushing) and `/depends` (cross-task ordering) all need a
dispatch shape `RunDispatch`/`BuildPrompt` don't build yet — every task
today is `IntentImplement`, fresh branch, no continuation — and are listed
in `directives.go`'s own doc comment as exactly that, not silently
dropped. `add_review_comment` calls from a run are recorded (`agent.
Result.ToolCalls` carries them, the same seam `ProcessResult` reads
`ask_question`/`comment_on_issue`/`propose_task` off of) but never turned
into a real `CreateReview` call for the same reason: nothing yet dispatches
with review intent for one to attach to. `propose_task`'s `depends_on` is
resolved now: `relayProposedTasks` files each entry as a real
`model.LinkDependsOn`, against an existing task id (the proposing task's
own included, which is what a piece split out of the work in hand names)
or the local `id` of an *earlier* `propose_task` call in the same run —
earlier, not any, since a batch resolved in call order cannot contain a
cycle, and a `depends-on` cycle is two tasks neither of which is ever
dispatchable again. An entry naming neither is kept in the proposal's own
body for a human instead of filed as a link: `task_blocked` inner-joins
`task` on the target and so ignores a dangling one, while `model.IsBlocked`
counts it as open forever, and a proposal blocked by something that does
not exist has nothing that could ever unblock it.

Resolving it is half of it; an agent has to be told to write one at all,
and told the two facts it needs to. `propose_task`'s description now asks
for `depends_on` in so many words — a proposal that names nothing is
unblocked the moment a human approves it and can be dispatched beside the
work it was meant to follow — and `BuildPrompt` names the running task's
own id, which an agent otherwise could only reverse out of its branch
name. The same schema carries `auto_merge`: a proposal inherits the
proposing task's setting as before (bwsalmon/agents#345), an explicit
`false` opts a proposal out of that inheritance for work an agent judges
deserves its own review, and an explicit `true` is still capped at what
the proposing task itself holds (`proposedAutoMerge`) — a run that could
mark its own proposals auto-merge would be granting itself the unreviewed
merge a human withheld. `BuildPrompt` mentions `auto_merge` only to a task
that is itself an auto-merge job, since there is nothing another task
could do with it.

Filing a fix task when a PR goes red is built now (bwsalmon/agents#283):
`SyncPullRequests` runs a merge queue, one per target repo, over every
task that asked for `/auto-merge` and still has a PR open. Only the
queue's head — whichever of a repo's waiting tasks sits first in the
backlog — is ever acted on in a
cycle; a fix filed for the second task while the first is still being
repaired would likely need refiling the moment the first merges and
changes what the second is based against, so everything behind the head
just waits. Nothing merges while CI is still
running: a check run GitHub has not finished reads `PrHealth.PENDING`
(`healthFrom`), which is neither clean nor failing, so the head simply
holds its place and the next cycle asks again — a queue that merged on
"no failure reported yet" would land changes before their tests had said
anything about them. Pending outranks failing on purpose, so a red job
alongside a still-running one waits too: the queue files exactly one
automatic fix per pull request, and it is worth a cycle to file that one
against CI's whole verdict rather than against whichever job went red
first. Nor does it merge before CI has said *anything*: an empty check
list is read `PENDING` too, until the head commit has carried it for
`defaultCheckRegistrationWindow` (two minutes). GitHub creates a
workflow run's check runs asynchronously after processing a push, so a
sync landing in the gap between the push and that sees nothing — and
nothing is also what a repo with no CI configured answers, forever, with
no way to tell the two apart from the Checks API. The window is the only
thing that can, and a deployment with genuinely no CI pays it once per
head commit. And all of that is reasoning about one *commit*, so the
cycle names it at every step rather than letting any of them mean
"whatever the branch points at now": the check runs are read for the head
sha off the pull-request read (a branch-scoped read answers for a commit
the cycle never saw), the window above is keyed on it, and the merge
carries it in GitHub's own `sha` parameter, which refuses with `409` if
the branch has moved since. That closes the gap a push landing mid-cycle
would otherwise walk through — a human's own "push a fix by hand", a fix
task merging into the branch it repairs, a redispatched task pushing
again — and costs one cycle when it happens: the task keeps its queue
position and the commit that landed is judged next cycle on its own CI.
Nor does it wait for CI that is never coming: a head that
has read `PENDING` for longer than `defaultCheckStallDeadline` (two
hours, timed per head commit over one unbroken run of pending reads) is
given up on — a comment naming the checks that never finished,
`Observation.MergeQueueBlockedAt` set, the queue moved on — since a
workflow waiting on an approval nobody gives, or a provider that posted
"queued" and went away, would otherwise hold its repo's whole queue for
the life of the deployment with nothing said to anyone. No fix task is
filed for that one: nothing has failed, and a check that never finishes
is usually waiting on something outside the pull request, so there may
be nothing in it to repair. A conflicted or failing head is not taken at
its word, either. Before anything is filed for it, `refreshStaleHead`
asks GitHub to merge the pull request's own base branch into its head
branch (`POST /repos/{owner}/{repo}/merges`, `Client.MergeBranch`) —
because the common shape of a broken head is not a broken change but a
stale one: its checks last ran against a `main` that has since moved, so
either GitHub can no longer compute a merge ref and every
`on: pull_request` job dies at checkout (which reads as *failing*, not as
a conflict), or the verdict that did report is about a tree nobody would
ever merge. The queue does not try to tell those apart before acting —
`mergeable_state` reports `blocked` rather than `behind` for exactly this
case, and a check run names no base — it asks for the merge and lets the
answer classify: `201` means it was behind and is not now, so CI re-runs,
the head holds its queue position while it does, and the *next* cycle
decides on a fresh verdict; `204` means the branch already contained its
base, so the failure is genuine and the fix task is filed as it always
was; `409` means it genuinely conflicts, which is the case a fix task is
really for, filed immediately and now naming the conflict the queue
watched GitHub refuse rather than one inferred from a `Mergeable` flag.
That is one API call in place of a full agent run for what has been the
majority of this deployment's automatic fixes — every one of them
resolved by a plain `git merge origin/main` an agent booted a sandbox to
type. It happens once per pull request
(`Observation.MergeQueueRefreshedAt`, persisted for the same reason the
CI clocks are not: losing it would cost a repeated write to GitHub rather
than another window of waiting), only for the queue head, and never for a
fix task's own stacked branch or one the queue has given up on. It is a
merge, never a rebase: nothing force-pushes a branch an agent may still
hold a clone of, or moves the base out from under an in-flight stacked
fix. A conflicted or failing head that survives all that gets a fix task filed straight
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
queue itself: `queueOrder` derives the whole queue from `Task.AutoMerge`,
`Origin.Reason` and `MergeQueueBlockedAt` fresh every cycle, and
`queueHeads` takes each repo's first entry from it — the same "derive it,
don't store it" discipline `TaskState` already holds to.

What the queue does write down is where it is. Every cycle,
`showQueueAtFrontOfBacklog` moves the tasks waiting to land to the front
of the backlog in the order they will land, and `fileFixTask` files a
repair at the very head of it (`Store.MoveToFrontOfBacklog`,
`Store.OrderKeyForNewTask`) — so a task list answers "what is grain about
to finish, and in what order" without anyone opening a task. It is the
same order in both directions: `queueOrder` reads position back off the
backlog rather than comparing `Task.CreatedAt` behind everyone's back, so
dragging one waiting pull request above another really does change which
merges first, and `Store.Ready` needs no carve-out for a fix task any
more — being at the head of the list is what dispatches it first, which
is a thing a human can see and, if they disagree, undo.

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
`live_test.go` drives the same two scenarios
`tests/e2e/e2e_test.go` already proved by hand (a push that becomes a
merged, closed PR; a question that parks a task and a reply that
resumes it) through
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

`agent/antigravity` can run an agent end to end against `mcp/`'s tools today,
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
kontur persisted at `konturctl vm create` time, plus the pod IP that port
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
same assumption v1's sandbox image build stood in for — `../scripts/kontur/`
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
`-kontur-sandboxes` (and the rest of its `-kontur-*`/
`-cri-runtime-endpoint` flags, see "What this does not have yet" above)
now opts a real deployment into `KonturSandboxes` instead — the flag that
picks the image lives in `-kontur-create-arg`, repeated once per
`konturctl vm create` flag/value pair a deployment's own `konturctl vm create
-h` calls for, rather than a name this repo guesses at.

A real `github.RESTClient` exists and is wired into the daemon too, driving
every call `pkg/orchestrator` makes (issue listing/labelling, branch and
pull-request state, check runs, comments) — but not the agent's own
`ask_question`/`comment_on_issue`/`propose_task`/`add_review_comment`
calls: a run's own MCP server still wires those to a `mcp.MockSink` it
builds and discards internally on every call, so they still just record
what they were asked to do rather than posting it anywhere real.
`ProcessResult` only sees them after the fact, through the `agent.Result`
`Run` returns, not while the run is live. Giving `Framework.Run` (or its
caller) a way to inject a real sink is still open.

`tests/e2e/` is that whole chain driven by hand, in a test, rather than
by `dispatch.Cycle` itself: it calls `dispatch.Cycle` to decide what runs,
then
drives `agent/antigravity` (scripted in most tests; the real `agy`
binary in `live_test.go`, gated on `GEMINI_API_KEY` and an installed
`agy`) through a sandbox-stand-in
directory against a real `gitproxy` in front of a local git server, and
plays the part of "the PR opened," "the PR merged" and "a human replied"
with the same `store.Observe` calls a real GitHub-sync component would
make. It proves the pieces already built compose correctly; it does not
close the gap above, since nothing there is wired to run on its own yet.

`self-debug` and `self-repair` (bwsalmon/agents#540, "configuration
mode") went from `ui.OfferedCapabilities` names with nothing behind them
to real `model.CapabilityProvider`s -- `pkg/capability/selfdebug` and
`pkg/capability/selfrepair` -- but what each one grants is not material
in a sandbox or text in a prompt, `model.CapabilityProvider`'s only two
channels; it is tools. `orchestrator.Config.GrantTools` is the new,
narrow seam that adds: a capability name to a function building extra
`mcp.Tool`s, consulted by `runOne` only for an `Interactive` task, and
kept as a caller-supplied map rather than a fifth `CapabilityProvider`
method on purpose, so `model/capability.go`'s own "a provider is handed
no Runner" stays true of the package itself even though a deployment can
now wire one in from outside it. `selfdebug.SourceTools` is read-only --
`read_grain_source`/`list_grain_source`, confined to whatever directory
a deployment's `-upgrade-src-dir` already names, needing no confirmation
of any kind, since nothing it exposes can change anything.
`selfrepair.HostCommandTools`' `run_host_command` is the opposite: it
runs a shell command directly against the same host `grain daemon`
itself runs on -- no sandbox, no adapter, the real machine -- so every
call posts the command into the task's own chat and blocks
(`selfrepair.Confirm`, polling `Store.Comments`) until a human replies
there with approve or deny, or a timeout refuses it for them. That block
is a real synchronous wait inside one tool call, unlike every other
human-in-the-loop primitive here (`ask_question` parks the whole run and
picks a reply up on the next dispatch) -- it only works if the tools run
in the same OS process as the store the reply lands on.

**Nothing satisfies that any more.** It held while the default framework
was the home-grown in-process Gemini runtime, which registered a run's
tools in-process and so already held that `*model.Store` connection.
Both frameworks that remain (`agent/antigravity`, `agent/claude`) fork a
CLI that manages its own MCP connection and ignore `RunConfig.Tools`
entirely, because there is no in-process registry to hand a forked
process. `Config.GrantTools` still assembles these tools and
`RunDispatch` still passes them, but no `Framework` consumes them, so
`selfrepair`/`selfdebug`'s host tools reach no running agent today.
Closing that gap means giving the `mcpserver` subcommand a route back to
the store, which is a design question rather than a missing flag: the
isolation that makes the subprocess frameworks safe is exactly that it
holds no store handle. What it does hold is deliberately narrow and
deliberately not that: a read-only GitHub client scoped to one branch
(`-data-dir`/`-pr-repo`/`-pr-branch`, for `pull_request_status` -- see
"Letting a run watch its own CI", below), and a REST client of the
daemon aimed at one endpoint about one task id
(`-server`/`-task`, for `open_pull_request` -- see "A run can open its
own pull request"). Neither is a store handle, and neither can answer
`selfrepair.Confirm`'s blocking read of `Store.Comments`.

bwsalmon/agents#621 turned that pair of capabilities into an explicit
"configuration agent": an overlay button the frontend keeps reachable in
the bottom-right corner of the screen no matter what view is on screen
(`ConfigurationAgentButton.jsx`), which files a task with nothing but
`{"configuration": true}` and opens its chat the moment it exists. What
that one field expands into -- `Interactive` forced true, `self-debug`
and `self-repair` both granted, a default title and a prompt oriented at
helping with a problem, a question, or grain's own configuration -- is
assembled once, server-side, in `ui.Client.CreateTask`, so nobody
filing one (this button today, conceivably a CLI flag later) has to
reassemble the bundle by hand. `Task.Configuration` also changes how
`dispatch.Cycle` schedules the task: `dispatchConfiguration` starts every
such task unconditionally, ahead of the capacity-gated loop that governs
everything else, so the configuration agent can always start a sandbox
even when the deployment is already at its worker limit -- the moment
someone reaches for it is often exactly the moment the deployment is
already saturated.

## The agent runtime is a CLI now, not our own turn loop

grain used to drive the model itself. `pkg/agent/gemini` held a
hand-written turn loop: call the Gemini API's function calling, translate
each `mcp.ToolInfo` into a `genai.FunctionDeclaration`, execute whatever
`FunctionCall` came back against an in-process `pkg/mcp` registry, append
the results, go round again. It worked, and it was ours to maintain --
the schema translation, the turn accounting, the thought/text split, the
partial-result-on-failure rule, all of it code in this repo tracking an
API that moves.

`pkg/agent/antigravity` replaces it with Google's Antigravity CLI, the
`agy` binary that replaced Gemini CLI. The loop is agy's now. What is
left here is the shape `pkg/agent/claude` already settled on for driving
a real CLI: build the arguments, hand it a prompt, parse the transcript
it streams back. Both frameworks a deployment can pick between are now
subprocess drivers, and `agent.Framework` is the seam that makes them
interchangeable.

Three things about agy shaped the port, none of them cosmetic.

**It has no `--mcp-config`.** agy registers MCP servers per *user* --
`agy mcp add` writes them into `~/.gemini`, and caches each server's tool
manifests under `~/.gemini/antigravity-cli/mcp/<server>/`. A per-user
registration cannot express what grain needs, which is a per-*run*
binding: two runs dispatched concurrently against two different sandboxes
would share one registration, and whichever wrote it last would decide
where both runs' tools landed. So `Framework.Run` gives each run its own
private `HOME` -- a temp directory holding nothing but the settings file
naming that run's own `mcpserver` server -- and deletes it as the run
returns. That has the same effect `claude`'s `--strict-mcp-config` has
there: the only MCP server a run can see is its own, because there is no
other settings file in the `HOME` it was given to find one in.

**It has no `--max-turns`.** `RunConfig.MaxTurns` is therefore enforced
here rather than by the binary, and enforced on the live stream rather
than on the finished capture -- a cap applied after the process exits
would report a runaway run without ever having stopped one. A small
`io.Writer` spliced into agy's stdout counts completed `agent_response`
steps as they stream past and cancels the run's context at the cap;
`procgroup.Prepare` is what turns that into a kill of agy *and* its MCP
child rather than an orphan.

**It has no way to empty its native tool roster.** `claude` takes
`--tools ''`, which is how `agent/claude` guarantees a run reaches the
sandbox only through grain's own MCP tools. agy has no equivalent, so
that guarantee is weaker here: what this package does instead is give the
subprocess a `HOME` with exactly one MCP server in it and a working
directory that is the sandbox, and report -- as a transcript line, on the
run itself -- any tool agy's own `init` event advertises beyond the ones
grain published. A deployment that needs a hard guarantee should run
against a kontur sandbox, where the controller's filesystem is not
reachable from the guest at all.

Two smaller notes. The prompt travels over stdin as a `stream-json` user
event, not as the argument to `--print`: untrusted issue content must
never become a `ps`-visible argument, the same discipline v1's
`dispatch.py` set. And `RunConfig.Addenda` -- folding a comment posted
mid-run into the next turn -- is gone in practice, because it needed a
turn boundary to poll at and neither remaining framework has one; a
comment posted while a run is in flight waits for the next dispatch, as
it already did under `agent/claude`.

### What this cost

`RunConfig.Tools` has no consumer any more. It was read only by the
in-process runtime, and a forked CLI cannot be handed an in-process
registry. `orchestrator.Config.GrantTools` still assembles
`selfrepair.HostCommandTools` and `selfdebug.SourceTools`, and
`RunDispatch` still passes them, but nothing consumes them -- so an
Interactive task's `run_host_command` confirmation prompt
(`selfrepair.Confirm`, which blocks on `Store.Comments` from inside a
tool call) is not reachable by a running agent today. Closing that gap
means giving the `mcpserver` subcommand a route back to the store, which
it deliberately does not have: besides its sandbox, all it takes is one
branch to read CI for (`-pr-repo`/`-pr-branch`) and a daemon URL and a
task id (`-server`/`-task`, for `open_pull_request` -- "A run can open
its own pull request", below), which is a REST client of one endpoint
rather than a store handle, and that narrowness is exactly what makes
the subprocess frameworks safe to run. It is a design question, not a
missing flag, so it is recorded here rather than guessed at.

### Operating it

`-agy-path` names the binary, defaulting to resolving `agy` on `$PATH`,
and is flags-only for the same reason `-claude-path` is: where a binary
lives is a property of the machine, not of the deployment's stored
configuration. The `agent-framework` setting's vocabulary is now
`"antigravity"` or `"claude"`; `"gemini"`, the name the default framework
had while it was our own turn loop, is still accepted everywhere it can
arrive -- a stored `grain_config` row, a config file, a `-agent-framework`
flag -- and normalized to `"antigravity"` by
`model.NormalizeAgentFramework`. Nothing rewrites those rows: folding the
old spelling in on read is cheaper than a data migration and is the same
answer for a unit file that still passes the old flag. A deployment
upgrading across this change needs `agy` installed on the controller and
otherwise keeps its existing `-gemini-api-key-file`, which `agy`
authenticates with as `GEMINI_API_KEY` in the subprocess environment
(never in argv).

## Letting a run watch its own CI

A run could always push more than once — the git proxy authorizes every
push to the task's own target (`gitproxy/authorize.go`), and
`ConfigureGitCredentials` leaves a working identity and credential helper
behind — but it had no way to find out what CI made of a push. The
checks were read minutes later by a different process
(`SyncPullRequests`), and a red build became a whole separate fix task
(`fileFixTask`), dispatched into a cold sandbox, to repair something the
run that broke it was still sitting there able to repair.

`pkg/mcp`'s `pull_request_status` closes that loop. It reports the branch
tip, the pull request open for it if there is one, and every check run
against the pushed commit with the failing ones named — enough for a run
to push, look, fix and push again inside one dispatch.

It does not reopen docs/design.md's split surface ("Sandboxes: git
transport only. No REST, no GraphQL"). The tool is served by the
`grain mcpserver` process, which runs on the *controller*, and reads
GitHub with the controller's own `secrets/github` ladder — exactly the
shape the `ask_question` escape hatch already had, and acceptable for the
same reason: what crosses into the sandbox is a rendered answer, never a
credential and never a general-purpose API call. The scope is fixed at
process start from flags `cmd/grain/mcpserver.go` receives
(`-pr-repo`/`-pr-branch`, written by each framework's `mcpServerArgs`
from `agent.RunConfig`), and no tool argument can move it: a run reads CI
for its own branch or nothing. The tool is registered whatever those
flags said, so a task with no repo attached gets its own explanation
rather than an "unknown tool" that reads like a broken grain.

Three things had to be said out loud rather than left implicit.
`BuildPrompt` names the push/check/repair loop, because nothing about a
tool description tells a run that it may push a second time and the
sentences around it read like one final act. An unfinished check is
reported as carrying no verdict, never as passing — the same call
`healthFrom` makes at the merge gate, made again here so a run that
pushes and sees three queued jobs does not declare itself done. And
`BuildPrompt` says where the loop ends: the job is done when the checks
have finished and passed *and* the branch still merges cleanly into its
base — `task.Base` where the task names one, described rather than
guessed where it does not. A conflict is read off the pull request before
any check is (`healthFrom`'s `PrConflicted`), so a green branch that
conflicts still never merges, and the run holding the checkout can fetch
and merge in a turn — which is cheaper than the fix task the merge queue
would otherwise file for it minutes later in a cold sandbox.

## Reaching a sandbox guest without a route into it

A slot's VM guest is reached by exec'ing into that VM's own container:
`docker exec <vm container> kontur exec` (`mcp.DockerExecRunner`).
bwsalmon/kontur ships the guest-side half -- `kontur exec` SSHes to the
guest's own tap-attached address, which the docker backend records as
`KONTUR_EXEC_ADDR` when it starts the VM container. That address needs no
address translation to reach, because the container shares the network
namespace netshim set the tap up in: `internal/guestexec`'s own words,
"reachable directly from this container's own network namespace ...
without going through NETSHIM_GUEST_PORT's external DNAT at all."

This replaced an SSH connection from the daemon to the external port
netshim forwards on the VM's container address, and the reason to prefer
it is how much only existed to describe that route in from outside:

- `pkg/kontur` no longer reads kontur's state files or resolves an
  address at all. `Port` (the external port, out of kontur's own JSON),
  `PodIP` (crictl) and `DockerPodIP` (`docker inspect`, with its
  `index`/`Networks` template) are all gone; `Exists` is what remains of
  the state file, and only to answer "has this VM been created".
- `KonturSandboxes` lost `resolveEndpoint` and `waitForSSHPort`, and with
  them the `Backend`, `RuntimeEndpoint` and `SSHKey` config.
- `mcp.SSHRunner` is gone; only its shell-quoting helpers survive
  (`shellquote.go`), which the tools still need to build one command
  string for the guest to parse.
- Only the docker backend is supported, since it is the only one whose
  VMs run as containers to exec into. `createArgs` passes
  `-backend docker` itself rather than taking it as config.

`TestKonturSandboxesDockerExecReachesTheGuestWithoutResolvingAnAddress`
holds the claim: its fake docker cannot answer an address lookup and
nothing is listening on any port, so getting tools back at all is the
proof none of that is consulted.

Two details are worth knowing:

- **`-kontur-exec-key` is a path inside the VM's container**, not on the
  host. It named a deployment keypair `setup.sh` generated, staged into
  a directory the VM's container already mounted, and
  `guest-setup.sh` baked into the guest's `authorized_keys`.

  No deployment does that any more: kontur generates a keypair in each
  VM's own container at boot and hands the guest the public half on its
  kernel command line, so there is no deployment keypair to generate,
  nothing to stage, and `setup.sh` passes no `-kontur-exec-key` at all.
  The flag stays for the case it is now the only answer to -- a guest
  image that authorizes a key of its own rather than the one kontur
  generates -- which is why it is optional rather than required.
- **`docker exec` cannot distinguish a failure to reach the guest from a
  guest command that exited 1**, the way `ssh` can with its own reserved
  status: it reports the exit status of whatever it started, and
  `kontur exec` exits with the guest command's own. `DockerExecRunner`
  tells the two apart by the first line of stderr (see
  `execFailedBeforeGuest`), erring toward "it never ran" -- the same
  `exitCode == -1` the SSH transport reported for an unreachable sandbox.

Readiness changed shape with the transport. `resolveEndpoint` could only
wait for a TCP port to start answering, which happens before the guest
has booted to a usable sshd; `waitForGuestExec` probes by running a whole
command *in the guest*, so a caller holding a runner already knows one
ran. Both fast-fail on a VM container that has already exited.

What a fake cannot settle is whether `kontur exec` authenticates against
*this* guest image at all -- that rests on kontur's own `KONTUR_EXEC_KEY`
handling and on `scripts/kontur/guest-setup.sh`'s `authorized_keys`, neither
of which this repo's fakes own -- nor whether `KONTUR_EXEC_ADDR` is
really set to somewhere the guest answers, nor whether exit statuses and
stdin survive both hops. `TestKonturSandboxesAgainstARealDockerBackedVM`
(`kontur_docker_real_test.go`) covers all four against a real
konturctl/docker/cloud-hypervisor VM under real KVM, and skips wherever
docker, `/dev/kvm` or the guest-image build prerequisites are missing --
so it never runs on a hosted runner, and does run wherever kontur's
prerequisites genuinely exist.

## And the route out of one

The other direction had been broken for as long as flat mode has been the
default, and nothing said so. `docs/design.md`'s "Sandbox egress is open
by default" was simply not true of any VM this repo booted: a sandbox
guest could reach its own subnet -- the git proxy on the docker bridge
gateway, which is why dispatch worked at all -- and nothing beyond it. No
`proxy.golang.org`, no `registry.npmjs.org`, no GitHub, no apt mirror.
Enough of the guest image is built around that (`scripts/kontur`'s warm
module and npm caches, the `npm` wrapper that skips Playwright's
browsers) that the missing network read as a design constraint rather
than as a fault.

It was a fault, in one line of `third_party/kontur`. A flat-mode guest
takes over the identity the container runtime assigned its namespace, and
`netshim.DiscoverIdentity` reads that identity back off the external
interface: address, MAC, MTU -- and the namespace's default route, which
reaches the guest as the gateway field of the `ip=` kernel parameter
`FlatGuestConfig` derives. It looked for that route by testing
`r.Dst == nil`, which a route read back off the kernel never satisfies:
the kernel omits `RTA_DST` when the prefix length is zero, and
`vishvananda/netlink` fills the absence back in the way iproute2 does,
synthesizing `0.0.0.0/0`. So the gateway was always nil, the parameter
always read `ip=<addr>:::<mask>::eth0:off`, and klibc's `ipconfig`
configured an address with no default route behind it.

Every other part of the takeover fails loudly -- a guest with the wrong
address or MAC never becomes reachable, and its dispatch fails on that --
which is exactly why this one survived: it fails silently. The VM boots,
`kontur exec` reaches it over the control link, every tool call succeeds,
and only the network is gone. Confirmed on a live sandbox guest, whose
`/proc/cmdline` carried the empty gateway field and whose routing table
stopped at its own `/16`; re-running the guest's own `ipconfig` with the
gateway filled in installed the default route, after which DNS and HTTPS
both worked from inside the guest.

The fix is `netshim`'s, and it is a local patch to the vendored copy
rather than the resync this repo prefers -- see
`third_party/kontur/VENDORED.md`'s "Local patches" for that trade and for
how it goes away. What is grain's own is the assertion that stops it
recurring: `assertGuestHasEgress`, in
`TestKonturSandboxesAgainstARealDockerBackedVM`, compares the guest's own
default route against the gateway docker reports for that VM's network
namespace -- so the claim under test is "the guest took over the
namespace's route out", not "the runner happened to use 172.17.0.1". It
runs in the `real-vm` job, on every pull request and every push to
`main`.

NAT mode never had this: there `konturctl` fills the gateway in itself,
from the bridge CIDR it already knows. Neither mode gives the guest a
resolver of its own -- `ip=`'s DNS fields go unused, and what a guest
resolves with is whatever `/etc/resolv.conf` its image was built with,
which is worth revisiting separately.

## The UI

`pkg/ui`, served by `cmd/grain`'s "daemon" subcommand (bwsalmon/agents#237,
folded in by #363 -- it used to be its own "ui" subcommand), is
[`docs/data-model.md`'s "first-party UI"
direction](docs/data-model.md#direction-a-first-party-ui): create a
task, approve a proposed one (or withdraw that approval, which is what
[`docs/data-model.md`](docs/data-model.md#taskstate-is-derived-not-stored)
calls taking a queued task back out of the queue: the approval is a
declaration, and removing it leaves the task a proposal again), attach or
remove a capability, comment,
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
Neither `pkg/ui.Server` nor `cmd/grain`'s own CLI takes a GitHub
credential at all any more — the daemon that serves the UI/API takes
`-as`, naming the single principal every task and comment created
through it is attributed to (defaulting to the OS user), and every
caller of that API — the browser frontend, the CLI, anything else
reaching it — acts as that one principal. This is a single-operator tool,
reached however an operator's network puts it in front of them —
loopback, an SSH tunnel, Tailscale, IAP (bwsalmon/agents#363) — rather
than authenticated per caller; a real permission gate is worth building
the day this runs somewhere with more than one operator behind it
(bwsalmon/agents#237's follow-up), and it would gate store writes rather
than API calls.

**Why a local web server, not Electron/Tauri/a native app.** `go build`
already produces one dependency-free binary per OS `cmd/grain` runs on
(Mac, Linux today); a `net/http` server that opens the system's default
browser gets "runs standalone on Mac and Linux" for free, in the one language
every other substrate here already commits to (see "Why Go" above), with
no second *runtime* — Node, Rust, Xcode — for a deployed binary to carry.
"Set up to run on iOS/Android in the future" is what shapes `pkg/ui` into
an HTTP+JSON API in the first place rather than server-rendered pages: a
future mobile client — native, or a thin webview shell — is just another
caller of the same `/api/*` surface the daemon's own embedded frontend
already uses, with nothing about the server to rewrite — the same surface
`pkg/ui.HTTPClient` gives `cmd/grain`'s own CLI (see "The UI and the CLI
talk to the daemon over REST" below).

The frontend itself (`ui/`, bwsalmon/agents#356) is React built with
Vite, not the plain HTML/CSS/JS this section used to describe — that
earlier no-framework, no-build-step choice bought a repo `go build`
alone could produce, at the cost of every UI change being DOM plumbing
by hand (`el()`, manual diffing against `lastList`/`lastDetail` to avoid
stealing focus on a poll) in a ~1200-line file with nowhere to grow.
React and its ecosystem — component boundaries, hooks, the wider supply
of libraries a task UI eventually wants (routing, richer forms, charts)
— buys back the extensibility that file was starting to cost, and is
worth a real toolchain now that one is already needed to build it. What
survives from "why a local web server" is the deployment shape, not the
build step: `npm run build` (wired into `make build`/`test`/`vet` and
the `go-test` CI job, and into `Dockerfile.build` for `container-build`)
has to run before `go build` can see real content in `pkg/ui/static/` —
the directory it `//go:embed`s — but that step runs once, at build time;
the artifact `cmd/grain` ships is still the one dependency-free Go
binary this section opened with, with the built frontend baked into it
rather than a Node runtime tagging along.

**Material UI (bwsalmon/agents#450) for primitives, not for its default
look.** Every interactive element — buttons, text fields, checkboxes,
selects, dialogs, chips, the error toast — is an `@mui/material`
component now rather than a bespoke `<button className="primary">` or a
hand-rolled dropdown; that buys the accessibility, keyboard handling and
focus management (a modal that traps focus and closes on Escape, a
select that behaves like a native one) those were quietly missing,
without every screen reinventing it. `AppThemeProvider`
(`ui/src/AppThemeProvider.jsx`) feeds MUI's own `ThemeProvider` a theme
(`theme.js`) built from the same accent/danger/surface values
`style.css`'s `:root` tokens already defined (bwsalmon/agents#364's
Plane-inspired palette), so adopting MUI's components didn't also mean
adopting Material Design's own visual language — the dense, dot-not-pill
task rows and status colors this section's own screenshots would show
are unchanged. `style.css` still owns what MUI has no primitive for: the
state dot/badge, the sidebar's brand mark, and layout for the task list
and detail panel.

**`grain demo` (bwsalmon/agents#276, folded into its own subcommand by
#363) for trying out the frontend on its own.** A real `grain daemon`
needs a real Gemini key, a real store, and a real deployment's tasks to
look at anything. `grain demo` opens a throwaway embedded store in a
fresh temp directory instead and seeds it with fake tasks, one in each
`model.State` (`cmd/grain/demo.go`), through the same `ui.Client`/
`model.Store` writes a human clicking through the UI would make — no fake
`Store` standing in, matching the "real embedded SQLite, not a fake"
discipline every test in this repo already holds to
(`pkg/ui/client_test.go`). That makes it a real server exercising the real
frontend code, with fake data as the only difference from a real
deployment — useful for checking a frontend change renders every state
correctly without an orchestrator, a sandbox, a Gemini key, a real store,
or a git repo anywhere behind it. It takes no `-data-dir` of its own —
there is no real store to point it at by mistake, only the throwaway one
it creates and seeds itself.

**Freshness, not a cache.** Every mutation in the frontend
(`ui/src/App.jsx`'s `act`) re-fetches the task afterward rather than
assuming its own optimistic update is now true, matching the direction
document's "it shows freshness for anything" — read live from the store
rather than presenting a stale value as current. There is nowhere here
for staleness to hide since nothing is ever cached across one request.

**And it refreshes itself.** A task changes state when `graind`
dispatches it, when a run finishes, and when a pull request merges —
none of which the browser is told about, so without a poll the screen
only moves when somebody clicks. `App.jsx` re-reads every three seconds,
skipping the tick entirely while the tab is hidden and never overlapping
itself.

Two details keep that useful rather than annoying. React's own
reconciliation already skips DOM writes for output that hasn't actually
changed, so an idle screen polled every three seconds never flickers,
loses focus, or resets its scroll — the vanilla-JS frontend this
replaced had to do that by hand, diffing a poll's JSON against
`lastList`/`lastDetail` before deciding whether to re-render at all, and
this needs none of it. And when the open task does change, an unsent
comment survives the re-render because `DetailOverlay.jsx`'s comment box
is an uncontrolled input: React never touches a `<textarea>`'s own DOM
value on a re-render, only on mount, so a reply someone is halfway
through typing is untouched by newer comments arriving underneath it.
Both behaviors were checked by driving the real UI in a browser.

Polling rather than a change feed is deliberate, and unlike when this
store ran on Dolt, there is no longer a substrate underneath it to
decline: SQLite gives grain no commit log and no ready-made diff to build
a watch on (see "Grain no longer keeps a commit history" above), so a
real feed would mean building one from scratch — a diff joined across
six tables to answer "what changed about this task" (a capability toggle
changes `task_grant`, a comment changes `task_comment`), a story for
history that grows without bound, and handling for a cursor that has
aged out. That is a real feature; polling is fifteen lines with nothing
to get wrong, and for one operator watching a handful of tasks on the
same machine the two are indistinguishable.

## Deployment configuration lives in the store too

bwsalmon/agents#320 asked the same "the store is the record" question
"Input is a model update, not a GitHub issue" (above) already answered
for tasks, aimed at the daemon's own flags this time: `-max-workers`,
`-poll-interval`, `-gemini-model`, `-claude-model`, `-max-agent-turns`, `-github-host`,
`-github-insecure-http`, `-gcp-project` and `-gcp-agent-service-account`
used to be the only way to set any of these, which meant changing one
meant restarting the daemon with a different command line, and there was
nothing a UI could show a human short of re-parsing that command line
somehow.

`model.Config` (`pkg/model/config.go`) and `Store.GetConfig`/`PutConfig`
are the store-backed answer: one row in `grain_config`, the same
one-row-per-deployment shape `grain_schema` already uses. `cmd/grain`'s
"daemon" subcommand's own `loadConfig` (`daemon.go`)
reads it at startup — and re-reads it on every reconcile tick, see
"Changing a setting doesn't mean restarting the daemon" below — and
writes those flags into it as a one-time seed
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
(`-gemini-api-key-file`, `-kontur-exec-key`) — bwsalmon/agents#320's own
"but not the secrets."

`pkg/ui.Settings`/`UpdateSettingsRequest` (`pkg/ui/settings.go`) and
`GET`/`PUT /api/settings` are what actually let something change it:
partial updates, the same nil-means-leave-this-one-alone convention
`UpdateTaskRequest` already uses for a task's own fields, applied as a
read-modify-write against whatever `grain_config` currently holds (or
the zero `model.Config`, the first time). `grain settings` is the CLI
side of the same `Client` methods — no flags prints what is stored (or
that nothing is, yet); any flags apply just those, the way `grain
update` already treats a task's own flags. Every store-backed field has
a flag there, all three dimensions of the deployment-wide sandbox VM
shape (`-sandbox-cpus`/`-sandbox-memory-mb`/`-sandbox-disk-gb`) included:
a setting reachable only
from the Settings pane would be one a deployment could not be configured
from a shell, which is where `grain sync` and every scripted setup
already live. Unset, vCPUs and memory print as the shape actually in
effect — `bwsalmon/kontur`'s own default, carried alongside the stored
value as `sandboxCpusDefault`/`sandboxMemoryMbDefault` — rather than as
the bare `0` that is stored, since a literal `0` reads as a deliberately
empty VM. Disk has no such default to print beside it, deliberately (see
`Settings`' own doc comment): a VM's disk is however large the guest
image behind it happens to be, which is a property of the image a
deployment built rather than a constant this build could name, so unset
disk prints as `unset` instead of a number that would be wrong for
anyone who rebuilt their guest.

`ui/` (bwsalmon/agents#333) now has a settings panel too — the topbar's
"Settings" button opens a form reading `GET /api/settings`,
distinguishing `configured: false` (nothing saved yet, before any daemon
has started or any value set) from a populated one the same way `grain
settings` (no flags) already does. Saving sends only the fields an
operator actually changed via `PUT`, leaving the rest out of the request
entirely so they can't clobber what's already stored — the same
partial-update contract `UpdateSettingsRequest`'s pointer fields already
give a CLI caller. A 400's `ValidationError` message (a bad duration
string, an empty required field the first time) surfaces through the
same error banner task creation's own validation errors already use.

### Changing a setting doesn't mean restarting the daemon

Storing the configuration was only half of it. `loadConfig` read
`grain_config` exactly once, at startup, so *saving* a setting and
*applying* one had come apart: the Settings pane wrote a row and then
showed it back as though something were now running that way, when in
fact nothing but the concurrency limit (re-read by `RunCycle` itself) would
change until someone restarted the process. An operator raising the turn
cap, switching models, or widening a sandbox saw a saved value and no
different behaviour, with nothing on screen to say why.

`cmd/grain/daemon.go`'s `liveConfig` closes that gap. The reconcile loop
is the deployment's own heartbeat, so it is also where a settings change
is noticed: once per tick, before the cycle, `liveConfig.refresh`
re-reads `grain_config` and hands each change to whatever it configures
— its own ticker for `poll-interval`, a rebuilt `model.CapabilityRegistry`
for `gcp-project`/`gcp-agent-service-account`, and
`KonturSandboxes.SetDefaultShape` for
`sandbox-cpus`/`sandbox-memory-mb`/`sandbox-disk-gb`.
The rest were already read per cycle or per dispatch, or gained it here:
`RunCycle` re-reads `max-workers`/`max-mergers` *and* `max-agent-turns`;
`dispatchConfig` re-reads `agent-framework`, `gemini-model` and
`claude-model` when a run's framework is built (which is per dispatch,
for the same reason the credential is); and `target-repos`,
`newest-first`, `environment-name` and the three "by default" toggles are
read out of the store by `pkg/ui` on the request that needs them. What a
change costs is therefore at most one poll interval, and nothing already
in flight is disturbed: `Deps` is copied per cycle and per dispatch, so a
run keeps the registry and the caps it started under.

Two settings genuinely cannot be swapped under a live deployment:
`github-host` and `github-insecure-http`. They are baked into the git
proxy's forwarder, the GitHub REST transport and the `github-sandbox`
capability provider when the process starts, each of them read without
synchronisation by requests already in flight, so changing one *is* a
data race rather than a setting change. `liveConfig` deliberately keeps
this process's own startup value for those, which is what makes
"what is running" a different question from "what is stored" — and lets
the answer be reported rather than guessed at. `pkg/ui.Settings` carries
`restartRequired` (the constant list, so the field can be annotated
before anyone touches it) and `pendingRestart` (the subset whose stored
value has actually diverged from `Config.RunningConfig`, the running
daemon's own view of itself). The Settings pane annotates those fields
with a "needs restart" badge, turns it into a warning-coloured "restart
to apply" once one has been changed, and raises a banner naming what is
saved but not yet running; `grain settings` prints the same thing as a
closing line. `restartOnlySettings` in `pkg/ui/settings.go` is the one
list both ends read, so a setting cannot be applied live *and* annotated,
nor left needing a restart in silence.

### Telling one deployment from another

`model.Config.EnvironmentName` (grain/task-69) is a name an operator
gives a deployment — "staging", "dev", a hostname — and it is the only
setting here that changes nothing the daemon does. Nothing dispatches,
sandboxes, or authenticates differently because of it; `pkg/ui` is the
only thing that reads it at all.

It exists for the one mistake a single-operator cluster invites. Two
deployments of grain are pixel-identical: the same sidebar, the same task
list, the same Merge and Approve and reboot buttons, and nothing on
screen saying which store is behind them. Approving on the wrong tab is
therefore a mis-click rather than a mistake anyone could have caught, and
no amount of `target-repos` fixes it — that setting refuses a repo, while
this one answers "which deployment am I looking at" *before* the click.

So it is a label, and rendered like one: a warning-coloured badge beside
the grain mark in the sidebar (`Sidebar.jsx`, on screen in every view),
and the same name in front of the browser tab's own title (`App.jsx` —
first, not last, because a narrow tab truncates its title from the end
and `grain — sta…` would say nothing this is for). Empty, the default and
what every deployment upgrading across this reads back, draws neither:
an operator running one deployment has nothing to be told apart from, and
grain's own shape is the one it has always had.

It rides on `GET /api/config` as well as `GET /api/settings`, unlike most
of what the Settings pane edits: the frontend needs it on first paint, on
every view, and `/api/config` is the one call `App.jsx` makes before it
renders anything. Free text, since what environments a deployment sits
among is the operator's own vocabulary and grain has no list to validate
against — `ui.UpdateSettings` only trims it (so a stray space is not the
difference between named and unnamed) and bounds it to 32 runes with no
line breaks, which is what it takes for a badge to stay a badge.

### A capability can be ready and still ungrantable

The Settings pane's Capabilities tab answers "would this capability work
if a task were granted it": which deployment settings it still needs
(`missingConfig`), which secrets are unset (`missingSecrets`), and
`ready` when neither list has anything in it. That turns out not to be
the whole question, and the half it left out is the one that is harder to
see.

Which capabilities a task can be granted at all is decided somewhere
else entirely: `ui.OfferedCapabilities` (`pkg/ui/labels.go`, named
`DefaultCapabilities` when this was written) is the
picker's listing, and `grantsFor`/`SetCapability` reject any id it has no
row for as "unknown capability" before a `model.Grant` is ever written.
The set of capabilities `cmd/grain/daemon.go`'s `capabilityProviders`
*registers* is a different list, maintained by hand in a different file,
and the two have drifted: `gcp-key` and `github-sandbox` are registered
by the daemon and absent from the picker. A deployment can therefore
have `gcp-key` fully configured — a project, an agent service account, a
`gcp-key-minter` secret, the tab showing **Ready** — and still never mint
a key, because no task has ever been able to ask for one. Nothing failed:
`ResolveGrants` was never reached, so there is no refusal in a run's
output, no error in the daemon's log, and nothing at all in the sandbox
where `/home/debian/.gcp-service-account.json` should be. This is the
second distinct way `grain-gcp-key` has failed to reach a sandbox — see
`konturSandbox.PlaceFile` above for the first, which was fixed — and
unlike that one it is invisible from every surface an operator had.

`CapabilityStatus.Grantable` is that missing half, reported alongside
`Ready` rather than folded into it, because the two name gaps that are
fixed in opposite places: an unready capability is this deployment's
configuration, while an ungrantable one is grain's own code and no amount
of configuring will move it. The Capabilities tab badges it "Not
grantable" with a line saying configuration cannot fix it, and `grain
settings` — which until now printed no capability information at all,
leaving a shell on the host with no way to ask this question — prints the
whole table, both gaps named separately.

Reporting the drift is not repairing it. Whether `gcp-key` should join
the picker, or be granted to every dispatch the way v1 minted one
unconditionally per sandbox (`gcp_keys.py`: "every sandbox, every
dispatch... rather than a task label"), is a design question this leaves
open; what changes here is that a deployment in that state now says so.

### A default set of capabilities, seeded onto the task

Both. `gcp-key` and `github-sandbox` got picker rows first, and
`model.Config.DefaultCapabilities` is the other half: a deployment-wide
set of capability ids, chosen on the Settings pane's Capabilities tab,
that every new task is filed already holding. A deployment that wants a
service-account key in every sandbox — v1's shape — ticks `gcp-key` once
and stops thinking about it.

What it is *not* is v1's per-dispatch mint restored. The set is read at
creation, by `ui.CreateTask`, and written onto the task as ordinary
`model.Grant`s (`GrantByDefault`, provenance only — nothing reads `Via`
to decide what a grant does). It is never consulted again at dispatch.
That one choice answers the whole question the picker rows left open:

- **The default is modifiable, which is what was actually asked for.**
  The new-task form opens with those boxes already ticked (`GET
  /api/config`'s `defaultCapabilities`) and sends the resulting list, so
  unticking one files the task without it. Afterwards it detaches from
  the task like any other grant. A deployment-level set read at dispatch
  could be neither seen on the task nor taken off one.
- **A failed mint stays a failed dispatch, and needs no degrade tier.**
  `prepareCapabilities` treats a refused resolve or a failed materialize
  as no dispatch at all, and that stays true for a defaulted grant.
  v1 needed its local `except` because nothing held the request: the
  mint happened per dispatch, for every sandbox, with nowhere to record
  that it had failed, so swallowing the error was the only way a broken
  minter did not stop the deployment. Here the grant is on one task, the
  failure is that task's, and the fix — repair the capability, detach it
  from the task, or drop it from the default set — is reachable from the
  failure. Running an agent while quietly withholding a capability its
  task is recorded as holding would trade a loud stop for a run that
  does the wrong work.
- **`Grantable` keeps its meaning.** A capability must have a picker row
  to be defaulted at all (`UpdateSettings` validates the set against
  `OfferedCapabilities`, which is what `DefaultCapabilities` was renamed
  to, since the two names now mean different things). That row is also
  what lets a human drop it from one task. `CapabilityStatus.Default` is
  reported next to `Ready`/`Grantable` rather than folded into either:
  the Capabilities tab and `grain settings` both flag a defaulted
  capability that is not ready, because that is a deployment-wide
  problem — every task filed will fail on it — rather than a per-task
  one.

The cost, stated plainly: turning an entry off does not disarm the tasks
already filed with it, since they hold their own grants. That is the
same property that makes a default modifiable in the first place.

A stored id this build no longer offers (a renamed capability, the way
`scratch-repo` became `github-sandbox`) is skipped at creation rather
than failing it — `UpdateSettings` refuses an unknown id on the way in,
so a stale entry can only come from an upgrade, and a settings row left
behind must not become a deployment where no task can be filed at all.
Per-repo defaults are the next step and resolve in the same place
(`(*ui.Client).defaultCapabilities`); they compose as more ids in the
set a new task starts with, which is a different thing from
docs/data-model.md's folder `offers`, those being floors a task cannot
drop rather than a seed it can.

### The same set, per repo

The ask task-14 came from also said "we will also want this to be
possible on individual repos in the future," and this is that: a repo can
name capabilities of its own that a task filed against it starts holding,
on top of whatever the deployment already defaults.

**Where it is stored is a new `repo_config` table**, keyed `(owner,
name)` the same way `qualification_config` already is, holding
`model.RepoConfig` — one field today, `DefaultCapabilities`, with the
same comma-separated storage `grain_config.default_capabilities` uses.
A new table rather than a column somewhere: `base`, `preamble` and
`max_concurrent` are docs/data-model.md's own next three per-repo
settings, and this is the row they would join. A repo has a row only
while it has something of its own to say — `PutRepoConfig` deletes rather
than writing one that says nothing, so "has a row" and "adds something"
stay one fact and nothing has to filter empty rows back out.

**It is deliberately not the folder `offers` tree**, which the same
document describes and which stays available for what it is for. An
offer is a *floor*: unioned in when a task's grants are resolved, not
droppable by the task. Everything here is a *seed*: written onto the task
at creation, visible on it, and untickable on the form that files it.
Mixing the two silently is the failure worth avoiding in both directions
— a human unticking a capability and getting it anyway, or an operator
setting what they think is a floor and watching tasks file without it —
so they compose at different moments and neither feeds the other.

**The two layers union, deployment-wide first, and a repo can only
widen.** `(*ui.Client).defaultCapabilities` is still the one place a new
task's starting set resolves; it now takes the target repo
`CreateTask` has already parsed, defaults and `NoRepo` included. A
`NoRepo` task has nothing to key the second layer on and gets the
deployment's set alone; a task that named no repo is filed against
`Config.DefaultTarget` and gets *that* repo's defaults, because the layer
is keyed on the repo the task ends up targeting rather than on whether
the request spelled it out.

Whether a repo can *subtract* — "everything except `gcp-key` here" — is
the same "except here" question docs/data-model.md defers for ceilings,
and it gets the same answer: not yet, and the first person who needs it
is the signal. Until then the deployment-wide set is for what genuinely
belongs everywhere, a repo lists what it needs, and whoever files a task
can untick any of it on the form.

**What Settings reports had to gain a second axis.**
`CapabilityStatus.Default` used to mean "this deployment defaults this",
and with two layers a single flag would describe a deployment-wide
default that only some tasks actually get. So `Default` keeps exactly its
old meaning — every task, wherever it points — and
`CapabilityStatus.DefaultRepos` names the repos that default it on their
own. The Capabilities tab shows both, `grain settings` prints both, and a
repo that restates something the deployment already gives appears in
both, since dropping the deployment-wide entry leaves the repo's own
standing.

Editing lives where the thing being edited does: the deployment-wide set
on Settings' Capabilities tab, a repo's own on the repos page next to
that repo (`GET`/`PUT /api/repos/{owner}/{name}/capabilities`). The
new-task form resolves the union itself, from `GET /api/config`'s
`repoDefaultCapabilities`, so changing the repo picker re-ticks the boxes
for the repo now targeted — unless the picker has been touched by hand,
after which the ticks are the human's and a re-seed that put back
something they had just unticked would file a task with what they had
already said no to.

**Schedules, templates and suites still are not seeded**, and this is the
second time that has been decided rather than merely deferred. Each
carries a grant set somebody authored once, in a form of its own, and
those forms edit an existing set as often as they create one. Seeding
their pickers would write today's defaults into a stored set that then
never tracks them again: the next save of an unrelated field would
silently widen a set somebody wrote down, which is the thing task-14
avoided by seeding at task creation and reading nothing at dispatch. A
schedule that wants `gcp-key` says so, once, where every task it files
can be traced back to it.

### A `grain repo` family, and why `-target-repos` stayed put

The layer above left a one-directional surface behind: `grain settings`
prints what each repo defaults ("default in: `owner/name`", on the
capability line it already printed) with no way from a shell to act on
what it just showed. `grain repo` (`cmd/grain/repo.go`, grain/task-36) is
that missing half — `list`, `capabilities [-set a,b] <owner/name>`, `add`
and `remove` — over four new `ui.HTTPClient` methods mirroring the
`ui.Client` ones the repos pane already calls.

**Why this and not schedules, templates or suites**, which are still
UI-only and stay that way. The CLI's subset was never "tasks only": it is
task management *plus deployment configuration* — `grain settings`,
`grain secrets`, `grain config` — because "why did this deployment do
that" is asked from a shell on the host at least as often as from a
browser. A repo's own defaults are deployment configuration by that
reading, and were the one member of the category with no spelling here.
Schedules, templates and suites are authored *content*: written once, in
a form built for writing them, and docs/schedules.md records their
absence from the CLI as an open gap waiting on somebody who needs it
rather than as a decision. Adding a `repo` family does not make them
next.

**`-target-repos` stays on `grain settings`.** The repos *pane* dropped
its own copy of that field when bwsalmon/agents#473 moved add/remove onto
the repo rows, but what it dropped was a comma-separated text box — a bad
control for a human and a perfectly good flag for a script. The field
itself is still deployment-wide configuration (`model.Config.
TargetRepos`), and it is the whole-list form `grain sync`'s own
`settings` section already speaks verbatim (`ui.UpdateSettingsRequest.
TargetRepos`); removing the flag would leave the CLI unable to say
declaratively what a config file next to it can, and break existing
scripts to buy nothing. So both spellings exist and write the same field:
`-target-repos` replaces the list, `grain repo add`/`remove` change one
entry, and `grain repo add` says out loud when the list it prints back
has exactly one entry — an empty allowlist is what means *unrestricted*,
so the first repo added to a deployment that never restricted itself
narrows it rather than widening anything.

**`grain repo list` is composed on the client, from `GET /api/config` and
`GET /api/tasks`**, rather than served by a `GET /api/repos` of its own.
A repo is not a stored row: the folder tree is still unbuilt, so "a repo
grain knows about" is *derived* — whatever `targetRepos` names, union
whatever a task targets, union whatever carries defaults of its own — and
`ui/src/state.js`'s `repoRows` already derives it from those same two
responses. A third derivation on the server would be a second definition
to keep in step with the first. It does differ from `repoRows` in one
way, deliberately: a repo that carries defaults while being neither
allow-listed nor targeted still gets a row, because
`SetRepoDefaultCapabilities` permits exactly that repo to exist and a
list whose job includes reporting per-repo defaults must not be the one
place they are invisible. (The repos *page* still drops such a row; that
is a UI gap, filed separately, not a difference of opinion.)

## Write-only secrets access when colocated

`pkg/secrets.Store` (above, "no secret store in the model") was
read-only until bwsalmon/agents#357: `Resolve` was the whole surface,
since nothing except a dispatch resolving a capability's credential had
any reason to touch it. A UI running alongside the server is a
different caller with a different need — an operator who wants to set a
GitHub token or rotate a Gemini key without hand-editing files under
`-data-dir/secrets` over SSH.

`Store.Set`, `DeleteKey`, `DeleteSecret` and `List` are the added
surface. `List` reports `SecretInfo{Name, Keys}` for everything on
disk — names and key names, never a value — which is what lets a caller
show which secrets are set without this package ever handing one back
outside of `Resolve` itself. `Set`/`DeleteKey`/`DeleteSecret` now also
validate every path segment they're given (no `.`, `..`, or separator),
tightened onto `Resolve` too: it used to let a key contain `/` and
resolve wherever that led, which nothing exercised on purpose but which
writing a caller-supplied string to disk can no longer risk.

`pkg/ui`'s `Config.Secrets` is nil unless the deployment says otherwise.
Before bwsalmon/agents#363, that meant naming the *server's* `-data-dir`
from a second process — the standalone `grain ui`'s own `-server-data-dir`
flag, only useful when that UI happened to run on the same host as the
server it pointed at. Now that the UI only ever runs inside the daemon
that owns the store (`cmd/grain/daemon.go`'s own `startUIServer`, "The UI
and the CLI talk to the daemon over REST" below), it always has that
directory to hand and wires it up unconditionally — there is no longer a
cross-process case where it would not, and no flag to set. `grain demo`'s
own throwaway UI is the one caller that still leaves it nil, on purpose:
a fake store seeded with fake tasks has no real secrets to manage either.
`GET /api/secrets` reports `{enabled, secrets}` either way, so the
frontend's secrets pane can hide its controls behind a note rather than
show ones that would only ever 404; `PUT`/`DELETE
/api/secrets/{secret}/{key}` and `DELETE /api/secrets/{secret}` are the
set/delete-one-key/delete-the-whole-secret surface, each answering with
the refreshed `{enabled, secrets}` the same way a mutating task route
answers with the task. `grain secrets` (`cmd/grain/secrets.go`) is the
CLI side, a mode of its own alongside `daemon`/`mcpserver` rather than a
`runCLI` verb, since it has nothing to do with the task store and
opening one for it would be pure overhead: `-data-dir` here means what
`grain daemon`'s own `-data-dir` does (secrets live at
`<data-dir>/secrets`), `list`/`set`/`delete` mirror the API one-to-one,
and `set` takes its value from `-value-file` or, left unset, from
stdin — deliberately never from an argv flag, which any other process
on the same host could read back out of this one's own command line.
Unlike the UI, it never goes through the daemon's own REST API: it edits
the files directly, so it works even when no daemon is running.

`Config.Reboot` (bwsalmon/agents#395) is the same nil-means-unavailable
shape as `Config.Secrets`, for a much smaller surface: a func the UI's
"reboot host" button in the settings panel calls to reboot the machine
`grain daemon` is itself running on, in place of an SSH session an
operator would otherwise need just to run one command. `startUIServer`
wires it to `sudo systemctl reboot` unconditionally, the same command
v1's `reboot_controller` MCP tool already ran (`grain/automation/mcp_server.py`)
for a task holding the `self-repair` capability — the difference here is
who is pulling the trigger, a human at the UI rather than a task granted
that capability, so there is no capability to gate it behind.
`scripts/setup.sh` grants `$GRAIN_USER` (the unprivileged account
`grain-daemon.service` runs as) passwordless sudo for exactly that one
command line, the same narrow-as-possible sudoers shape
`provision/controller.sh` already uses for v1. `GET /api/config` reports
`rebootEnabled` so the button can hide itself rather than offer an action
that would only ever 404 -- the case for `grain demo`'s throwaway UI,
which leaves `Config.Reboot` nil since there is no real machine behind it
worth rebooting.

## Two agent frameworks, either per task

`agent/antigravity` and `agent/claude` both existed for a while before
either was actually a choice: `model.Config.AgentFramework` (bwsalmon/agents#609)
stored one, and `cmd/grain`'s `buildAgentFramework` (#615) read it once,
at startup, to build the single `agent.Framework` every dispatch then
used. Two things were wrong with that shape, and they were the same
thing twice: the framework was decided too early, and the credential it
needed was decided somewhere an operator could not reach.

`Deps.Framework` is a factory taking a name now --
`func(ctx, framework string) (agent.Framework, error)` -- called per
dispatch with the task's own `model.Task.AgentFramework`. Empty is the
common case and means "this deployment's default", which
`cmd/grain`'s own `defaultAgentFramework` resolves by re-reading
`grain_config` on each dispatch rather than from the config loaded at
startup: switching the default in Settings takes effect on the next run,
not the next restart. A task that names one instead
(`agentFramework` on `POST /api/tasks`, or the picker under New task ->
Advanced options) overrides it for that task alone -- the same
"zero means unset" per-task override
`SandboxCPUs`/`SandboxMemoryMB`/`SandboxDiskGB`
already are, for the same reason: a task filed with no choice must
follow the deployment wherever it is set later, rather than pin itself
to whatever was configured the moment it was filed.

The credential each framework runs as moved with it. Both are stored in
this deployment's own secrets database now, under the two well-known
names `pkg/secrets` exports (`GeminiAPIKeySecret`,
`ClaudeOAuthTokenSecret`), and the Settings pane writes them: a
write-only field per framework, a set/not-set chip, and a Clear button,
backed by `GET`/`PUT`/`DELETE /api/agent-keys/{framework}`. Nothing
reads a value back out through the API, the same rule the secrets pane
those are built on already holds to. `-gemini-api-key-file` and
`-claude-oauth-token-file` still work and still seed a deployment
(`scripts/setup.sh`, and `terraform/gcp`'s own
`grain-gemini-api-key`/`grain-claude-oauth-token` instance metadata),
but a key set through the UI wins over either, and takes effect on the
next dispatch with no restart -- `agentCredential` reads the database
first, then the file, on every framework it builds.

What that costs is one client construction per dispatch instead of one
per process, which is nothing beside the run it precedes. What it buys
is that a daemon with no credential at all is now a perfectly ordinary
state: it starts, serves the UI, and says which keys are missing --
where before, `runDaemon` failed outright on a missing key file, leaving
an operator with a UI reporting `reconcilerDown` and no way to fix it
from there. A run whose framework has no credential fails as its own
`setup-failed` run naming the pane to set it in
(`orchestrator.runOne` builds the framework inside its setup guard,
before `RunDispatch` takes over finishing the run), and
`scripts/setup.sh` no longer holds `grain-daemon.service` back until a
Gemini key exists: a UI that is not running is a credential that can
never be pasted in.

Both frameworks need a binary on the host, and that requirement
outlived the wiring above. It was once claude's alone: `agent/claude`
execs `claude` per dispatch, resolving a bare `"claude"` against the
daemon's own `$PATH` when `-claude-path` is unset -- and nothing in the
v2 deployment path ever put one there. v1 did (`provision/controller.sh`
installed it for the `grain-agent` account `claude -p` ran as), but when
the framework became a stored setting, and then a live per-task choice,
the binary was never brought along. A deployment could therefore offer
"claude" in Settings, report its OAuth token as set, and fail every run
it dispatched with `executable file not found in $PATH`.
`scripts/setup.sh` closed that with an `install_claude_cli` that ran the
CLI's own installer on every deployed host, on every run and whichever
framework was currently selected (selecting the other one reaches the
very next dispatch). bwsalmon/agents#645 moved it one step earlier:
`Dockerfile` installs it into the deployment image, in CI, so its
presence is settled when the image is built rather than depending on
every deployed host being able to reach claude.ai at deploy time -- see
"The deployment is a container" below. The readiness summary still
reports it alongside the two credentials, asked of the image rather than
of the host, and `GRAIN_CLAUDE_PATH` still names an operator's own copy,
which `setup.sh` bind-mounts into the container at that same path.

Replacing the home-grown Gemini runtime with the Antigravity CLI ("The
agent runtime is a CLI now", above) gave the other framework the same
requirement: `agy` is a binary too, and for a while it was the one
nothing installed anywhere -- an operator's own manual step on every
host, for the *default* framework, which made "this deployment cannot
dispatch anything" a state it could sit in indefinitely. `Dockerfile`
installs both agent CLIs now (bwsalmon/agents#645), from their own
installers, in CI: an image carrying one of them is an image that fails
every run choosing the other, and which one a run chooses is a live
per-task decision. `scripts/setup.sh` checks the image for both
(`verify_agent_cli`) and reports each in its readiness summary;
`GRAIN_AGY_PATH`/`GRAIN_CLAUDE_PATH` still override either with a copy on
the host, bind-mounted in at the path they name.
`buildAntigravityFramework` fails the same way `buildClaudeFramework`
does when one is missing: naming the install, not the `$PATH` lookup, so
an operator reads a missing binary rather than a broken grain.

`grain-daemon.service` also exports a `HOME` that exists now
(`$GRAIN_DATA_DIR/home`). `$GRAIN_USER` is created `--no-create-home`,
so systemd would otherwise hand the daemon the `/home/grain` its passwd
entry names and nothing ever creates -- which the daemon itself never
minded and the claude CLI, which writes its own state under `$HOME`,
would. `agy` needs nothing from it: `agent/antigravity` hands every run
a private `HOME` of its own, for the per-run MCP isolation described
above.

One consequence worth naming: two frameworks writing into one
`TranscriptDir` means two transcript formats in it at once, so
`ui.Config.LiveTranscripts` can no longer be whichever reader matched
the deployment's framework at startup. `cmd/grain`'s
`liveTranscripts.Tail` picks per file instead.

*How* it picks changed with the runtime replacement. While one framework
tee'd an already-readable narrative, "does the file open with a JSON
object" separated them. Both mirror their subprocess's own NDJSON now --
claude's `--output-format stream-json`, agy's -- so the discriminator is
the key each vocabulary tags its events with instead: claude's carry
`type`, agy's carry `event` (`transcriptIsClaude`). It sniffs the first
line that *parses* rather than the first line, since reading a file the
framework is still appending to routinely catches a half-written one. A
run's finished transcript needs none of this, since
`agent.Result.Transcript` is already rendered text by the time the store
sees it.

## The UI and the CLI talk to the daemon over REST

Dolt permitted one writer when embedded, which suited a cron-driven
controller and did not suit a controller plus a UI plus a human at a
CLI, each opening the store directly. That became real the moment the
CLI and the UI started writing the store instead of GitHub ("Input is a
model update, not a GitHub issue", above): for a while, the answer was a
second writer *class* — `dolt.Connect` dialing a real Dolt SQL server
instead of the embedded database, so a daemon, a UI and a CLI could all
hold their own connection open at once, and later, once
bwsalmon/agents#366 replaced Dolt with embedded SQLite, simply calling
`sqlite.Open` (`pkg/model/sqlite`) on the same file directly: SQLite has
no wire protocol to serve in the first place, so its own file locking —
WAL mode, so a reader is never blocked by the one writer holding the
lock; `_txlock=immediate` and a five-second `busy_timeout`, so an
overlapping writer waits its turn or fails outright rather than
corrupting anything (`pkg/model/sqlite`'s own doc comment; "Locking, not
merging" above) — was enough to serialise a daemon, a UI and a CLI all
writing the same file at once, no server process required.

bwsalmon/agents#363 removed the second writer *entirely* rather than
scaling it. The daemon now serves `pkg/ui.Server` itself, in-process
(`cmd/grain/daemon.go`'s own `startUIServer`, gated on `-ui-addr`), over
the exact `*model.Store` `RunCycle` already has open — no second
connection, and no separate store process needed just to let the two
coexist. `cmd/grain`'s task CLI stopped opening the store at all: it is
`pkg/ui.HTTPClient` now, a plain REST client of that same server (`-server`,
default `http://127.0.0.1:8420`), the identical JSON API the browser
frontend already speaks. The frontend and the CLI reach that one process
however an operator's network puts it in front of them — a loopback
port, an SSH tunnel, Tailscale, IAP — which is also the whole
answer to "does the API need its own auth": it doesn't, because nothing
downstream of the daemon's own store connection accepts one, and
whatever can reach `-ui-addr` at all acts as the daemon's one configured
principal (`-as`). `scripts/setup.sh` reflects this: one systemd unit
(`grain-daemon.service`), one store, no separate store process to run
alongside it.

There is no `-store-addr` or equivalent anymore, either: SQLite has no
server mode to dial in the first place (`pkg/model/sqlite`'s own doc
comment), so `grain daemon`'s `-data-dir` always names a plain file on
its own disk, and every mode that ever took a store flag — `grain
daemon`, and, before #363, the standalone `grain ui` and the CLI itself
— takes just that one, with no "embedded, or a server" distinction left
to make.

## A run can open its own pull request

"Letting a run watch its own CI" (above) gave a run `pull_request_status`
-- what GitHub says about the branch it has pushed. This is the other
half: the pull request itself.

grain has always opened a pull request for the branch a run pushed --
after the run had already exited (`orchestrator.ProcessResult` ->
`salvagePushedBranch` -> `finishWithPullRequest`). That ordering has one
cost: a check that only ever runs *on a pull request* -- which is most of
them, `.github/workflows/tests.yml`'s own `on: pull_request` included --
has nothing to run against until the run is over. A change that builds
and tests cleanly in a sandbox and then fails the repo's own workflow is
then a fact nobody learns until a human opens the pull request, and
fixing it costs a whole second dispatch -- of an agent that has by then
forgotten everything it knew about the change.

So a run can now ask for its pull request while it is still running, and
read back what the checks say: the `open_pull_request` tool (`pkg/mcp`'s
`NewOpenPullRequestTools`). It takes no arguments -- repo, branch, base and
title are grain's, exactly as they always were -- and it opens *the same*
pull request the finish path would have (`EnsurePullRequest`, which finds
an already-open one for the head before opening anything), so the run's
own ending adopts it rather than colliding with it. Calling it again is
how an agent watches a check that was still running: it never opens a
second pull request, and the checks it reports are read fresh
(`checkRunsFor`, the same reader the merge queue trusts, Actions-workflow
fallback included).

Two things it deliberately does not do. It does not mark the task
completed -- that is what would put a still-running task into
`SyncPullRequests`' merge queue, where a branch the agent is still
pushing to could be merged out from under it; `CompletedAt` stays the
finish path's to set. And it does not open anything from the `mcpserver`
process itself. That process does hold a GitHub client -- the read-only,
one-branch one `pull_request_status` uses -- but opening a pull request
is a *write*, and which branch is opened against which base has always
been grain's decision rather than an agent's. So `-server`/`-task` point
it at the running daemon's own REST API instead --
`POST /api/tasks/{id}/pull-request`, one call about one task id, answered
by `orchestrator.OpenPullRequestForTask` against the daemon's own GitHub
client. Everything about which repo and which branch that means is read
from the task's own record, so nothing an agent can put in a tool call
reaches GitHub as data.

That is a route back from a forked `mcpserver` to the daemon, which "What
this cost" (above) records as not existing. It is a deliberately narrow
one: a REST client, one endpoint, one task id, no store handle -- not the
in-process `*model.Store` `selfrepair.Confirm`'s blocking confirmation
would still need.

### Telling the run it has it

A tool nobody reads about is a tool nobody calls. `BuildPrompt` names
`open_pull_request` in the same paragraph as the push/check/repair loop,
so a run learns it can open its pull request and read CI without having
studied the roster -- which is the whole failure the tool exists to fix,
one level up: an agent that finishes and exits never sees its own checks.

It is named only for a run that really has it. Registration turns on
`-server`/`-task`, and the `-server` half comes from `-ui-addr`
(`daemonServerURL`), so a deployment serving no UI/API gives its runs no
such tool -- and a prompt that promised it there would send an agent
after something that is not on its roster. The one thing that knows is
the `Framework` itself, which is why it answers for its own runs
(`agent.PullRequestFramework`, implemented by `pkg/agent/claude` and
`pkg/agent/antigravity` as "was I built `WithGrainServer`?");
`RunDispatch` asks, and passes the answer to `BuildPrompt`. A `Framework`
that does not implement it at all answers no, which is the safe
direction: a run never told about a tool it happens to have loses one
convenience, where a run told to call one it does not have burns turns on
an error it cannot fix.

What that leaves worth measuring, rather than assuming, is whether runs
actually start calling it, and whether a run that sees a failing check
fixes it instead of opening the pull request and stopping there.

## A run can rebuild its own sandbox

Every tool a run has runs *inside* its sandbox. That is the whole design
-- the agent is on the controller, the work is in the guest, and
`run_command`/`read_file`/`edit_file`/`write_file` are the only crossing
(`pkg/mcp`). It also means a sandbox broken badly enough takes every one
of those down with it, and leaves the run with no move at all: a guest
that has stopped answering, a root filesystem an unlucky build filled, an
interrupted `apt`/`npm`/`docker` that left a state no command can
untangle, a process that will not die. The agent then spends whatever
turns it has left failing at things that have nothing to do with its
task, and the only recovery was for the run to end and the whole task to
be redispatched -- which throws away everything the agent had worked out
along with the broken sandbox.

`recreate_sandbox` (`pkg/mcp`'s `NewRecreateSandboxTools`) is the way
out. It takes no arguments, destroys this run's sandbox, builds an empty
one under the same name, and puts back what grain itself had set up in
the old one: the git credentials pointing at the proxy, whatever the
task's capabilities placed, the task's attachments, and a fresh clone of
its repo with its branch checked out. Then the run carries on, in the
same conversation, in a clean sandbox.

The name is what makes this possible without the run's tools going stale.
A sandbox is addressed by name -- a directory path, or a kontur VM whose
container name follows from the VM name (`kontur.PodName`) -- never by a
handle to the particular filesystem or guest behind it. So the tools the
run already has, and the ones its forked `mcpserver` holds in a separate
process that nothing here could reach to replace, address the new sandbox
the moment it exists. `orchestrator.SandboxRebuilder` is the one method
that adds: `konturSandbox.Rebuild` reuses Acquire's own create-and-wait
pair (`create` deletes whatever is under the name first, which is exactly
the destroy half), and `hostSandbox.Rebuild` its `RemoveAll`/`MkdirAll`
pair.

Both halves of that are driven rather than argued.
`tests/e2e/mcpserver_recreate_sandbox_test.go` puts a real `grain
mcpserver -server/-task` subprocess in front of a real `ui.Server` over a
real `SandboxRecreations` with a live `RunCycle` run registered in it,
rebuilds that run's sandbox mid-run, and then commits and pushes from the
clone that came back — through the same tool handles it held before the
rebuild, with the credentials grain re-minted for the same sandbox name.
`pkg/orchestrator/kontur_docker_real_test.go` (the `GRAIN_REAL_VM_TESTS`
job) makes the harder version of the argument on the other backend: a
`Rebuild` between two `run_command` calls on a real VM, checking that the
container really was replaced, that what is behind it is a new guest, and
that the second call still lands.

What the run cannot get back is its own uncommitted work, and the tool
says so in the one place an agent reads before deciding to call it: the
description. Commits already *pushed* are safe, because they are on the
remote rather than in the sandbox, and the re-clone continues the
existing remote branch rather than branching over it (`prepareCheckout`)
-- so a rebuild costs a run its unpushed work and nothing more.

The hop is the same one `open_pull_request` makes, for a sharper version
of the same reason. The `mcpserver` process could not do this even if it
were allowed to: creating a sandbox needs the shape this run asked for,
the proxy token to mint for it, the already-minted capability material to
place in it and the repo to clone into it, none of which exists on that
side. So `-server`/`-task` point it at
`POST /api/tasks/{id}/sandbox/recreate`, one call about one task id,
answered by `orchestrator.SandboxRecreations` -- a registry each
dispatched run puts itself in (`runOne`) and takes itself out of when it
ends. A task with no live run there is told so; nothing a tool call
carries chooses which sandbox is destroyed, because the task id was fixed
at process start.

Two details are worth knowing:

- **The capability placements are written back, never materialized
  again.** Re-materializing would mint a second credential and a second
  lease behind the back of the single revoke `RunDispatch` performs when
  the run ends. So `RunDispatch` hands the registry the
  `model.Materialized` it already has (`setMaterialized`) and the rebuild
  rewrites that same content, which is idempotent.
- **Only the rebuild itself can fail the call.** Everything after it
  comes back as a warning, because by then the old sandbox is gone and
  what the caller most needs is an account of what it is now sitting in
  front of -- the same reasoning `PullRequestStatus.ChecksError` follows.
  A run whose credentials did not come back cannot push, and one whose
  repo did not clone has an empty directory rather than the checkout
  everything else it was told assumes, so the rendered answer puts those
  in their own section rather than folding them in with what worked.

Unlike `open_pull_request`, `BuildPrompt` does not name this one. The
trigger for reaching for it -- a sandbox that has stopped working -- is
one an agent cannot miss and does not have to be taught to look for, and
the description is where it reads what the tool costs. Whether that holds
is worth watching: the failure mode to look for is a run that grinds on
against a wedged sandbox without ever trying it.

## Deploying it

`scripts/setup.sh` (bwsalmon/agents#355) is the first real answer to "how
does this run anywhere" — this file's own opening line used to say
nothing here was wired in yet, and now this is the one path that is. It
runs `grain` directly on the target machine as a single
`grain-daemon.service`, no controller VM involved: v2 has no host adapter
yet (see "What this does not have yet" above), and its daemon already
defaults to `orchestrator.HostSandboxes` — plain host directories, not a
VM — so a controller VM would have bought nothing v1's own shape needed
for a different reason (isolating a real per-task sandbox, which v2 does
not have either way yet). It *pulls* what it deploys rather than
building it — see "The deployment is a container" below — so `docker`
and systemd are the only things it needs of the host, and it installs
docker itself on a vanilla Debian VM that has none (`ensure_docker` --
bwsalmon/agents#617: until then, only `terraform/gcp/files/deploy.sh`
guaranteed it before ever invoking this script, which was no help to a
host that reaches this script the way this section's own opening line
describes -- putting it on a bare VM and running it). Everything else it
uses is either a shell builtin or part of a base system install; the
handful of steps that want a richer tool — a `git`, a `curl` — run one
out of the deployment image it is already pulling (`image_run`), which
is why it needs neither on the host and clones nothing. There used to be a second service
(`grain-ui.service`) and, before
bwsalmon/agents#366 replaced it with embedded SQLite, a `dolt sql-server`
container behind it, needed only because a daemon and a UI writing the
same store used to mean two writers ("The UI and the CLI talk to the
daemon over REST", above) — bwsalmon/agents#363 folded the UI into the
daemon itself, so there is still exactly one service here and no store
container. Safe to re-run: it is the installer and the
updater both, seeding a secret or a config value only the first time and
leaving anything already on disk alone every time after. `./setup.sh
--help` lists every setting.

`scripts/setup.sh` only ever *seeds* an already-minted
`GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE` — until bwsalmon/agents#358, nothing
in this repo minted one. `grain setup gcp` (`cmd/grain/setup.go`,
`pkg/gcpsetup`) is that missing piece: it creates the agent and minter
service accounts the gcp-key/gemini-key capabilities need, grants the
minter `roles/iam.serviceAccountKeyAdmin` on the agent account (and, with
`-enable-gemini-key`, `roles/serviceusage.apiKeysAdmin` on the project),
enables the APIs both calls need, and — with `-mint-key -key-out <path>`
— mints the minter's own key, ready to feed straight into `setup.sh` as
`GRAIN_GCP_SERVICE_ACCOUNT_KEY_FILE`. It authenticates with Application
Default Credentials by default, or `-credentials-file` for a specific
operator identity. Every step is get-or-create: running it again is a
no-op wherever the first run already succeeded. A step the credential it
ran as can't perform (typically an IAM grant, needing more than Editor)
is printed as a `gcloud` command to run by hand instead of aborting the
whole run — re-running `grain setup gcp` afterward picks up right there.
`pkg/gcpsetup/gcpsetup_test.go` covers the ordering, the idempotency, and
the manual-step fallback against a fake `Admin`, no real project involved
(the same bar `pkg/capability/gcpkey`'s own tests hold to); nobody has
run it against a real project yet — the "Accepted limits" list above
still says as much about GCP token minting generally, and this is a
bootstrap for that gap, not a live-verified closing of it.

`grain sync -config <path>` (`cmd/grain/sync.go`) is the reconfiguration
half: the command a GitHub Action calls whenever a config repo's checked-
in configuration changes. It reads one JSON file with up to two
independent sections — `"settings"`, unmarshaled straight into
`ui.UpdateSettingsRequest` and applied through `HTTPClient.UpdateSettings`
against `-server`, the same running daemon `grain settings` itself talks
to (bwsalmon/agents#363: there is no store flag here any more), and
`"gcp"`, which re-runs the exact `pkg/gcpsetup.EnsureInfrastructure` logic
`grain setup gcp` uses, so IAM drift (a binding removed by hand, a newly
enabled gemini-key rollout that needs a grant it didn't before) gets
repaired on every sync rather than only at install time. It never mints a
new minter key on a `sync` run — that stays a deliberate,
`grain setup gcp -mint-key` action. Reachability is the part this command
does not solve: the daemon's UI/API is bound to loopback only by default
(this section's own security note, above), so a workflow needs either a
self-hosted runner that *is* the deployment host (the simplest shape: the
workflow step becomes `grain sync -config deploy/grain.json`, `-server`'s
own default already pointing at loopback, no network hop at all) or a
tunnel of some kind — SSH, Tailscale, IAP — to wherever `-ui-addr` actually
listens. A `"gcp"`-only sync needs neither — just a GCP credential, the
same Workload Identity Federation the deploy workflow in the config repo
already uses works here too, with no static key in the workflow. `cmd/grain/sync_test.go` covers both sections' validation and,
for `"settings"`, a real round trip against an `httptest.Server` wrapping
an embedded store, including a second, no-op sync run.

Neither command invokes an agent to walk an operator through a manual
step — printing the exact command was judged enough for a first version;
see bwsalmon/agents#358's own "If there is enough we need to automate
manually we may want to invoke an agent" for the option this leaves open,
should the list of manual steps grow long enough on a real project to be
worth it.

## The deployment is a container

Everything above described a deployment that a host *built*: clone the
repo, run `make container-build`, install a binary, run it under systemd
with whatever else the machine happened to have. bwsalmon/agents#645
replaced that with one artifact. `Dockerfile` builds an image carrying
`grain` and every binary it shells out to — `git`, the docker CLI,
`konturctl`, the `claude` CLI —
`.github/workflows/build-artifacts.yml` builds and pushes it to GHCR on
every commit, and `scripts/setup.sh` pulls it.

What that buys is not build speed, though a deploy did stop costing
several minutes of Go and npm. It is that the set of things that have to
be true of a deployed host shrank to `docker` and systemd. Before, a host
could be running the right commit and still fail every dispatch because
its `claude` CLI install had 403'd months ago (`install_claude_cli` was
non-fatal on purpose — refusing to deploy over a blocked download would
have been worse), or because a `konturctl` built from a stale vendored
tree was still on its `PATH`, or because the deploy died on `make:
command not found` for a package nobody lists. None of those states can
exist now: the binaries and the daemon are one thing, versioned together,
and what CI proved buildable is byte-for-byte what runs.

Three tags per commit, each answering a different question:
`sha-<short sha>` is exactly what is running and can never move;
`<branch>` — that branch's name with `/` replaced by `-`, since a docker
tag may not contain one — is what a deployment tracking a branch follows,
and what the Upgrade button resolves; `latest` is `main`'s, under the
name a human types. The image job runs on every branch, not just `main`,
precisely because of the middle one: a branch with no image published for
it is a branch nobody can upgrade onto. Only the step that moves a
*shared* name -- `latest`, for either image -- is pinned to `main`.

There is no release of bare binaries. `grain` and the kontur binaries
were once published as assets on a rolling `build-latest` GitHub
Release; nothing ever fetched them, since a deployment pulls the image
and `konturctl` is a wrapper that execs into it, so the images are the
release and the only one.

`grain-daemon.service` is a `docker run` of that image. The unit itself
runs as root, because a docker client has to reach a root-owned socket to
ask for anything; the *container* runs as `$GRAIN_USER`'s own uid:gid, so
the store, the secrets database and every sandbox working tree come out
owned exactly as they were before any of this was containerized.
`setup.sh`'s `docker_run_args` is the whole list of what the process is
given, and the reasoning per entry lives there; the shape of it is: host
networking (the UI's port, and the git proxy every sandbox — a kontur VM
in its own netns included — has to reach), the data/sandbox/source
directories bind-mounted **at the paths they have on the host**, the
journal read-only for the UI's Logs pane, and the docker socket only when
kontur sandboxing or the Upgrade button actually needs it. Same-path
mounting is not tidiness: `konturctl` writes a VM's disk overlay at a
path and then hands that same path to the host's docker daemon as a bind
mount, so a path that meant two different directories would silently
produce a VM with the wrong disk.

Two things a container cannot do for itself, and how it asks instead.
Binding port 80 (`setup.sh`'s own default `-ui-addr`) needs
`CAP_NET_BIND_SERVICE`, and `--cap-add` alone grants a non-root process
nothing — so the image gives the `grain` binary the matching *file*
capability, which turns that bounding-set entry into a grant for that one
binary and for nothing else in the container, a task's own `bash -c`
included. And rebooting the host, or restarting the service, reaches a
systemd that is not there: the daemon touches a file under
`$GRAIN_DATA_DIR/control` and a `.path` unit on the host turns it into
the real command (`write_control_units`; `-reboot-cmd` and
`-upgrade-restart-cmd` are what point it at those files). That replaced
both `NOPASSWD` sudoers drop-ins this used to install, and grants
strictly less — there is no sudo rule left to widen, and the only two
things the daemon can cause are the two those units name.

`grain` and `konturctl` are still on the host's `PATH`, as two-line
wrappers around the same image (`install_cli_wrappers`), so an operator's
`grain list` and `kontur-diag.sh`'s `konturctl vm list` reach exactly the
build the service is running rather than a host copy that can drift from
it. `setup.sh` uses the `grain` one itself, for `grain schema-version`
and `grain secrets`.

A kontur deployment runs *two* images, and only one of them is grain.
The other is the sandbox each task's VM runs — both the container and
the guest inside it (`scripts/kontur/build-guest.sh`'s output, published
as `guest`) — and it used to be built on every host from that host's own
checkout, which is precisely how a deployment could end up running grain
from one commit and a sandbox from another. It is pulled now, and which
one is not something a deployment is told: CI publishes a sandbox per
commit, and the grain image built from that same commit
carries its reference, stamped in at link time
(`cmd/grain/sandboximage.go`, the Dockerfile's `SANDBOX_IMAGE` build
arg). `grain sandbox-image` prints it; `setup.sh` pulls whatever it
prints; and `pkg/upgrade`'s image path asks a *newly pulled* grain the
same question and pulls its sandbox before cutting over, so the two
halves move together or not at all. The stamp names the immutable
`sha-` tag rather than a branch, which is what makes a rollback ask for
its own older sandbox rather than whatever that branch points at today.

The guest *disk* used to be the one thing a deployment still built, and
that was not an oversight: `guest-setup.sh` baked the deployment's own
SSH public key into the image's `authorized_keys`, so a generically
published disk would either have carried a keypair everyone has or
admitted nobody at all. kontur generates that keypair per VM boot now
(`internal/guestkey`), so nothing deployment-specific reaches the disk —
and a guest is derived from a published kontur image by booting it,
provisioning it and committing the result, which yields an image that is
itself runnable. That is why the sandbox container and the guest stopped
being two artifacts: they are one, built once in CI, and a host builds
nothing at all.

What did not stay on the host: the git checkout, and with it `git` and
`jq`. `setup.sh` used to clone one, update it on every run, and re-exec
itself out of it when that update replaced the script mid-run; it now
needs nothing on a host but `docker` and systemd, and everything it
wanted a checkout for comes out of the image instead. The source is in
there (`/usr/local/share/grain/src`) for the self-debug capability,
which is the same drift that move was meant to close. The two steps that
want a real `git` (a `git ls-remote` at `GRAIN_TARGET_REPO`, and the
empty commit pushed to it) and the one that wanted `curl` and `jq` (the
GCP metadata token) all run inside that image too —
`setup.sh`'s own `image_run`. Keeping the copy of `setup.sh` on a host
current is the job of whatever put it there: on the GCP path,
`deploy.sh`, which clones and updates that checkout itself.

Both agent CLIs are in the image, not on the host: `claude` and `agy`
alike, installed from their own installers at build time ("Two agent
frameworks, either per task", above). `GRAIN_CLAUDE_PATH` and
`GRAIN_AGY_PATH` still name a copy on the host when a deployment has to
pin a particular version, and `setup.sh` bind-mounts whatever they name
at that same path.

## Upgrading from the UI

bwsalmon/agents#396 (filed "For v2") asked for a specific, narrow thing:
target a branch from the UI, and have an "Upgrade" button download it,
build it locally (containerized, since `make container-build` already
was — see "Deploying it" above), and start running the new version, a
host restart along the way accepted as fine for now. `pkg/upgrade` is
that, and nothing more: `Upgrader.Start` fetches and hard-resets
`-upgrade-src-dir` onto the given branch, runs `make container-build`
there, installs the binary to `-upgrade-install-path`, and — if
`-upgrade-restart-cmd` names one — runs a command to bring it up.
`GET /api/upgrade` reports how that went (`idle`/`running`/`ok`/
`failed`, with a detail string), persisted to a file under `-data-dir` so
it survives the very restart it triggers; `POST /api/upgrade` starts one
and serializes against a second call arriving while it's running.
Deliberately absent, the same way `grain sync`'s manual-step fallback
above stops short of walking an operator through it: no rollback if the
new binary is broken, and no health check before cutting over to it —
a build or install failure leaves the old binary running untouched
(`RestartCmd` is never reached unless every earlier step succeeded), but
a build that succeeds and then misbehaves at runtime is not something
this catches.

Checkout and build together are bounded by `Config.Timeout` (45 minutes
by default) rather than running unbounded, and every command either one
runs is killed by its whole process group, not just its own direct
child, once that bound trips — bwsalmon/agents#633 ("v2 Deploys are
hanging"): a stalled `git fetch` or a `make container-build` stuck on an
unresponsive docker registry used to leave `GET /api/upgrade` reporting
`running` forever, with no way for a second click to ever get past
`ErrUpgradeInProgress` short of restarting the whole daemon process by
hand.

Every flag is empty by default, which disables the feature entirely (the
UI's own Upgrade pane reports itself unavailable, the same convention the
Secrets pane already uses for its own optional `-server-data-dir`
wiring).

Since bwsalmon/agents#645 there are two pipelines behind that one button,
and `Config.Image` picks which (`pkg/upgrade/image.go`). A deployment
that runs from an image has no checkout to fetch into, no toolchain to
build with, and a binary at `-upgrade-install-path` that is not what the
service runs — so "upgrade to branch X" becomes: pull
`-upgrade-image:<tag for X>` (that branch with `/` replaced by `-`, the
same substitution CI makes when it pushes — `TagForBranch`), run it once
with `schema-version` as a health check, and write one
`GRAIN_IMAGE=<ref>` line into `-upgrade-image-ref-file` before restarting.

The unit reads that file as an `EnvironmentFile` and interpolates it into
its own `ExecStart`, which is the whole mechanism: an upgrade repoints a
deployment by writing one line, with no systemd unit to rewrite, no root
anywhere in the path, and the same file `setup.sh` itself writes on every
run — so the script and the button are two ways of doing one thing rather
than two mechanisms that can disagree. It is also strictly simpler than
the binary path in one way worth naming: there is no rollback, because
the health check runs against the pulled image *before* the ref file is
touched at all, so a failure leaves the deployment pointing exactly where
it already pointed.

`scripts/setup.sh` wires up the image path for the one deployment shape
it knows about, and the restart it names is a touch of
`$GRAIN_DATA_DIR/control/restart` rather than `sudo systemctl restart`:
see "The deployment is a container" above for that channel and why a
container needs one. `-upgrade-src-dir` is not passed at all any more:
with `-upgrade-image` set nothing builds, and the source `grantTools`
reads for the self-debug capability is the copy inside the image
(`cmd/grain/daemon.go`'s `sourceDir`), which cannot disagree with the
binary next to it.

`GRAIN_ENABLE_UI_UPGRADE` (default `1`) is the escape hatch for a
deployment shape that already has its own rollout mechanism and cannot
tolerate a second one racing it: set to `0`, `setup.sh` leaves the
upgrade flags off entirely, so the daemon starts with the feature
disabled, same as if none of this section existed. `terraform/gcp`'s own metadata-driven rollout
(`config-sync.sh`/`deploy.sh` — which watches the
`grain-deploy-generation` instance-metadata attribute Terraform writes,
and re-runs `deploy.sh`, and through it `setup.sh`, from there) sets
exactly that, since Terraform's own state is the record of what
`grain_ref` a GCP deployment is on, and letting an operator's UI click
upgrade it out from under a `terraform apply` (or the reverse) would let
the two silently disagree about what's actually running
(bwsalmon/agents#405).


## Slots are gone; a sandbox belongs to one run

A *slot* was one identifier doing five unrelated jobs: the concurrency
unit `dispatch.Cycle` drew from a fixed pool, the name a long-lived
sandbox was built under and reused across tasks, the identity the git
proxy authenticated, the number a kontur VM's `-ip`/`-port` were derived
from, and the row the sandbox-health pane keyed on. Only the first was
ever a real idea. The rest existed because that identifier happened to be
durable, and each one cost something:

- **Isolation had to be bolted on.** A slot's VM outlived every task
  dispatched onto it, so `runOne` deleted and rebuilt it after each run,
  `runDaemon` ran a reset pass over every slot at startup to cover the
  runs a crash interrupted, and `KonturSandboxes` remembered each slot's
  git credentials so the rebuild could reapply them.
- **`HostSandboxes` never got that at all.** Its directories were
  deliberately long-lived — resetting one between tasks was "the caller's
  job", and no caller did — so sequential tasks on one slot genuinely
  shared a filesystem.
- **A proxy token outlived the tasks that used it.** One token per slot,
  minted at startup, shared by every run that ever landed there.

Now `Sandboxes` is a lifecycle rather than a lookup: `Acquire` builds one
sandbox for one run, and the `Sandbox` it returns is `Release`d when that
run ends, success or failure alike. A task cannot inherit a filesystem
that no longer exists, so the recreate, the reset pass, the remembered
credentials, and the `recreatingSandboxes`/`shapedSandboxes` optional
interfaces are all gone with the problem they solved. What
`docs/design.md` lists as a non-goal for v1 — "isolating *sequential*
tasks on one sandbox from each other" — is here a property rather than
something knowingly given up.

**Concurrency is a count.** `Cycle` takes a limit and starts runs until
that many are live (two counts since "Merge capacity is its own number"
below; one, `max_concurrent`, when this was written). The DB-level backstop that used to be a
unique index on the slot each run claimed (bwsalmon/agents#434, catching
two overlapping cycles that both thought a slot was free) is now a count
inside `StartRun`'s own transaction, which rules that race out rather
than detecting it after the fact; the index that remains says a task has
at most one run in flight, which is what `task_state` already assumed.

**A run outlives the cycle that started it.** `reconcileDispatch` used
to wait for every run it dispatched, and `cmd/grain`'s reconcile loop
waits for a cycle before it ticks again — so one long run held the whole
controller. Nothing else was dispatched however much of the limit
was free, no pull request was synced, no schedule came due, until that
agent finished. A deployment configured for several concurrent runs only
ever reached that number when a single tick happened to find several
tasks ready at once; a task filed a second after a run started waited out
the whole run. `orchestrator.InFlight` is where the goroutines go
instead: `RunCycle` returns once the dispatch *decisions* are made, and
the next tick dispatches into whatever headroom is free then.

The limit is still a count in the store, not a count here —
`LiveRunCount`, re-checked inside `StartRun`'s transaction — which stays
accurate across ticks precisely because a run's row stays live until the
goroutine that outlived the cycle finishes it. What `InFlight` is for is
waiting: `drainInFlight` gives a cancelled run its chance to release its
sandbox before the process exits, and a test that dispatches
asynchronously needs to know when the work is done. A `Deps` with no
`InFlight` keeps the old shape, waiting for its own runs and joining
their errors — which is what every one-shot caller (a test, a single
cycle) wants, having no next tick to do the waiting for it.

Ticking while a run is live opened one window that could not exist
before. A run's row is finished (`FinishRun`, inside `RunDispatch`) a
moment before `runOne` has turned its result into the effects it implies
— the observation that says the task completed, the pull request it
opened — and in between `task_state` sees no live run and no completion,
so the task reads `queued` again. A tick landing there dispatched the
same task a second time. `dispatch.Busy` closes it: the process still
holding that result tells `Cycle` to pass over the task, without
spending capacity on it, exactly the way a task still backing off after
a failure is passed over.

**A sandbox is named after its run.** Nothing else is in a position to
name it — it is built for that run and destroyed with it — and a run ID
is already unique, already durable, and already what a log line or a
`konturctl vm list` most usefully shows. `task_run.sandbox` is that name;
`task_run.slot` is gone. Under the docker backend the whole VM name must
fit 11 bytes (netshim derives `tap-<name>` and `ctl-<name>` from it, and
Linux caps an interface name at 15), so `VMNameFor` checks that budget
and says what is spending it rather than letting `konturctl vm create`
refuse an interface name several layers down. A two-byte prefix leaves
nine, which covers five-digit task ids with double-digit attempts.

**The proxy token dies with the run.** It is minted as that run's sandbox
is prepared rather than once per slot at startup — the security property
`docs/data-model.md` predicted would fall out of a sandbox per task. That
required `gitproxy.SandboxTokens` to start re-reading its file when shown
a token it does not recognise: pinning the map at startup was correct
while every token was minted before the proxy started, and would now
reject every run's git. `cmd/grain/daemon_token_ordering_test.go` used to
pin the opposite guarantee and now pins this one.

Moving the mint also made `SandboxTokenStore` concurrent for the first
time. Every method on it is a read-modify-write of one JSON file, which
was safe while `runDaemon` minted a slot at a time in its own startup
preamble and is not while `reconcileDispatch` runs a goroutine per
dispatch: unguarded, two mints lose each other's tokens and publish
half-written JSON through a shared temp-file name, so a run ends up
holding a token that never reached the file and fails every git operation
it makes. It takes a mutex now, and writes through a uniquely-named temp
file. `SandboxTokens.reload` had a matching hazard on the read side — it
read the file and swapped the map in two critical sections, so a reload
that read *earlier* could install its map over one that read *later* and
answer "unknown" for a token that is on disk — and now does both under
one hold of the write lock.

And the file shrinks as well as grows: `Deps.RevokeSandboxToken` drops a
sandbox's entry once it is released. One token per slot was a fixed set
for a deployment's life; one per run is a new entry every dispatch, in a
file every mint rewrites whole. This is upkeep rather than
authorization — `Store.GitScope` already answers "no live run" for a
finished run's sandbox, so a stale entry authorizes nothing either way.

**`ReapOrphans` replaced the reset pass.** At startup no VM can belong to
this process, so any under the deployment's own prefix is a leftover from
one that died before it could release it — the same argument, at the same
moment, `RecoverOrphanedRuns` makes for the rows such a process leaves
live. It deletes them rather than rebuilding them, because they are meant
to have been deleted already.

**`HostSandboxes` sweeps too, and had to.** The same argument holds for a
run's directory, but only `KonturSandboxes` implemented `ReapOrphans` at
first, so nothing ever removed a directory whose process died before its
`Release` could run — and being killed mid-run is the ordinary case here,
not a rare one: `grain-daemon.service` stops its container with `docker
stop --time 30`, while a run's own unwinding is allowed minutes
(`cmd/grain`'s `shutdownDrain`), so every upgrade or restart that lands
on a run in flight leaves that run's whole checkout behind. Those
accumulate one per killed run until the filesystem `-sandbox-dir` sits on
is full, at which point `Acquire`'s `mkdir` fails with `ENOSPC` and
*every* task fails at setup, before its agent starts — a deployment that
is wedged permanently rather than one that recovers. Both backends
implement the sweep now, and `runDaemon` asks for it by interface
(`orphanReaper`) rather than by holding a concrete `KonturSandboxes`
alongside `Deps.Sandboxes`, so a restart is what reclaims the space.

**`KonturConfig.BaseIP`/`BasePort` became `IP`/`Port`,** passed verbatim
to every VM rather than offset by a slot number. Under the docker backend
— the only one this package builds VMs under — `internal/dockervm.Create`
gives every VM its own netns-holder container that the VM joins with
`--network container:`, so they share no bridge and cannot collide on an
address, and `Port` only ever reaches `NETSHIM_VMS` inside that
namespace. The derivation was guarding a collision that shape makes
impossible; it dates from the static-pod backend, where a pod's
containers genuinely did share a namespace. That reasoning is from
kontur's own source rather than from two VMs observed coming up on one
address, so it is worth confirming against a real NAT-mode host before
leaning on it. Flat mode, the default, ignores both.

**A run that cannot get a sandbox still has to be finished.**
`dispatch.Cycle` makes a run durable before anything builds a sandbox for
it, and `RunDispatch` — the only thing that finishes a run — is never
reached when setup fails. Left there, the row stays live forever:
`task_state` reads it as `running` so the task never returns to `queued`,
`LiveRunCount` keeps counting it so the deployment loses a unit of its
worker capacity, and `retryEligible` reads *finished* runs so the
backoff never retries. Nothing sweeps it — `MaxRunRuntime` is enforced
inside `RunDispatch`, `RecoverOrphanedRuns` is a startup pass — so it
lasts until someone restarts the daemon. That was survivable while a
slot's VM was built once at startup; with a VM boot on every dispatch's
setup path it is the ordinary way a run fails, so `runOne` finishes such
a run itself, outcome `setup-failed`, and dispatch's own backoff retries
the task.

Two smaller consequences of the same move. The first: **a VM's name stopped
being anyone's choice.** The budget got tighter without the flag that spends
it changing, since a name built from a run id needs more of
`maxVMNameLen`'s 11 bytes than one built from a slot number. The first
answer was `CheckNamePrefix`, refusing an outgrown
`-kontur-vm-name-prefix` at startup rather than letting every dispatch
discover it separately — which caught nothing, because the value it checks
was never in this repo's Go: `scripts/setup.sh` and `terraform/gcp`
both defaulted to `kontur-`, which fit while a VM was named `kontur-1`, and
the default deploy path (kontur sandboxing being on by default) therefore
refused to start at all.

The check was the wrong shape. 11 bytes minus a run id's nine leaves two,
and there is no useful choice to make inside two bytes — only a wrong one,
whose cost is a daemon that cannot build a single VM. So the name is
`orchestrator.VMNamePrefix`, a constant, and the flag that used to carry it
is `-kontur-sandboxes`, a bool that only opts in. Deployments that must not
reap each other's VMs get that from separate `-kontur-state-dir` values,
which is what `ReapOrphans` actually lists from.

`dispatch.RunID` gave a byte back at the same time: it reads
`<task>-<attempt>` rather than `<task>-r<attempt>`, the `r` having said
only what the field's position already said, while costing a decimal digit
of task id. The nine bytes now cover eight digits of task id and attempt
combined — `999999-99` fits exactly — and
`TestVMNameBudgetCoversRealisticRunIDs` pins where that ceiling actually
bites, since task ids only ever climb toward it.

The second: `Release` runs on a context detached from
cancellation, which is right, but now with a deadline: unbounded, a hung
`konturctl vm delete` would pin the dispatch goroutine and the unfinished
run row beneath it for the life of the process, which is the failure
detaching is meant to prevent. `Acquire`'s own cleanup needs that same
detachment for a sharper reason: `kontur.Delete` execs through
`exec.CommandContext`, so against an already-cancelled context
`deleteQuietly` did not merely fail — it never ran at all. Since `ctx` is
cancelled whenever the daemon is stopping *or* a task was closed mid-run,
the ordinary way an `Acquire` is interrupted was also the way it leaked a
VM, until the next startup's `ReapOrphans` got to it.

**What this costs.** A VM boot moves onto the critical path of every
task, where it used to be paid once at startup. `docs/data-model.md`
already names the mitigations — a golden image, and a warm spare booting
ahead of demand — and neither is in place yet. Worth measuring before
reaching for either: a warm spare is a pool of *VMs*, which is a much
smaller idea than a pool of assignments, and does not bring slots back.

The sandbox-health pane changed meaning with everything else: it reports
live sandboxes, so an idle deployment shows nothing rather than a table
of idle slots.

## Disk is the third dimension of a sandbox's shape

A sandbox VM's size had two knobs — `sandbox-cpus` and
`sandbox-memory-mb`, deployment-wide in Settings and overridable per task
— and no third. Its disk was whatever the guest image happened to be:
`konturctl` gives each VM a copy-on-write qcow2 overlay backed by that
image, and an overlay is created at exactly the image's own virtual size,
which `scripts/kontur/build-guest.sh` packs to the rootfs plus 20%
headroom. That is a few hundred megabytes of slack for every run, and a
build-heavy checkout spends it: the run fails part way through with a
disk-full error, on a VM that had CPUs and memory to spare.

`sandbox-disk-gb` is that knob, everywhere the other two already are:
`model.Config.SandboxDiskGB` and `model.Task.SandboxDiskGB`, the Sandbox
tab in Settings and the shape override under New task -> Advanced
options, `orchestrator.Shape.DiskGB` resolved per dimension against the
deployment default, and `konturctl vm create -disk-size-gb` at the one
moment a sandbox's size is decided. Zero keeps meaning "unset" — the flag
is left off the create entirely, so a deployment that never sets one
passes exactly the arguments it passed before.

Two things about it are genuinely unlike CPUs and memory, and both are
visible in the code:

- **There is no default to show.** `ui.Settings` reports
  `sandboxCpusDefault`/`sandboxMemoryMbDefault` so an unset box can show
  what is really in effect rather than a misleading literal 0. Disk has
  no such constant: an unset disk is however large *this deployment's*
  guest image is, which is a property of an image somebody built, not a
  number this build can name. The field has no placeholder, and says so
  in words instead.
- **A bigger disk is not by itself more space.** The image's filesystem
  ends where it ended; the extra is unallocated until something grows it.
  `scripts/kontur/guest-setup.sh` installs a `grain-growfs` unit that
  runs `resize2fs /dev/vda` on each boot, which is a no-op on a VM whose
  disk was not enlarged and a one-line grow on one whose was.

**It needs a `konturctl` that takes `-disk-size-gb`.** The vendored
snapshot under `third_party/kontur` does not: `staticpod.VMSpec` has no
disk-size field, and `writeQcow2Overlay` — which already takes the
virtual size as an argument — is called with the source image's size
unconditionally. Passing the flag against a `konturctl` without it fails
the create, which is why zero omits it rather than sending an explicit
size: a deployment that has not set one is unaffected either way, and a
deployment that sets one has said out loud that it expects the flag to
work. Landing that flag on `bwsalmon/kontur`'s `main` and re-vendoring is
the other half of this, and belongs there rather than as a local patch
here — see `third_party/kontur/VENDORED.md`.

### Monitoring it

Setting a size is half the ask; the other half is seeing whether it was
the right one. The sandbox-health pane now reports disk alongside load
average and memory, from both ends:

- **Per sandbox.** `KonturSandboxes.sandboxHealth` already pulled
  `/proc/loadavg` and `/proc/meminfo` out of the guest in one command
  over the same transport a run's own tools use; it now asks for
  `df -Pk /` in that same command rather than paying a second guest login
  out of the five-second health budget. `-P` is what makes the parse
  reliable — one line per filesystem, six columns — and the row is found
  by shape ("the line whose last field is `/`") rather than by position,
  so the `/proc` lines sharing the stream cannot be mistaken for it.
- **For the host.** `pkg/sysstat` gains `DiskUsage`, one `statfs` call,
  which `hostStats` asks about the daemon's own `-data-dir` rather than
  about `/`: the daemon runs in a container whose root filesystem is an
  image layer nobody's runs fill, while the data directory is on the host
  volume that holds the store *and* every VM's disk overlay. That is the
  disk a deployment actually runs out of.

Both report 0/0 when there is no reading to be had, which the pane shows
as a dash and the trend charts skip rather than plotting as an empty
disk — the same "unavailable is not zero" treatment memory already got.

## Measuring throughput and latency

The previous section ends with "worth measuring before reaching for
either," and grain could not. Every moment needed to answer *how much is
this deployment getting done, and where does a task's day actually go?*
has been in the store since tasks became rows — filed, approved,
dispatched, finished, completed, closed — and nothing ever read them
together. `pkg/metrics` does, `GET /api/metrics` serves it, and `grain
metrics` prints it:

```console
$ grain metrics -window 7d
window: 2026-08-27T00:00:00Z -> 2026-09-03T00:00:00Z (168h0m0s)

throughput
  tasks filed                  42  (6.0/day)
  tasks completed              38  (5.4/day)
  tasks closed                  3
  attempts started             61
  attempts finished            60  (8.6/day)
  attempt outcomes         succeeded=45 failed=14 cancelled=1
  attempts per completion    1.58

capacity
  mean concurrent runs       0.42 of 3  (14% of the limit)
  live now                      2

latency (stages that ended inside the window)
  stage                                        n        p50        p90        max
  filed -> approved                           12      4m12s     1h2m0s    3h10m0s
  approved -> attempt started                 38        31s      2m10s       9m0s
  attempt started -> agent's first turn       57      3m20s      6m41s      11m2s
  agent's first turn -> attempt finished      57      9m11s     21m30s      48m0s
  one whole attempt                           60     12m48s     26m10s      52m0s
  attempt finished -> next attempt started     8       2m0s       4m0s       6m0s
  first attempt started -> completed          38      9m10s      22m0s      48m0s
  filed -> completed                          38      12m0s      50m0s    3h10m0s

backlog (right now, not over the window)
  awaiting_reply=1  proposed=1  queued=4  running=2
  oldest queued: task 51, waiting 2h14m0s
```

**Nothing is stored, and nothing is counted on a hot path.** A report is
derived from the rows every time it is asked for, the same way
`task_state` is a view rather than a column
(`docs/data-model.md`: "anything derivable is derived, never stored").
There is no counter to increment, nothing to reset, and no way for a
metric to drift from the task it describes — retry a task, close it,
edit it, and every report from then on says what the record now says.
The cost is a full scan of `task` and `task_run` per report, which is
what a single-operator deployment can afford and what a much larger one
would have to revisit.

**One moment had to start being recorded: `task_run.agent_started_at`.**
A run's `started_at` is stamped by dispatch, before any sandbox exists
(`Store.SetRunSandbox`), so `finished_at - started_at` is a VM boot, a
clone, a capability mint *and* the agent's own work, fused into one
number that cannot answer the question the previous section asks.
`RunDispatch` now records the moment it hands the run to
`agent.Framework.Run`, which splits that in two: **attempt started ->
agent's first turn** is the setup a golden image or a warm spare would
cut, and **agent's first turn -> attempt finished** is what the agent
framework spent, which grain does not control. Writing it can never cost
a run — a failure there is logged and the dispatch proceeds — and it is a
nullable column added by an ordinary `ensure*Column` migration, so an
existing store keeps working and its older runs simply report no split.

**A window bounds measurements, not rows.** A sample belongs to a window
when the moment it *ended* falls inside it, so a task filed last month
and completed this morning contributes its whole lead time. That makes
the report answer "what did this deployment deliver during these dates"
rather than "what happened entirely within them," which nothing ever
asks.

**A missing moment is skipped, never guessed**, which is why every stage
carries its own `n` and two stages of one report legitimately disagree
about how many samples they have. A run that failed in setup never
reached an agent, so it has no setup or agent sample — but it was still
an attempt, and still took time, so it is in `one whole attempt`. A task
a human filed directly was approved in the instant it was filed, so it is
left out of `filed -> approved` rather than counted as a zero that would
drag the percentile of the proposals that really did wait.

**The stages do not sum to the lead time,** and are not meant to. A task
can sit in `awaiting_reply` for a day, back off between attempts, or wait
on a dependency, and none of those is a stage anything records the start
of. Each stage is measured on its own; the lead time is what somebody who
filed a task actually waited, and the rest are answers to why it is what
it is.

**Throughput alone cannot say whether a deployment is fast enough,** so
the report carries the two gauges it has to be read against: the backlog
(by `task_state`'s own vocabulary, since "not finished" covers queued,
blocked, awaiting a reply and failed, which are four different problems —
and counting only the unfinished states, since every task ever completed
is a census rather than a queue), and occupancy as a fraction of
`max_concurrent`. Idle capacity next to a
deep queue is a scheduling problem; saturated capacity next to a deep
queue is a capacity one. They are the first two numbers any optimization
here should have to move.

**The UI reads the same report,** as the Metrics tab of the Debugging
pane — alongside Logs and Sandbox health, since all three are read-only
views of how the deployment is behaving rather than knobs on it. The
window picker sends the same strings `-window` takes, the throughput
buckets are drawn as sparklines, and the two presentation rules above
are enforced rather than described: the latency stages are a table of
independent distributions and never a stacked bar, which would draw a
claim about them adding up that the numbers do not make, and the backlog
is a section of its own headed "right now, not over the window". Each
stage's `n` sits beside its percentiles, and a percentile with too few
samples behind it to mean what its name says — fewer than 10 for a p90,
100 for a p99 — is dimmed and footnoted rather than shown as if it were
one. Unlike the panels beside it there is no poll: a report costs a full
scan every time it is asked for, so it loads once and reloads when the
window changes or Refresh is clicked.

What this still does not have is a history of its own: because nothing
is stored, a report can only ever be computed from rows that still
exist, so a task deleted from the store takes its own past contribution
with it.

## Measuring the daemon's own tick

The section above ends with the pair worth reading together — idle
capacity next to a deep queue is a scheduling problem — and leaves the
next question unanswered. `queue_wait` (approved -> the first attempt
starting) is the one latency stage that is grain's own scheduling rather
than anyone's work, and two entirely different causes produce the same
number:

1. the deployment was at `max_concurrent` and the task genuinely waited
   for headroom, which `runs.utilization` near 1.0 already shows; or
2. there was headroom the whole time, and the task waited on the tick —
   `-poll-interval`, plus however long a `RunCycle` pass itself takes
   before it reaches the dispatch decision.

Nothing measured the second, so a tick that had quietly grown to minutes
under a large store looked exactly like a busy deployment. `GET
/api/metrics` now carries a `cycles` section beside `runs`, and `grain
metrics` prints it right after capacity, so both causes are on screen
before the `queue_wait` row is:

```console
reconcile tick (this daemon, since it started -- not stored, so not over the window)
  ticks measured              720  (of 5304 run; older ones forgotten)
                                    p50        p90        max
  tick duration                    83.4ms      1.42s      4.9s
  tick to tick                        30s        30s     34.9s
  cycle start -> dispatch          21.1ms     34.6ms    212.7ms
  scheduling floor: 30.02s  (tick-to-tick p50 + dispatch p50 -- the queue wait a task pays
    for grain's own scheduling with no contention at all)

  reconciler         wait p50        p50        p90   failed
  schedule             1.1ms      2.4ms      8.9ms        0
  dispatch            21.1ms     11.2ms     48.0ms        0
  sync                32.4ms     47.1ms      1.31s        3
```

**The number the section builds to is the scheduling floor.** Ticks do
not overlap (`cmd/grain`'s `reconcile` waits for one to return before the
next interval starts), so tick-to-tick is the loop's real period, which
is the `-poll-interval` only while a tick is fast compared to it. Adding
the dispatch wait to it gives what a task pays for grain's own scheduling
with no contention involved at all. A `queue_wait` p50 near that floor is
the tick; a `queue_wait` p50 far above it is the deployment being full.
They are opposite problems with opposite fixes — more concurrency for one,
a faster or better-ordered cycle for the other — and until now the report
could not tell them apart.

**A per-reconciler breakdown, not one number for the tick.** The
reconcilers run in order ("Reconcilers, not a pipeline"), so a
pull-request sync that has grown to a minute is a minute every decision
behind it did not get to spend, and a single tick duration cannot say
which one grew. Each reconciler reports how far into the cycle it
started, how long it took, and how many of those cycles it ended in an
error — that last one because a reconciler that is fast *because* it
fails immediately is not a fast reconciler, and a duration alone cannot
tell the two apart.

**The dispatch wait is recorded by the cycle, not picked out of the list
by name.** `pkg/orchestrator` is the package that knows which reconciler
is the decision a queued task is actually waiting for — it names them —
so it reports that offset as its own field (`CycleTiming.DispatchWait`)
rather than leaving every consumer to hardcode the string `"dispatch"`.

### Why this one measurement is in memory

Everything else in `pkg/metrics` is derived from rows that already exist,
and holds to `docs/data-model.md`'s "anything derivable is derived, never
stored." A tick is the one thing that leaves no row at all: it reads the
store, decides, and returns. That left two options.

A **row per cycle** would be durable across restarts and queryable over
any window. It costs a new table, a write on every single tick forever
(2,880 a day at the default 30s `-poll-interval`) and a growth curve
nothing prunes — to measure something whose whole purpose is to say
whether *this process, right now* is dispatching promptly. It would also
make the measurement change what it measures: a tick that writes a row is
a tick with a store write in it.

A **ring in the process** — `orchestrator.CycleTimes`, 720 cycles, six
hours at the default interval — costs bounded memory, no schema, no
write, and stores nothing, which keeps the doctrine intact rather than
carving an exception into it. That is what this is. The honest cost is
that it is lost on restart, and the report says so rather than hiding it:
the section is scoped to "this daemon, since it started", carries
`observed` alongside `n` so a truncated ring is visible, and reports
`first`/`last` as the span it really covers, which is however long the
ring is rather than however long a window was asked for. Tick history
belongs to the process that produced it anyway — a daemon that has just
restarted has a fresh tick, with none of the accumulated store the slow
tick this exists to catch would have been slow because of.

The wiring follows the same seam the sandbox-health pane already uses:
`pkg/ui` does not import `pkg/orchestrator`, so `cmd/grain`'s own
`cycleTimesAdapter` is the one place both types are in scope, and the
ring is package-level in `cmd/grain` because the UI/API server starts
before the reconcile loop that writes into it (the same ordering
`reconcilerDown` and `livePullRequests` already straddle). A UI with no
reconcile loop behind it reports `"enabled": false` rather than a tick of
zero: "nobody measured it" and "the tick costs nothing" are opposite
answers, and only one of them is ever true.

`tests/e2e/loadtest_test.go` had measured `RunCycle` tick duration with
instrumentation of its own since it was written ("RunCycle tick duration:
n=… p50=… p95=…"), and no longer does. The harness passes a `CycleTimes`
into the `Deps` it ticks and reports out of that instead, so the load
test and a deployment read one measurement taken by one piece of code,
rather than two that agree only for as long as nobody changes either. It
is also the more useful of the two: the harness now reports the dispatch
wait and the per-reconciler breakdown alongside the tick, and fails on
what a stopwatch around `RunCycle` could not see at all — a cycle that did
not run every reconciler, and a p95 dispatch wait past a third of the
per-tick budget, which is a queued task waiting on the tick itself rather
than on a deployment that was full. A tick growing under a large store is
the thing a load test is best placed to catch, and that is the shape it
now catches it in.

## Merge capacity is its own number

Concurrency was one number: `max_concurrent`, the count of runs
`dispatch.Cycle` would let be in flight. Every kind of run drew on it
equally, which put the two least alike kinds in direct competition. Most
runs are new work, at the start of its life. A few are the merge queue's
own fix tasks (`Origin.Reason == ReasonFix`, `fileFixTask`), filed
against a pull request that will not land — the last step of work that is
already committed, pushed and reviewed. A saturated deployment starved
exactly the second kind, and starving it is expensive twice over: the
branch a fix targets keeps moving while the fix waits, so a repair
delayed long enough has to be filed again, and the queue behind it waits
too.

`model.Limits` is that one number split in two — `MaxWorkers` and
`MaxMergers`, in the store as `grain_config.max_workers`/`max_mergers`,
on the daemon as `-max-workers`/`-max-mergers`, in Settings as "Max
worker agents" and "Max merge agents". The rule they make is
deliberately asymmetric:

- no more than `MaxWorkers` runs of ordinary work are ever live;
- no more than `MaxWorkers + MaxMergers` runs are ever live at all.

A merger is bounded only by the second, so it may take a worker's free
slot; a worker is bounded by both, so it can never take a merger's. With
3 workers and 2 mergers a deployment can be running five mergers, or
three workers and two mergers, but never four workers and never six of
anything. Capacity kept back for finishing work is reachable by work
that is nearly finished, and by nothing else.

The split lands in the two places the old count did, and nowhere new.
`Limits.Admits` is the whole rule, written once: `dispatch.Cycle` asks it
before spending capacity it can see (`Store.LiveRunCounts`, the live
count split the same way, and `Store.ReadyMergers` to say which ready
task is which kind), and `Store.StartRun` asks it again inside the
transaction that records the run, which is where the limit is actually
enforced — two overlapping cycles cannot both spend the last slot. A
candidate whose own half is full is passed over rather than ending the
cycle, the same skip a task still backing off gets, so a queue of
ordinary work at the head of the backlog no longer hides the fix task
behind it from the capacity kept for it.

`-max-concurrent` still parses, as the former spelling of
`-max-workers`: a deployment's unit file is written once by
`scripts/setup.sh` and the Upgrade button then replaces only the binary,
so dropping the old flag would stop the daemon at the moment nobody is
watching. The stored column is migrated rather than reinterpreted
(`ensureConfigWorkerMergerColumns`): `max_workers` is backfilled from
`max_concurrent`, which is then dropped, and `max_mergers` starts at
`model.DefaultMaxMergers` — one — so an upgraded deployment keeps the
ordinary concurrency it had and gains a single slot the merge queue
cannot be shut out of. Setting it to 0 puts a deployment back exactly
where it was: fix tasks contending for worker capacity like anything
else.
