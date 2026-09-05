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
                escape-hatch tools (ask_question, request_secret,
                comment_on_issue,
                propose_task, add_review_comment) -- plus three tools whose
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
                rebuild its own sandbox" below. And update_status
                (NewStatusTools): a run can put one short phrase on its
                own task's row -- "waiting for CI", "running the test
                suite" -- so a task that has read 'running' for half an
                hour says what that half hour is going on, over the same
                hop again (POST /api/tasks/{id}/activity) and for the
                plainest of reasons: the row lives in the daemon's store.
                It is the only one of the three that changes nothing --
                grain shows the phrase and never reads it back -- see "A
                run can say what it is doing" below. NewSandboxTools runs
                those four locally, confined to a directory; NewSSHSandboxTools
                (DockerExecRunner) runs the same four tools inside a
                kontur-managed sandbox VM's guest instead, by exec'ing
                into that VM's own container -- see "Reaching a sandbox
                guest without a route into it" below.
                NewPullRequestTools adds pull_request_status and
                wait_for_checks: the two tools here that really read
                GitHub, from the controller, so a run can see CI's
                verdict on the commits it pushed and repair a red build
                inside its own turn budget. The first answers what CI
                says now; the second blocks until CI has a verdict at
                all -- one call instead of a poll loop paid for a turn
                at a time -- see "Letting a run watch its own CI" and
                "Waiting for CI instead of polling it" below
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
                the mcp_config.json naming its own "mcpserver" server (a
                per-user `agy mcp add` registration cannot express a
                per-run sandbox binding); and there is no --max-turns, so
                RunConfig.MaxTurns is enforced here, by counting completed
                agent_response steps on the live stream and cancelling the
                subprocess. agy's own three-minute cap on a single MCP
                tool call is raised past wait_for_checks' longest wait in
                that same file, since it has no MCP_TOOL_TIMEOUT to raise
pkg/agent/claude/  Framework via the real `claude` CLI, run as a
                subprocess on the controller (bwsalmon/agents#255) --
                this points --mcp-config at this same grain binary's own
                "mcpserver" subcommand (cmd/grain/mcpserver.go), the same
                way v1's dispatch.py pointed it at
                `python3 -m grain.automation.mcp_server`, and parses the
                resulting --output-format stream-json transcript back into
                an agent.Result
pkg/agent/codex/  Framework via OpenAI's `codex` CLI, run as a subprocess
                on the controller. Shaped by the same two gaps agy has --
                no --mcp-config (so each run gets a private CODEX_HOME
                holding just the config.toml naming its own "mcpserver"
                server) and no --max-turns (so the cap is counted here,
                off the live stream) -- plus a third: no --allowedTools to
                empty its native roster with, so the per-run config denies
                that roster anything worth having instead
                (sandbox_mode read-only, approvals never, code mode off)
pkg/capability/geminikey/  a MINT model.CapabilityProvider: mints, places
                and revokes a Gemini API key, direct against the API Keys
                API, plus Reap, the hourly project-wide sweep that deletes
                grain-prefixed keys older than 24h whether or not a Lease
                survived to name them
pkg/capability/gcpkey/  the gcp-key capability: a real MINT
                model.CapabilityProvider that mints/revokes a per-task GCP
                service-account key against the IAM API directly
                (google.golang.org/api/iam/v1, no gcloud subprocess), plus
                Reap, a standalone safety net that deletes anything GCP
                itself reports as older than 24h regardless of whether a
                Lease survived to say so
pkg/secrets/    a model.CredentialResolver backed by one encrypted file
                inside the state repository (staterepo, below), sealed to
                a public key whose private half lives outside that
                repository under <data-dir>/secrets and is the operator's
                to manage. The separation bwsalmon/agents#366 asked for
                ("put secrets in a separate db, config and tasks in a
                common db") is stronger this way, not weaker: cloning the
                repository gets everything grain knows and nothing it can
                authenticate as. X25519 + HKDF-SHA256 + AES-256-GCM out of
                the standard library, no dependency added to encrypt one
                file
pkg/staterepo/  grain's database as a git repository: every table
                exported to tables/<name>.json, rows sorted by primary key
                and columns in declared order, so an unchanged database
                produces byte-identical files and a settings change is a
                diff an agent can propose. On two clocks: everything
                anybody reads on every 30s sync, and the four tables grain
                writes to itself on every reconcile cycle (tier.go) once
                an hour, which is what stops a busy day from costing 2,880
                commits and gigabytes of .git -- see "Two clocks, so the
                repository grows with the data and not with time" below.
                Import is the other direction
                and is a wholesale replacement -- that is how a merged
                pull request, including one that deletes a row, becomes
                the running configuration. Load imports the whole of it
                at startup; Apply imports the settings tables of it into
                a daemon that is already running, so a merged change
                takes effect on the next tick rather than on the next
                restart, without replacing the task and run rows a live
                run holds ids from. The remote is optional by
                construction: no Remote is `git init` under the data
                directory, which is what a local install with no GitHub
                account gets
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
                It also files the review a finished task declares
                (SyncReviews, Task.ReviewTemplateID) and holds that
                task's own merge until the review has landed -- see "A
                review, before the merge" below.
                RunCycle runs the halves as independent reconcilers
                rather than one pipeline -- see "Reconcilers, not a
                pipeline" below. It also times itself
                (orchestrator.CycleTimes): a bounded in-memory ring of how
                long recent ticks took and how far into each one the
                dispatch decision was reached, which is the one thing a
                deployment measures about itself rather than derives from
                a row -- see "Measuring the daemon's own tick" below. And
                it stops dispatching entirely while the agent's own
                provider has refused for want of budget
                (orchestrator.Pause) -- see "Pausing when the agent runs
                out of budget" below
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
pkg/version/    which build of grain this is, read back out of the
                binary's own `go build -buildvcs` stamp: the commit, its
                timestamp, and whether the tree was dirty. Nothing to
                bump by hand and nothing for a deployment to be told --
                the same reasoning cmd/grain/sandboximage.go gives for
                stamping its sandbox tag in at build time. pkg/ui puts it
                on GET /api/config and the sidebar's footer prints it, so
                "is this deployment running the change I just merged?"
                is answered by the page in front of you rather than by an
                image tag that describes what was deployed
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
about testing this module ships anywhere. `tests.yml`'s `go-test` job
runs that same `go test -race ./...`, character for character — for a
long time it ran a plain `go test ./...` instead, so the detector never
saw a commit that a developer had not happened to run `make test` over,
which for a daemon of this shape (a reconcile loop, a goroutine per
dispatch, an addendum poller each, a `ForbiddenSet` swapped under a
serving proxy) is the coverage worth having. `tests/deploy` compares the
two commands now, so neither file can be changed on its own.

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
  `request_secret` (grain/task-230) is the one thing a run can ask for
  that must *not* come back as a comment: it relays the request and parks
  the task exactly as `ask_question` does, but records the credential's
  name on `Observation.PendingSecret`, and the task pane answers it with
  a write-only box whose value goes to `PUT /api/tasks/{id}/secret` and
  straight into the encrypted secret store. A reply is conversation, and
  conversation is the next run's prompt; this way the run gets the *use*
  of a credential on its next attempt and never the material.
  `propose_task` files a real `model.Task` with no `Approval`, so
  `proposeTaskTool`'s "a human must accept it first" contract is enforced
  by the state machine rather than by withholding a label, and
  `model.LinkProposedBy` records which task proposed it — something the
  issue version had no way to say. It joins the backlog where a task a
  human files joins it, `Store.OrderKeyForNewTask` at whichever end
  `model.Config.NewestFirst` names: by default the end of it, behind
  everything already queued, rather than at `OrderKey`'s zero value —
  which is not "no position" but a position ahead of every task filed
  since the backlog started.
- The merge queue's own two voices moved too: it asks for a repair with a
  store comment instead of an issue (`fileFixTask` then, `requeueForRepair`
  now), and both it and `escalateToUser` comment through
  `Store.AddComment` as the `merge-queue` principal, so a human reading a
  task's conversation can tell the queue's remarks from a relayed
  agent's.
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

The `mcp.NewMockTools` escape hatches (`ask_question`, `request_secret`,
`comment_on_issue`,
`propose_task`, `add_review_comment`) a run's own MCP server wires
internally are still discarded rather than acted on *while a run is
live* — `ProcessResult` only ever inspects `agent.Result.ToolCalls`
after a run finishes, and relays all five for real at that point (see the
package tree entry above); giving `Framework.Run` (or its caller) a way
to inject a live sink instead is still open. `add_review_comment` is
relayed too now that a review dispatch exists to attach one to: on a
review task's run its calls become a draft review on the pull request
under review, repeated on that task's own conversation and taking it off
automatic merge until a human has read them; on any other run they are
relayed into that run's own task conversation. What the *agent* is told about all five is
that relay rather than that sink: the tools' descriptions and
confirmations used to answer every production run with "mocked — no
GitHub comment was posted", and describe v1's issue, trigger label and
issue-per-proposal, none of which has been true since tasks became rows
(docs/agent-ergonomics.md, findings 1 and 2). They now say where the
words really land — the task's own conversation, when the run
finishes — and `add_review_comment` names both of its own destinations,
the pull request under review and, failing that, this task's
conversation.
`pkg/mcp/mock_tools_test.go` is what holds them to it. Neither sandbox stand-in carries any real
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
dropped. `add_review_comment` calls from a run are read off the same seam
(`agent.Result.ToolCalls`, where `ProcessResult` already finds
`ask_question`/`comment_on_issue`/`propose_task`) and turned into a real
`CreateReview` call whenever the run has a pull request behind it — which
a review task's does, through the task it reviews
(`relayReviewFeedback`). `propose_task`'s `depends_on` is
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

Repairing a PR that goes red is built now (bwsalmon/agents#283):
`SyncPullRequests` runs a merge queue, one per target repo, over every
task that asked for `/auto-merge` and still has a PR open. Only the
queue's head — whichever of a repo's waiting tasks sits first in the
backlog — is ever acted on in a
cycle; a repair started for the second task while the first is still
being repaired would likely need starting over the moment the first
merges and changes what the second is based against, so everything behind
the head just waits. Nothing merges while CI is still
running: a check run GitHub has not finished reads `PrHealth.PENDING`
(`healthFrom`), which is neither clean nor failing, so the head simply
holds its place and the next cycle asks again — a queue that merged on
"no failure reported yet" would land changes before their tests had said
anything about them. Pending outranks failing on purpose, so a red job
alongside a still-running one waits too: the queue asks for exactly one
automatic repair per pull request, and it is worth a cycle to ask for
that one against CI's whole verdict rather than against whichever job
went red first. Nor does it merge before CI has said *anything*: an empty check
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
would otherwise walk through — a human's own "push a fix by hand", a
repair run pushing to the branch it was sent to fix, a redispatched task
pushing again — and costs one cycle when it happens: the task keeps its queue
position and the commit that landed is judged next cycle on its own CI.
Nor does it wait for CI that is never coming: a head that
has read `PENDING` for longer than `defaultCheckStallDeadline` (two
hours, timed per head commit over one unbroken run of pending reads) is
given up on — a comment naming the checks that never finished,
`Observation.MergeQueueBlockedAt` set, the queue moved on — since a
workflow waiting on an approval nobody gives, or a provider that posted
"queued" and went away, would otherwise hold its repo's whole queue for
the life of the deployment with nothing said to anyone. No repair is
asked for on that one: nothing has failed, and a check that never
finishes is usually waiting on something outside the pull request, so
there may be nothing in it to repair. A conflicted or failing head is not taken at
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
base, so the failure is genuine and the repair is asked for as it always
was; `409` means it genuinely conflicts, which is the case a repair is
really for, asked for immediately and now naming the conflict the queue
watched GitHub refuse rather than one inferred from a `Mergeable` flag.
That is one API call in place of a full agent run for what has been the
majority of this deployment's automatic fixes — every one of them
resolved by a plain `git merge origin/main` an agent booted a sandbox to
type. It happens once per pull request
(`Observation.MergeQueueRefreshedAt`, persisted for the same reason the
CI clocks are not: losing it would cost a repeated write to GitHub rather
than another window of waiting), only for the queue head, and never for a
branch the queue is not steering or one it has given up on. It is a
merge, never a rebase: nothing force-pushes a branch an agent may still
hold a clone of.

A conflicted or failing head that survives all that is **sent back to an
agent on its own branch** (grain/task-271). The queue comments on the
task saying what is broken, which branch to push to, and what CI printed
if a job went red; clears `Observation.CompletedAt`, which is what makes
`StateOf` read the task `queued` again; and stamps
`Observation.MergeQueueRepairAt`. The next `dispatch.Cycle` picks it up
through the ordinary path, the run continues the branch the pull request
is already open from, and GitHub re-runs that one pull request's checks
once. Until grain/task-271 the queue filed a *separate* task instead —
pre-approved, `/base` the broken branch, `/auto-merge true`, stacked and
merged back once green, which is `core.py`'s own `_suggest_fix` trick
minus the human approval step bwsalmon/agents#283 asked to remove. It
worked, and it cost two full rounds of CI for one resolution: the fix
branch's own before it could merge, and the head branch's afterward, with
the queue waiting out both and a second pull request left behind. A
conflict resolved on the branch that has the conflict needs neither.
`LinkFixTask` is still defined and such a task still merges if a database
holds one in flight; nothing writes a new one.

`MergeQueueRepairAt` is the whole record of a repair, and four things
read it: the queue waits on it rather than on another task's state, it
will not merge a branch an agent is still pushing to, the store spends
the capacity `Limits.Mergers` keeps back on the repair run (`mergerTaskSQL`,
which used to be the `ReasonFix` column alone), and never clearing it is
what holds the deployment to one automatic repair per pull request. If
the repair finishes and the PR is still broken, `SyncPullRequests` gives
up automatically rather than asking again: it comments explaining why,
sets `Observation.MergeQueueBlockedAt`, and the queue moves on to the
next task in that repo — a blocked task still merges the moment a human's
own push makes it clean, it just stops being anyone's queue head, so it
can no longer hold up what's behind it. It judges that a cycle *after*
the one the repair completed on, since dispatch runs before sync inside a
tick and the verdict read on that tick describes the branch as it was
before the repair pushed to it.

The task goes visibly back to working while this happens, which is what
a person watching the queue should see — so the frontend says which kind
of work it is: `ui.Task.Repairing` carries it, a repairing row's grain
mark animates in green rather than the accent's gold, and its badge reads
"Repairing" instead of "Running". A row that has gone back to running has
not gone back to the beginning.

No new record was needed for the
queue itself: `queueOrder` derives the whole queue from `Task.AutoMerge`,
`Origin.Reason` and `MergeQueueBlockedAt` fresh every cycle, and
`queueHeads` takes each repo's first entry from it — the same "derive it,
don't store it" discipline `TaskState` already holds to.

What the queue does write down is where it is. Every cycle,
`showQueueAtFrontOfBacklog` moves the tasks waiting to land to the front
of the backlog in the order they will land (`Store.MoveToFrontOfBacklog`)
— so a task list answers "what is grain about to finish, and in what
order" without anyone opening a task, and so a repair, being the head
task itself going back to work, is dispatched ahead of new work without
anything having to file it there. It is the same order in both
directions: `queueOrder` reads position back off the backlog rather than
comparing `Task.CreatedAt` behind everyone's back, so dragging one
waiting pull request above another really does change which merges first,
and `Store.Ready` needs no carve-out for the merge queue's own work any
more — being at the head of the list is what dispatches it first, which
is a thing a human can see and, if they disagree, undo.

That head is now the top of the list rather than its far end
(grain/task-201). `ui.Client.ListTasks` used to hand a UI or CLI the
*reverse* of the store's order unless `model.Config.NewestFirst` was set,
a newest-first display inherited from when a task's position was just its
age — so the paragraph above was only true if you read the list bottom
upwards, and the tasks about to merge sat furthest from where anyone
looks first. The flip is gone: a list reads top-to-bottom in the order
grain will work through it, merges at the very top, and `NewestFirst` now
only decides which end of that list a newly filed task joins (the bottom
by default, behind everything already queued; the top when it is on).

And that end is now chosen where the task is filed, not in Settings
(grain/task-202). The new-task form carries an "Add to backlog" picker —
front, so it runs next, or end, behind everything already queued — and
`grain create -position front|end` is the same choice from a shell;
`ui.CreateTaskRequest.AtFront` carries it, and a request that names an
end wins over whatever is stored. Naming one also *stores* it
(`Store.SetNewestFirst`, which writes `grain_config.newest_first` alone
rather than replacing the row the way a settings save does), so the next
task filed with no opinion joins the same end and the form opens on the
choice the last one made (`GET /api/config`'s `newestFirst`). That is the
whole of the remembering: `NewestFirst` was always "where new work joins
the backlog", and the only thing that changed is that filing a run of
urgent work at the front no longer means opening Settings first and
remembering to put it back. A filing that names no end changes nothing —
a schedule, a proposal or a script cannot quietly reset what a human
picked — and an interactive session is unaffected either way, since it
dispatches ahead of the backlog regardless of what any of this says.

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
correct, per "Two things the port corrected" above. `Reap` (and
`DeleteExpired` beneath it) is the "clean up after 24 hours if leaked"
safety net, mirroring `delete_expired_keys`.

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
the answer, not grain's store. `geminikey.Capability.Reap` plays the same
backstop role for Gemini keys, and is a `model.Reaper` too as of
grain/task-140 — it was a free function (`DeleteExpired`, still there for
a caller-chosen cutoff) that no binary called until then, which meant a
Gemini key minted for a run whose controller died between the mint and
the store write was never deleted by anything: `revokeAll` covers only
the leases grain still has a record of, and a lost record is precisely
what the backstop exists for.

Its one asymmetry with `gcpkey.Provider.Reap` is worth an operator
knowing: an API key carries no service account of its own for a
`ListKeys` call to scope to, so the sweep is **project-wide** and the
`grain-` display-name prefix is all that separates grain's keys from
anyone else's. Two grain deployments minting into one GCP project reap
each other's *leaked* keys — never each other's live ones (nothing under
24 hours old is touched) and never either daemon's own operating key
(`geminikey.OperatingKeyDisplayName`, exempted by exact name). Give each
deployment its own GCP project if that matters, the same way deployments
that must not reap each other's VMs get separate `-kontur-state-dir`
values.

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
`Revoke` once the run finishes. `Reap` is called too, now: `cmd/grain`'s
`reapCapabilities` sweeps every registered provider implementing
`model.Reaper` once an hour from the same reconcile loop, which is what
makes "clean up after 24 hours if leaked" hold within roughly that bound
rather than "eventually" — a standalone sweep independent of any one
dispatch, matching `gcp_keys.py`'s own `delete_expired_keys` cron job,
just on grain's own timer rather than cron's.

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
`ask_question`/`request_secret`/`comment_on_issue`/`propose_task`/
`add_review_comment`
calls: a run's own MCP server still wires those to a `mcp.MockSink` it
builds and discards internally on every call, so nothing happens at the
moment the agent makes one. `ProcessResult` only sees them after the
fact, through the `agent.Result` `Run` returns, not while the run is
live — and then relays a question, a secret request, a closing comment
and a proposal into the store for real, and a review comment onto the
pull request under review (or, failing one, into the task's own
conversation). Giving
`Framework.Run` (or its caller) a way to inject a real sink, so the
effect could happen while the run is still going, is still open.

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
Every framework that remains (`agent/antigravity`, `agent/claude`,
`agent/codex`) forks a CLI that manages its own MCP connection and
ignores `RunConfig.Tools` entirely, because there is no in-process
registry to hand a forked process. `Config.GrantTools` still assembles these tools and
`RunDispatch` still passes them, but no `Framework` consumes them, so
`selfrepair`'s host tool reaches no running agent today.

`self-debug` and `bootstrap-playbooks` are the halves that no longer
depend on any of that, because everything they offer is read-only and so
needs no route back into a live run's own conversation. `grain mcpserver`
takes `-grant <name>`, once per grant, and a `Framework` passes one pair
for each grant on the run's own task — `RunDispatch` reads them off the
task's `Grants` into `agent.RunConfig.Grants`, and `agent.GrantArgs` is
the one translation from "what this run may do" to "what that process is
told", shared by all three frameworks the way `RunDeadlineArgs` already
is. A repeated name rather than a flag per capability deliberately: which
tools a name turns on is `mcpserver`'s business alone, so a fourth
capability wanting this treatment is a name at either end and no new flag,
no new field and no framework change in between.

`-grant bootstrap-playbooks` is the simpler of the two: `bootstrap.
PlaybookTools`' `list_bootstrap_playbooks` and `read_bootstrap_playbook`
read markdown runbooks embedded in the grain binary itself, so the
subprocess is already holding everything they serve and needs no hop of
any kind. That matters because `Config.GrantTools` was the only thing
assembling them until this flag existed, and no CLI-driving `Framework`
consumes that map — a task granted `bootstrap-playbooks` was told about
tools that were on no run's roster.

`-grant self-debug` turns on `selfdebug.SourceTools`'
`read_grain_source`/`list_grain_source`, which answer what grain is
*built* to do. They refuse politely rather than disappearing when a
deployment has no source checkout (`-grain-src-dir` unset), so a run's
tool roster is a property of the grants it holds rather than of one
deployment's configuration.

The other half of that question — what this deployment actually *did* —
used to be four more tools on the same grant (`mcp.NewTaskTools`'
`list_grain_tasks`, `read_grain_task`, `read_grain_task_prompt` and
`read_grain_task_transcript`, reading another task's record, its
attempts, the prompt its agent was really handed and its session
transcript through `cmd/grain/mcpserver.go`'s `daemonTasks` over the
daemon's REST API). They are gone, and the state repository is why: the
rows they rendered are files in it now (`tables/task.json`,
`tables/task_comment.json`, `tables/task_run.json`, prompts and
transcripts included — see "The store is a git repository again"), so a
task that needs to read what this deployment did is given read access to
that repository, and reads it with `read_file` and `run_command` like
any other checkout. Four tools, a `TaskReader` interface, an adapter in
`cmd/grain` and two `ui.HTTPClient` methods, all to render what a clone
already hands over. What that trades away is freshness: `task_run` is a
churn-tier table (below), so an attempt's transcript reaches the
repository on `ChurnInterval` rather than immediately, and a
still-running attempt's transcript-in-progress — which the daemon did
serve, from `Config.LiveTranscripts` — is not in a clone at all.

Closing `selfrepair`'s own gap — a tool that blocks mid-call on a human's
reply in the task's chat — means giving the `mcpserver` subcommand a
route back to the store, which is a design question rather than a missing flag: the
isolation that makes the subprocess frameworks safe is exactly that it
holds no store handle. What it does hold is deliberately narrow and
deliberately not that: a read-only GitHub client scoped to one branch
(`-data-dir`/`-pr-repo`/`-pr-branch`, for `pull_request_status` -- see
"Letting a run watch its own CI", below), and a REST client of the
daemon aimed at one endpoint about one task id
(`-server`/`-task`, for `open_pull_request` -- see "A run can open its
own pull request"). Neither is a store handle, and neither can answer
`selfrepair.Confirm`'s blocking read of `Store.Comments`.

bwsalmon/agents#621 once turned that pair of capabilities into an
explicit "configuration agent": an overlay button the frontend kept
reachable in the bottom-right corner of the screen, which filed a task
with nothing but `{"configuration": true}` and opened its chat the moment
it existed. One field on the task (`model.Task.Configuration`, a column
of its own) expanded server-side into `Interactive` forced true, the
`self-debug`/`self-repair`/`bootstrap-playbooks` grants, a default title
and a prompt about helping with a problem, a question or grain's own
configuration -- and `dispatch.Cycle` started every such task
unconditionally, ahead of the capacity-gated loop, so it could get a
sandbox even at the worker limit.

**It is gone.** What it was for was changing this deployment's
configuration, and configuration is not something to change by talking to
a chat agent about it any more: it is the state repository (see "The
store is a git repository again"). Settings, repo configuration, prompt
extensions, schedules and suites are files an ordinary task can be
dispatched at, edit, and open a pull request against, reviewed and merged
like any other change, with `grain state check` validating it before it
lands. A one-click chat that could reach into the running deployment
instead was a second, unreviewed way to do the same thing.

Nothing it was built out of goes with it. `self-debug`, `self-repair` and
`bootstrap-playbooks` are still capabilities, still grantable from the
new-task form or a deployment's defaults, and still reach a run the same
way (`grain mcpserver -grant <name>`) -- what is removed is the bundle,
the button, the field, the column and the exemption from the concurrency
limit, not the tools. A deployment that wants what the button offered
files an interactive task and ticks those grants, which is what the
button was assembling for it. Removing the special dispatch path costs
nothing that the state repository does not already answer: an interactive
task filed to debug a saturated deployment waits its turn like everything
else, and a deployment saturated badly enough for that to matter has a
`MaxWorkers` a person can raise -- through the state repository, or
through Settings.

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
it streams back -- the shape `pkg/agent/codex` follows too. Every
framework a deployment can pick between is a subprocess driver now, and
`agent.Framework` is the seam that makes them interchangeable.

Four things about agy shaped the port, none of them cosmetic.

**It has no `--mcp-config`.** agy registers MCP servers per *user* --
`agy mcp add` writes them into `~/.gemini/config/mcp_config.json`, and
caches each server's tool manifests under
`~/.gemini/antigravity-cli/mcp/<server>/`. A per-user
registration cannot express what grain needs, which is a per-*run*
binding: two runs dispatched concurrently against two different sandboxes
would share one registration, and whichever wrote it last would decide
where both runs' tools landed. So `Framework.Run` gives each run its own
private `HOME` -- a temp directory holding nothing but the config file
naming that run's own `mcpserver` server, and the settings file
authentication needs (below) -- and deletes it as the run returns. That has the same effect `claude`'s `--strict-mcp-config` has
there: the only MCP server a run can see is its own, because there is no
other config file in the `HOME` it was given to find one in.

**It caps a single MCP tool call, and has no `MCP_TOOL_TIMEOUT`.** agy
abandons a tool call after three minutes when nothing says otherwise;
the knob is a `timeoutSeconds` key on the server's own entry in that
same config file, where a positive value is seconds and a negative one
means no cap at all. Three minutes is far short of `wait_for_checks`,
which blocks for as long as CI takes, up to
`mcp.MaxWaitForChecksTimeout` (an hour) -- so a run that deliberately
asked to wait out a slow build would have the call killed under it and
be told the tool failed, which is neither true nor useful. `Run` writes
the key out past that maximum for the reason `agent/claude` raises
`MCP_TOOL_TIMEOUT`: the deadline that ends the wait should be grain's,
whose expiry produces a report, not the CLI's, whose expiry produces a
tool failure.

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
sandbox only through grain's own MCP tools, and `codex` takes a read-only
sandbox that leaves its own tools unable to do damage. agy takes neither,
so a run always sees agy's `run_command`, `view_file`, `write_to_file` and
the rest beside grain's, and those execute wherever agy does -- on the
controller. Three things stand in for the switch. The private `HOME` holds
exactly one MCP server, so grain's tools are the only MCP tools there are.
They are registered eagerly, so the model sees them rather than having to
go looking. And the run's prompt opens by naming *both* rosters: which
`mcp_grain-sandbox_*` tools reach the sandbox, which of agy's own to treat
as unavailable, and the fact that the two rosters share names -- so a
model reaching for "run\_command" picks a tool by its prefix rather than by
its verb, which is the mistake the bare rule ("use grain's tools") leaves
available. `verifyToolRoster` then notes, on the run itself, a roster with
no route to grain at all. And a fourth thing does more than stand in for
the switch: a `PreToolUse` hook refuses the call outright, which a live
model has been watched running into (the next-but-one paragraph). A
deployment that needs a hard guarantee should still run against a kontur
sandbox, where the controller's filesystem is not reachable from the guest
at all.

**agy 1.1.26 has no denylist for its own native tools, and this is now
read off the binary rather than assumed.** The whole of what it offers,
with the evidence -- which is in the tree as of the first capture:
`docs/agy-surface.md` is the binary's own answer to every question below,
regenerated on demand by `.github/workflows/agy-surface.yml`, and every
claim in this section has been checked against it. Three needed changing
when it landed, and they are marked where they appear: the roster is 57
tools rather than the 55 written here before there was a capture, agents
are discovered from a third directory as well as the two named here, and
`agy -p /permissions` wants a credential set before it will answer, even
though it never checks that one. The rest stood.

- **No flag.** `agy --help` lists `--add-dir`, `--agent`, `--continue`,
  `--conversation`, `--dangerously-skip-permissions`,
  `--disable-slash-commands`, `--effort`, `--input-format`,
  `--json-schema`, `--log-file`, `--mode`, `--model`, `--new-project`,
  `--output-format`, `--print`, `--print-timeout`, `--project`,
  `--prompt`, `--prompt-interactive` and `--sandbox`. Nothing names a
  tool. The subcommands are `agent`/`agents`, `changelog`, `help`,
  `install`, `mcp`, `mic-serve`, `models`, `plugin`/`plugins`,
  `remote-control` and `update`; `agy mcp` takes only
  `add`/`remove`/`list`/`enable`/`disable`.
- **`enabledTools` and `disabledTools` are an MCP server's, confirmed.**
  They carry the same `json`/`yaml`/`mapstructure` tags as `command`,
  `args`, `env`, `url`, `headers`, `timeoutSeconds` and `tools`, on the
  server entry in `mcp_config.json` -- agy's own changelog names them as
  fields of that file. Naming agy's tools there is a trap rather than a
  near miss: grain's tools and agy's share names, so listing
  `run_command` would deny the run *grain's* `run_command` and leave
  agy's in place. The capture surfaces the near misses too, and they stay
  near misses: `allowedTools`, `deniedTools`, `allowedToolPrefixes`,
  `deniedToolPrefixes`, `deniedCommandPatterns` and
  `sandboxSystemAllowlist` are all in the binary's string table, and not
  one of them carries a `json`, `yaml` or `mapstructure` tag -- they are
  internal Go identifiers rather than keys any file this repository writes
  can reach. `excludeTools` does carry a tag, which makes it the closest
  thing to the switch this section is looking for; it sits among Gemini
  *extension* settings, and putting it in `settings.json` leaves the
  roster at 57.
- **The settings file is where the permission system lives, not a
  roster.** `settings.json` carries `permissionPreset`,
  `agentPermissions`, `fileAccessPolicy` and `toolConfirmation`
  (`AgentPermissionPreset` and `AgentSettingPolicy` enums) -- machinery
  for *approving* a tool call, which is exactly what `Run`'s
  `--dangerously-skip-permissions` switches off. Nothing there removes a
  tool from the roster.
- **A custom agent replaces the prompt, not the toolset.** A Markdown
  file with YAML frontmatter under `~/.gemini/antigravity-cli/agents/`,
  `~/.gemini/agents/` or `~/.gemini/config/agents/` -- all three, on
  1.1.26, planted together or one at a time -- is discovered even in an
  otherwise empty private `HOME`, the shape `writeAgyHome` builds, and
  `--agent <name>` selects it. Its frontmatter keys are `name`, `description`,
  `mainAgent`, `subagent`, `hidden`, `inheritMcp`,
  `inheritCustomizations`, `commandExecutionPolicy`, `model`, `rules`,
  `skills`, `plugins` and `mcpServers`. None of them names the native
  tools an agent may use. The `enable_write_tools` / `enable_mcp_tools` /
  `enable_subagent_tools` gates that *do* read like that switch belong to
  a *subagent* definition (`define_subagent`), and a grain run cannot
  reach them at all: `define_subagent` is not among the tools agy offers
  the model on the credential grain authenticates with (measured below,
  "A subagent is not a way around the hook").
- **Silence is the failure mode to watch.** An unknown key is ignored
  rather than rejected -- an unknown `settings.json` key still gets a run
  to its authentication check, and an unknown frontmatter key leaves an
  agent discoverable -- but a *known* frontmatter key given a value that
  does not parse drops the agent from `agy agents` without a word
  (`commandExecutionPolicy: off` and `: auto` parse; `deny`, `manual` and
  `DENIED` do not). Anything written here therefore has to be asserted
  live, not merely accepted by the binary.

**Two of those answers were incomplete, and the second of them is a
denial.** The settings file does carry a tool-level ruleset after all, and
agy documents a hook that blocks a call outright. Both are written into
every run's private `HOME` now (`permissionRules` and `hookConfigJSON` in
`agent/antigravity`). Their schemas were established the way the
paragraphs above were -- a throwaway CI job holding a real `agy` 1.1.26 --
and what they *do* was established afterwards, by running that binary
against a real credential and reading the tool steps back off its
`stream-json`:

- **`settings.json` takes `permissions.allow` / `permissions.deny`, and
  they load.** Write the block, ask the binary what it read
  (`agy -p /permissions`, which print mode answers without an agent turn
  and without a *valid* credential, though not without one at all --
  `docs/agy-surface.md`), and it prints one record per rule:
  `global<TAB>deny<TAB>run_command`. Bare names, `run_command(*)` and
  `regex:` forms all survive; a malformed value (`"deny": 12345`) is
  dropped in silence, no rule and no complaint -- the failure mode this
  section already warned about, and the reason
  `TestLiveAgyLoadsGrainsPermissionRules` (`tests/e2e`) now asks the
  binary that question nightly, one assertion per rule
  `permissionRules` wrote. They load for exactly the launch that finds
  them: agy rewrites `settings.json` on *every* start, keeping the keys it
  owns (`modelProvider`) and dropping the whole `permissions` block, so a
  second `agy` in the same `HOME` has no rules at all. Harmless as grain
  runs agy -- `writeAgyHome` builds a fresh `HOME` per run and `Run`
  starts the binary once in it -- and a trap for any future change that
  reuses one or starts agy again to resume a run. What the rules do *not*
  do is change the roster: the `init` event of a real stream-json session
  advertises the same 57 native tools with the block and without it, and
  the same 57 again for `excludeTools`, `permissionPreset`,
  `agentPermissions`, `toolConfirmation` and the `--sandbox` flag
  (`docs/agy-surface.md`; the count was written as 55 here and in
  `withheldNativeTools` before there was a capture to check it against).
  And `Run` passes
  `--dangerously-skip-permissions`, which that same event reports as
  permission mode `always-proceed`, while agy's own prompt calls an
  always-deny decision "overridden by dangerously-skip-permissions". That
  override wins, and it has now been watched winning: a live run holding
  this exact block, asked to list a directory, ran agy's own `list_dir` --
  one of the names it denies -- to completion. So the rules stop nothing
  as grain runs agy. They are written because they are the documented
  place to say what a session may do and they cost a run nothing, and the
  hook below is what actually blocks a call.
- **What dropping that override would cost is a live question, and it is
  now asked nightly.** Enforcing the rules means removing
  `--dangerously-skip-permissions` from `Run`, which leaves a run in agy's
  own `request-review` mode (the same `init` event says so) -- and agy's
  changelog describes a headless run there as *soft-denying* anything that
  would need a confirmation, naming the allow rule that would have
  permitted it. The risk is therefore not a run that is merely stricter
  about agy's own tools: grain's tools are the only ones that reach a
  sandbox, so a run whose allow rules are not matched is a dispatch that
  can do nothing at all. `permissionRules` writes them in the exact
  eagerly registered spelling (`mcp_grain-sandbox_run_command` and
  friends) because agy matches an approval rule strictly unless it is
  prefixed `regex:`, and `TestLiveAgyLoadsGrainsPermissionRules` proves
  those rules reach the binary -- but whether *that* spelling is what agy's
  permission check compares against takes a live model to find out, the
  same way the hook payload log had to establish that a tool name reaches
  the hook in the shape the deny list is written in.
  `TestLiveRunWithoutThePermissionOverride` (`tests/e2e`) is that
  measurement: the same framework a dispatch gets, minus that one flag
  (`antigravity.WithoutPermissionOverrideForTest`, the only seam that drops
  it), asserting that grain's own `run_command` still writes a file into
  the sandbox and reporting what a native call does under review mode --
  including whether grain's `PreToolUse` hook is still asked about one, or
  whether the permission check now refuses it first. Its control is the
  `init` event: a run reporting `always-proceed` measured nothing and fails
  outright, since a vacuous "safe to drop the flag" is the worst answer
  available. **Until that nightly has answered, `Run` keeps the flag** --
  and `TestRunOverridesAgysPermissionPromptsByDefault`
  (`pkg/agent/antigravity`) fails any change that stops passing it.
- **A `PreToolUse` hook is a hard block, and this one has been watched
  blocking.** A live `agy` 1.1.26 driving `gemini-3.1-pro-high`, told in
  as many words to run `echo ... > /tmp/agyprobe.txt` as a shell command,
  called its own `run_command`, then its own `write_to_file`, then
  `find_by_name`, had all three refused by grain's hook, and gave up with
  the file never created. That is the property this whole section is
  about, and `TestLiveNativeToolsAreDenied` (`tests/e2e`) is it as a test,
  run nightly by `live-agent.yml`. Where the mechanism comes from: the
  binary unpacks its customization guide into any fresh `HOME`
  (`antigravity-cli/builtin/skills/agy-customizations/docs/hooks.md`), and
  it specifies `hooks.json` in the global customization root
  `~/.gemini/config/` -- beside the `mcp_config.json` this package already
  writes -- handing each hook `{"toolCall": {"name", "args"}}` on stdin
  before the tool runs and reading a decision back on stdout, where
  `"deny"` means "hard block the execution immediately". That is a
  different mechanism from the permission prompt the flag auto-approves.
  grain's hook is grain: `hooks.json` runs `grain agy-tool-hook`
  (`cmd/grain/agyhook.go`), which answers from `HookDecision`.
- **Whether it is loaded at all is a separate, cheaper question, and it
  is now asked nightly.** `hooks.json` is not validated on load -- one agy
  cannot make sense of leaves a run with no hooks and no complaint -- and
  a live run only shows the hook working when the model happens to reach
  for a native tool. `agy -p /hooks` needs neither: it prints one
  tab-separated record per loaded hook,
  `grain-native-tool-denial<TAB>enabled<TAB>PreToolUse<TAB>*<TAB>command<TAB>'/path/to/grain' agy-tool-hook`,
  out of the config it just read and without an agent turn or a valid
  credential. `TestLiveAgyLoadsGrainsHookConfig` (`tests/e2e`) asserts
  each field of that record, and asserts as its control that the same
  command names nothing when `hooks.json` is removed from the `HOME` --
  without which "the output contains our hook's name" would not be
  evidence of anything.
- **What the hook is asked about is the name in the payload, and the
  nightly now records it.** `HookDecision` matches `toolCall.name` against
  agy's tool names, while agy's own hook guide describes matchers as
  matching *step types* ("lowercasing the step type and removing the
  `CORTEX_STEP_TYPE_` prefix") -- and a name arriving in a shape that deny
  list is not written in would deny nothing, silently. A transcript cannot
  answer it, since it reports the tool that ran rather than the name the
  hook was handed, so `TestLiveNativeToolsAreDenied` points agy's config
  at a stand-in for the grain binary that logs every payload before
  passing it to the real one (`tests/e2e/hook_payload_log_test.go`, which
  also covers the stand-in from the ordinary suite). Every name is logged,
  split into the ones the deny list matches, grain's own qualified names,
  and everything else; a run whose tool calls never reached the hook, or a
  native call the hook was never asked about under a matchable name, fails
  the test.
- **It denies by name, and never allows by name.** `HookDecision` denies
  the tools in `withheldNativeTools` -- agy's own file and command tools,
  now the whole of that set rather than a representative handful -- and
  says nothing at all about everything else. Not "deny anything without
  grain's prefix", because this hook stands in front of *every* tool call
  a run makes: a surprise in the payload would then be a run that can do
  nothing at all, whereas a deny list can only ever fail back to the
  behaviour this repository already had.
- **Saying nothing is the part that has to be exact, and the first
  version of this got it wrong.** It replied `{}` for a call it had no
  opinion about, reading a decision-less object as an abstention. agy
  reads it as a deny -- so every tool call every agy run made, grain's own
  MCP tools included, came back "tool call denied by pre-tool hook:" with
  an empty reason, and a run could do nothing but explain why. The whole
  contract, measured by running a real agy against a real credential with
  a hook replying to order: **no output at all** lets the call proceed and
  is the only spelling of "no opinion"; `"allow"` proceeds; `"ask"`
  proceeds, the prompt being auto-approved by
  `--dangerously-skip-permissions`; `"deny"` blocks and shows its reason
  to the model; `{}` or any object without a `decision` blocks with no
  reason; stdout that is not JSON fails the call (`failed to unmarshal
  result from hook ... via protojson`); and a hook command that exits
  non-zero, or is not there at all, fails the call too. The last two are
  why `grain agy-tool-hook` exits 0 whatever happens and writes either
  nothing or a decision: **a broken hook does not fail open**, it takes
  the run's tools with it.
- **The unit test could not see any of that, and the live test can.**
  Asserting the bytes grain emits is a check that grain says what it meant
  to say, not that agy listens -- and the `{}` bug passed it, because the
  test read the decision field out of parsed JSON, where `{}` and no
  output are the same thing. It now asserts the bytes; the property itself
  is `TestLiveNativeToolsAreDenied`, which tells a live model to use agy's
  own `run_command` on a path outside the sandbox and then looks at the
  controller's filesystem, at whether the hook denied any of grain's own
  tools, and at whether the run still reached its sandbox.
- **A subagent is not a way around the hook, and not because the hook
  covers one.** "Spawn a subagent and have *it* write the file" would
  bypass every denial above if a subagent's own tool calls did not reach
  `PreToolUse`, since `define_subagent` takes an `enable_write_tools`
  flag and neither it nor `invoke_subagent` was on the deny list. That
  question turns out to be unaskable in the configuration grain runs agy
  in, because there is no subagent to ask: **a live model told in as many
  words to call `define_subagent` got agy's own `unknown tool:
  "define_subagent" — check spelling` back, and grain's hook was never
  asked about the call** -- agy refuses an unknown tool before any hook
  runs, so the payload log (`hook_payload_log_test.go`'s recorder) holds
  nothing for it. The same run's `run_command` and `list_dir` were denied
  by the hook as usual, so the instrument was working.
- **The `init` event's roster is an upper bound, not the model's roster,
  and this is where that matters.** That event advertises 57 tools on the
  1.1.26 run captured here, `define_subagent`, `invoke_subagent`,
  `manage_subagents` and the browser tools among them -- but what agy *declares to the model* is
  a shorter list, and the two had never been compared. They can be:
  **agy honours `HTTPS_PROXY`**, so a `mitmdump` holding a CA the machine
  trusts reads the `streamGenerateContent` request bodies whole. A real
  grain run (agy 1.1.26, `gemini-3.1-pro`, API-key auth, sandbox
  host-rooted) declares 24 functions: agy's `view_file`, `run_command`,
  `manage_task`, `write_to_file`, `replace_file_content`,
  `generate_image`, `read_url_content`, `search_web`, `find_by_name`,
  `grep_search`, `list_dir`, `list_resources` and `read_resource`, plus
  grain's eleven `mcp_grain-sandbox_*` tools. No subagent tool, no
  browser tool, and no `call_mcp_tool` (grain's tools being registered
  eagerly instead). Six names in `withheldNativeTools` --
  `command_status`, `send_command_input`, `multi_replace_file_content`,
  `sed_file`, `notebook_edit`, `notebook_execution` -- are not offered
  either, which costs nothing: the deny list is matched against a call
  that happens, not against a roster.
- **What prunes that roster is partly a server-side feature flag, which
  is why the answer above is not a guarantee.** agy fetches its flags from
  `antigravity-unleash.goog` at startup (449 of them on the run captured
  here), and `cascade-enable-invoke-subagent-tool` -- an *experiment*,
  described as "invoke_subagent tool" -- is enabled with a single strategy
  constrained to `ide IN [jetski, jetski-insiders, jetski-dev]`, which the
  CLI is not. Rewriting that response in the proxy (constraints removed,
  rollout 100) did not by itself put the tools in front of the model, so
  at least one further gate is in the binary and the flag alone is not the
  switch. The point stands either way: **which tools a run's model may
  call is decided partly off this machine**, on a service that owes this
  repository nothing, so "agy does not offer subagent tools" is a fact
  about today rather than a property to build on.
- **So `withheldNativeTools` grew by two.** `define_subagent` and
  `invoke_subagent` are now denied by `permissionRules` and by
  `HookDecision` like the file and command tools, which changes nothing
  about a run today -- they are refused before the hook, being unknown --
  and closes the delegation route at the parent's call on the day that
  flag or that gate moves. Denying the *spawn* rather than trying to
  contain the subagent is the only stop that does not rest on the
  unanswered question: it is upstream of whatever a subagent would have
  done, and it needs no theory about whether a subagent's calls are
  hooked. A deployment that wants the guarantee rather than the deny list
  still wants a kontur sandbox, which is what this section has said all
  along.

So the prompt carries the rule, the permission rules say it again where
agy stores policy but stop nothing, the `PreToolUse` hook is where a call
is actually stopped, and a kontur sandbox is still what contains a native
tool that gets past all three. Only the hook has been watched stopping a
live model, and only nightly (`live-agent.yml`), since that is where the
credential is -- though two of the three questions turn out not to need
one: that agy loaded grain's hook, and that it loaded grain's permission
rules, are both answered by print mode against any runner with `agy`
installed, which is why the two tests that ask them fail on a laptop and
in the nightly alike rather than waiting for a model turn.

How that was established is worth keeping, because the question keeps
coming back and a *locked-down* grain sandbox cannot answer it: a sandbox
with no network beyond the git proxy cannot fetch `agy`, a 200MB stripped
Go binary that is not in the image. A throwaway job on this repository's
own CI installed agy with the same installer the Dockerfile runs, asked it
(`agy --help`, `agy changelog`, `agy agents`), read its config schema out
of the string table (`json:`/`yaml:`/`mapstructure:` struct tags, protobuf
accessor names, `jsonschema_description` text), planted agent definitions
in candidate directories to see which were read, and pushed the output to
a branch. `agy changelog` in particular is the closest thing to
documentation there is, since it ships in the binary.

**That job is not throwaway any more.** It is
`.github/workflows/agy-surface.yml` and `scripts/agy-surface.sh`, and the
next question about agy's flags, settings keys or on-disk layout costs one
`workflow_dispatch` rather than a re-derivation -- roughly every other
task that has touched `pkg/agent/antigravity` has needed one. It installs
the *unpinned* agy the Dockerfile would install, so what it reads is the
binary a freshly built image would carry rather than one a comment froze
months ago; it asks the fixed set of questions above, plus what agy
unpacks into a `HOME` it has never seen (its own customization guides
included, which is where the hook contract is written down), which of six
candidate directories its agents actually come from, which of six
candidate files its MCP servers actually come from, and what the `init`
event of a session opened with `Run`'s own argv advertises. It holds no
credential and runs on no schedule; the token that writes a branch lives
in a second job that never runs agy.

The output is committed, at
[`docs/agy-surface.md`](docs/agy-surface.md), rather than left as a
workflow artifact -- for two reasons that are the same reason. A sandboxed
agent cannot download an artifact and can read a file in its own checkout
(or `git fetch origin agy-surface`, the branch each dispatch pushes as one
commit on top of the ref it ran from, to be merged like any other pull
request). And a capture nobody can compare against the last one only
answers "what does agy do today", where the question behind every one of
these dispatches is "what changed" -- so the script writes nothing
run-specific, no dates and no temporary paths, every list sorted, and two
runs against one agy produce identical bytes. A diff is drift.

The behavioural half above went further only because a sandbox with
general network access *and* a Gemini key can do the whole thing itself:
install agy, write the private `HOME` this package builds, point its hook
at a script that replies to order, and drive real model runs until each
decision value has an observed outcome. A task that needs to re-answer any
of this should be given those two capabilities rather than a CI job --
each measurement is one model run of a few seconds, and the answers land
in the same session as the change they justify.

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
`selfrepair.HostCommandTools`, `selfdebug.SourceTools` and
`bootstrap.PlaybookTools`, and `RunDispatch` still passes them, but
nothing consumes them -- the two read-only sets reach a run by
`mcpserver`'s own `-grant` instead ("configuration mode", above), and
`selfrepair` is what is left, so an Interactive task's
`run_host_command` confirmation prompt
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

Authenticating that way is two things, not one, and grain does both. agy
reads `GEMINI_API_KEY` only for a session whose settings ask it to --
`"modelProvider": "gemini"` in `.gemini/antigravity-cli/settings.json` --
so `Framework.Run` writes that file into the private `HOME` beside the
MCP config whenever it has a key to pass. Without it agy ignores the
variable, falls through to the interactive browser login its OAuth
sessions come from, and, with a prompt on stdin rather than a terminal,
exits 1 with `authentication required. Run 'agy' to log in, then retry`.
The credential a run uses is grain's own either way: `GOOGLE_API_KEY` is
cleared in the same environment, because agy prefers it when both are
set and the subprocess inherits the controller's.

A model selection is two values, not one: agy takes `--model` and
`--effort`, and it refuses a bare family name without an effort. Both
spellings work, and both were measured against agy 1.1.26 with a live key
rather than reasoned about:

| command line | agy |
| --- | --- |
| `--model gemini-3.1-pro` | refused — `--model gemini-3.1-pro requires --effort (available: low, high)` |
| `--model gemini-3.1-pro --effort high` | runs |
| `--model gemini-3.1-pro-high` | runs |
| `--model gemini-3.1-pro-high --effort high` | runs |
| `--model gemini-3.1-pro-high --effort low` | refused — `gemini-3.1-pro-high conflicts with --effort=low` |
| `--model gemini-3.1-pro --effort medium` | refused — `gemini-3.1-pro has no "medium" effort` |

So `antigravity.DefaultModel` is the bare family name and
`antigravity.DefaultEffort` is the effort passed beside it, which is what
lets a deployment change a run's depth without also choosing a different
model (`grain settings -gemini-effort`, or the picker on Settings →
Agents). The catalog `agy models` prints is still the suffixed spelling
(`gemini-3.1-pro-high`, `gemini-3.8-flash-medium`), so a deployment that
named one of those before the effort was a setting of its own has it
stored — and `Framework.Run` passes no `--effort` at all for a model
whose name already carries one, since agy refuses that pair unless the
two happen to agree. Which efforts a model has is the model's own
business (3.1 Pro has no `medium`); nothing in grain checks the pair, and
agy refuses it before the run starts if it does not go together.

Which leaves the model name itself, and it was the last free-text field
over a vocabulary only the binary defines: a name agy has never heard of
saved without complaint, and the refusal arrived on the next dispatch as a
run that failed before it started. The effort had a picker because three
words could be written down in Go (`antigravity.Efforts`); the models
could not, because they change when agy is upgraded. So the pane asks the
binary (grain/task-365). `antigravity.Catalog` runs `agy models` and
`agy --help` under the same private, API-key-authenticated `HOME` a run
gets -- minus the per-run MCP config and hooks a listing has no use for --
parses the listing into ids, agy's own display labels, and the family and
effort suffix each id carries, and reads the effort vocabulary out of
`--effort`'s own usage line rather than assuming `Efforts()`, because that
is the word list those suffixes are split against.
`GET /api/agent-models` serves it, `cmd/grain`'s `agyModelLister` wires it
to the same `-agy-path` and Gemini key a dispatch resolves (caching a
successful listing for five minutes, and a failure not at all), and the
model field becomes a drop-down grouped by family.

The catalog also answers, for the first time in this repository, the
question the table above leaves to agy: which efforts a given model
actually has. It is written into the ids -- 3.1 Pro is listed `-high` and
`-low` and never `-medium` -- so the effort picker beside the model now
offers what that model was listed with, narrowed from (never widened
beyond) the vocabulary `Settings.geminiEfforts` reports and
`ui.UpdateSettings` validates against. Pick a model whose pairing agy
would refuse and the refusal is simply not on the menu.

The model half is a drop-down that still takes a written-in value, and
that is not a hedge: reading the catalog needs an installed agy, a working
credential and quota to spend, and a listing shaped the way this parser
expects. Every one of those can be false on a deployment that is otherwise
fine -- including on the very pane where the missing key would be pasted
in. So `GET /api/agent-models` answers 200 in all cases, distinguishing
"no lister is wired here" (`grain demo`, any UI not colocated with the
daemon) from "one was asked and could not answer", and `GeminiModelFields`
renders the same typable field either way with the reason under it, the
effort picker falling back to the server's whole vocabulary. A model name
grain has never seen still saves and is still passed to agy exactly as
typed; the picker only ever removes the need to remember one.

### Proving a live run actually gets the tools

Where that private `HOME` puts the MCP config is a fact about agy's
on-disk layout, and it is the one thing about this package no test in
this repository can check for itself. `agent/antigravity` wrote the
config to `~/.gemini/settings.json` for a while -- the file Gemini CLI
kept the same `mcpServers` map in -- and agy, which reads
`~/.gemini/config/mcp_config.json` and nothing else, silently loaded no
MCP servers at all. Every test covering that wiring stayed green
throughout, because every one of them drives a scripted runner or a stub
CLI: a stub reads whichever file the test told it to read, so the tests
agreed with the code about a name they had both got wrong. What that
costs in production is not an error, which is the difficulty -- agy
starts, answers, and has no way to touch the sandbox.

Fixing it settled the name against the real binary, without a credential:
on agy 1.1.25, `agy mcp add` writes `~/.gemini/config/mcp_config.json`,
`agy mcp list` reads it back, and the identical `mcpServers` map placed
in `~/.gemini/settings.json` lists no servers at all. That is static
analysis of the binary, and it stops one step short of the end of the
chain: it says which file agy reads, not that a real run holding a real
credential comes up holding grain's tools.

`tests/e2e/live_test.go` is where that last step is taken. It is the only
test that execs the real `agy`, and it now asserts the two things a
scripted run cannot show, separately and in this order:

- **a route to grain at all.** agy's opening `init` event names the tools
  it loaded, so the test keeps the run's raw `stream-json`
  (`RunConfig.TranscriptPath`) and reads that event back: the roster must
  carry `call_mcp_tool` or an `mcp_grain-sandbox_*` name. This is checked
  before the run's own error is, deliberately -- a run given no tools
  fails later on anyway, and every one of those later failures reads like
  a model that would not do as it was asked rather than like one that was
  never handed a way to.

  It asked a stronger question first, and got it wrong: it demanded
  `mcp__grain-sandbox__*` names in that roster, on the theory that the
  roster is the run's whole tool vocabulary. Running it proved otherwise.
  agy's `init` event lists agy's own 57 native tools and *never* an MCP
  tool, under either spelling, eagerly registered or not -- so the old
  assertion would have failed every live run, and blamed a config file
  that was in the right place all along.
- **a call that landed.** At least one `run_command` call has to have
  returned without error, and the assertions already there -- the pushed
  branch, the line in `NOTES.md` -- are what say it reached this run's
  own sandbox rather than merely existing on a roster.

**A nightly job runs it now, and no push-triggered job ever will.**
`.github/workflows/live-agent.yml` runs this one test on a `schedule`
(and on `workflow_dispatch`), on a runner it installs `agy` onto from the
same unpinned installer the Dockerfile uses, holding a `GEMINI_API_KEY`.
Leaving it to whoever remembers was the same shape of gap that let
`~/.gemini/settings.json` survive in the first place. Three alternatives
were weighed and rejected:

- **Not `tests.yml`.** That workflow holds no credential at all: it
  triggers on `pull_request`, which runs code from an unreviewed branch,
  and is only safe to trigger that way for as long as there is nothing in
  it worth stealing. Giving that job a key would hand every PR branch the
  deployment's own model credential.
- **Not every push to `main`** (nor `build-artifacts.yml`, which already
  holds a credential and triggers on `push`). The cadence settles it:
  `main` took 787 merges in the two weeks to 2026-09-04 -- about 56 a
  day, 170 on its busiest. What this test catches is agy's own layout,
  flags and event shape drifting, and that drifts when agy ships, not
  when grain commits, so re-asking it 56 times a day buys nothing over
  asking it once. The model is also free to decline, or to be
  rate-limited: per-push means a regularly red `main`, on the very
  workflow whose green tick otherwise means "the image published from
  this commit came up".
- **Not weekly either.** The Dockerfile installs `agy` from its vendor's
  installer with nothing pinned, so the agent a freshly built image
  carries can change on a day nobody touched this repository. Nightly
  costs six more model runs a week than weekly and cuts the worst-case
  detection window from seven days to one.

**`push`-triggered-therefore-trusted is not sound here**, which is the
part worth writing down. `build-artifacts.yml` makes that argument for
its `GITHUB_TOKEN`: a `push` event only fires for a branch in this
repository, which takes write access to create. True, and insufficient
for a model credential -- nearly every branch here is pushed by one of
grain's *own agent runs*, so a workflow file on a branch is code no human
has read. And a *repository* secret is readable by any workflow on any
branch push, whichever file declares it, so an `if: github.ref ==
'refs/heads/main'` guard would be a convention written in a file the
branch gets to rewrite, not a boundary.

So the boundary is the one GitHub enforces, and it is what a maintainer
has to create for this job to work:

- `GEMINI_API_KEY` as a secret on a **`live-agent` environment**, not a
  repository secret, with a deployment branch policy naming the default
  branch alone. A job on any other ref is then refused the secret before
  it starts, whatever that branch's copy of the workflow says.
- **No required reviewers** on that environment. Gating a nightly on
  someone clicking approve is the same "runs when a human remembers" gap
  this job exists to close. Bound the blast radius the other way instead:
  give it a key of its own, restricted to
  `generativelanguage.googleapis.com` -- the restriction
  `pkg/capability/geminikey` puts on every key it mints -- and rotate it
  independently of the daemon's operating key.

Until both exist the job's "Require a credential" step fails every night,
on purpose and with the instructions in the error: it blocks no merge,
and a job that skipped quietly would be this same gap in a new place. The
job runs the test twice before reporting a failure, because the
regression it exists for (a live agy holding none of grain's tools) fails
both attempts while a model that declined once usually does not decline
twice. `tests/deploy` asserts the arrangement rather than trusting it:
that the two branch-triggered workflows read no `secrets.*` at all, and
that this one triggers on nothing but the schedule and a manual dispatch.

**A maintainer can still run it by hand**, which is what a change to
`pkg/agent/antigravity` wants rather than waiting for the clock:

```
GRAIN_LIVE_AGENT_TEST=1 GEMINI_API_KEY=... \
  go test ./tests/e2e/ -run TestLiveIssueCompletesEndToEnd -v
```

The two that ask agy what it *loaded* rather than what it does need no
key, since print mode spends nothing (agy still refuses to start without
`GEMINI_API_KEY` set to something, so they pass a placeholder when the
environment has none):

```
GRAIN_LIVE_AGENT_TEST=1 go test ./tests/e2e/ -run TestLiveAgyLoads -v
```

with `agy` on `$PATH` and a Go toolchain to build `cmd/grain` with (agy
reaches the sandbox by forking grain's own `mcpserver` subcommand, so a
live run needs the binary as well as the key). `GRAIN_LIVE_AGENT_TEST` is
there because a skip and a pass are the same `ok` in `go test`'s summary:
with it set, an unset key or a missing `agy` fails the run instead of
skipping it, so nobody comes away believing the roster was checked when
nothing checked it -- and it is what the nightly job sets for the same
reason. The `-v` output logs the whole advertised roster, which is the
line to read.

**Last exercised: 2026-09-04, against agy 1.1.25 and a live Gemini API
key.** It passed, and it had to be repaired first: the run was cut off
five minutes in by agy's own `--print-timeout` default, its tools were
loaded lazily behind agy's `call_mcp_tool` dispatcher rather than
registered, and the names it did report were spellings grain did not
recognize. Those are fixed; what the roster came back as is the 57 native
tools quoted above, with `call_mcp_tool` among them and no grain tool at
all -- which is what the assertion now looks for. The proof that the run
reached its sandbox is the call that landed: `run_command`, recorded
under its bare name, which cloned, branched, committed and pushed.

The lesson to keep is the one that generalizes past this run: **every
fault it found was a difference between what agy does and what
`agent/antigravity` assumed it did, and every scripted test in this
repository asserted the assumption.** A fake that speaks the protocol
faithfully is still a fake of one's own beliefs about the protocol. Run
this by hand after anything that changes how agy is invoked, and after an
agy upgrade -- and when it fails, suspect the belief before the code.

## Letting a run watch its own CI

A run could always push more than once — the git proxy authorizes every
push to the task's own target (`gitproxy/authorize.go`), and
`ConfigureGitCredentials` leaves a working identity and credential helper
behind — but it had no way to find out what CI made of a push. The
checks were read minutes later by a different process
(`SyncPullRequests`), and a red build became a whole further dispatch
(`requeueForRepair`, a separate fix task before that), into a cold
sandbox, to repair something the run that broke it was still sitting
there able to repair.

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
and merge in a turn — which is cheaper than the repair the merge queue
would otherwise ask for minutes later in a cold sandbox.

## Waiting for CI instead of polling it

`pull_request_status` answers "what does CI say right now", which is the
honest answer to a question no run actually has. A run that has just
pushed wants to know how CI *ended*, and the only way to get that out of
a status read is to call it, be told the unfinished checks carry no
verdict, spend a turn waiting, and call it again. Every one of those
turns is a model round trip spent on waiting rather than on work, and a
run that guesses the interval wrong either burns its turn budget or --
worse -- reads one queued check, decides it has waited long enough and
finishes on a build it never saw the end of.

`wait_for_checks` (`pkg/mcp/wait_for_checks_tool.go`) moves that loop
inside grain: one call, and the answer is always a verdict rather than a
snapshot. It returns the moment the outcome is settled, which is not the
moment CI is finished — **any check failing ends the wait immediately**,
without waiting for the rest, since there is nothing left to learn about
a build that already has to be fixed and the sooner the run is told the
more of its budget is left to fix it in. Every check completing with
none failing ends it too, and that is the one green light in there.

Four clocks bound it, none of them an agent's to invent:

- `timeout_seconds`, the only argument, defaulting to 15 minutes and
  clamped to 30s..60m. A timeout is *reported* — with what each check was
  doing when it ran out, and the reminder that an unfinished check has
  not passed — rather than raised as an error.
- the run's own deadline, when grain told this server about one (see
  "Telling a run how long it has"). A wait is worth running only if the
  run outlives it, so each one is cut to what is left minus two minutes
  to act on the answer, and the report says when that is what bounded it
  — *"I waited up to 8m0s: the 10m0s this run has left before grain
  cancels it, less 2m0s to act on the answer — not the 15m0s asked
  for"*. Below the shortest wait there is, it does not wait at all and
  says so.
- a 3-minute grace on an entirely empty check list. GitHub reports no
  checks both when CI has not registered them yet and when the repo has
  no CI at all, and blocking the full timeout to tell those apart would
  spend fifteen minutes learning nothing. Once any check has appeared,
  the grace no longer applies: a slow build is waited out to the timeout.
- a 15-second poll interval, and a tolerance of three *consecutive*
  failed reads. One 502 in the middle of a long wait is a blip; a
  credential that cannot see checks at all fails every read, and
  surfacing that as an error beats reporting a timeout half an hour later
  and hiding the real cause behind a wrong one.

The scope is fixed at process start exactly as `pull_request_status`'s
is, and the commit is pinned too: the branch head is read once, up front,
so an answer can never mix verdicts from two different pushes. A branch
that was never pushed is answered immediately rather than waited on.
Cancelling the run reaches into the sleep, since that is the only place
the call spends any time.

One piece of wiring the tool cannot do for itself: `claude` caps how long
it will let a single MCP tool call run, well below the hour this one may
block for, and a cap left where it was would kill the call part-way
through a wait the agent deliberately asked for and report it as a tool
failure. `agent/claude`'s `mcpToolTimeout` raises `MCP_TOOL_TIMEOUT` to
just past `mcp.MaxWaitForChecksTimeout` so the tool's own clock is the
one that ends the call — and leaves an `MCP_TOOL_TIMEOUT` already in the
environment alone, since an operator who set one outranks that default.
`BuildPrompt` names `wait_for_checks` first and `pull_request_status`
only as the polling it saves, for the same reason the loop itself is
spelled out: a run told to "check CI" reaches for the status read and
then invents a waiting strategy out of turns it could have spent working.

## Putting the failing job's log in the CI answer

Both of those tools used to stop at the name: `FAILING go (failure)`.
That is enough to know the build is red and not enough to do anything
about it, so a run's next move was always to guess at what broke and try
to reproduce it — in a sandbox that is not the runner and may not be able
to run the failing suite at all. It is the same gap `github.JobLog`'s own
doc comment describes and `requeueForRepair` already closes for the merge
queue's own repairs, whose comment carries the tail of each failing job's
log rather than a job name.

So `pull_request_status` and `wait_for_checks` carry it too: under the
check list, each failing job's own URL and the end of its log, fenced in
four backticks so a log containing a fenced block cannot close the quote
early. Rendered by `github.JobLogExcerpt` and bounded by
`github.JobLogTailBytes` over the wire and `github.JobLogTailLines` on
the page — the same excerpt `requeueForRepair` builds, shared rather than
copied so a run reading its own break cannot be shown a
differently-bounded log than the repair asked for that same break.

Two things keep it cheap and keep it from taking the answer down with it.
The logs are read only once something has actually failed — never per
poll, since `github.FailedJobLogs` is three GitHub reads (a commit's
runs, a failed run's jobs, each failed job's log) and a fifteen-minute
wait that went looking every fifteen seconds would spend the whole wait
reading nothing. And a credential that may not read Actions logs, or CI
that is not Actions at all and so has no job log to fetch, degrades to
the answer these tools gave before — the checks, with the failing ones
named — with one sentence saying why there is nothing under the failure,
rather than turning a report of a red build into an error about logs.
`PullRequestReader` grows to six methods for it, still read-only and
still unable to name a repo the caller has not already pinned.

## Telling a run how long it has

`Config.MaxRunRuntime` cancels `framework.Run`'s context outright after
two hours by default (`RunDispatch`'s own `context.WithTimeoutCause`),
and until now nothing told the run. Not the prompt, not a tool. A run
that does not know a deadline exists has no reason to commit early, no
reason to push a half-finished branch before reaching for one more
refactor, and no basis for choosing between waiting fifteen minutes on CI
and finishing — and when the clock does run out, `salvagePushedBranch`
rescues what was *pushed*, while everything only committed, and
everything only edited, goes with the sandbox.

It is told twice now, because once is not enough.

**In the prompt**, with the real number: `BuildPrompt` takes
`cfg.maxRunRuntime()` and states it, together with what to do about it —
commit and push each piece as it starts working rather than saving one
push for the end, and, near the end, push what works and leave the rest
in a `comment_on_issue` note. A task with no repo gets the same deadline
and the other instruction, since its work product is that note and there
is no branch for it to push. This is exactly the class of fact
`BuildPrompt` exists to state: grain's own, unreachable from inside the
sandbox, and not the agent's to guess.

**On every tool result**, once the run is inside 20 minutes of the wall
(`mcp.RunDeadlineNoticeWindow`). A prompt is read once, at turn 1, hours
before the deadline it describes starts to matter, and a tool result is
the one piece of text a run reads every single turn. So
`mcp.Registry.AnnounceDeadline` appends a line to each answer —
`[grain] 14m left before grain cancels this run and destroys its
sandbox…` — with the same advice, escalating in the last five minutes
from "finish this piece and push it" to "there is no time for another
edit-and-test cycle". It rides on failed results too: that is the likelier
moment for a run to start a long repair it will never get to push.

**And in `wait_for_checks`' own arithmetic**, which is the one tool that
can do better than report the deadline. It decides how long to block
before it answers, and a run eight minutes from the wall that asked for
a fifteen-minute wait used to spend the whole of its remaining life
inside that single call: cancelled mid-wait, never shown the verdict it
blocked for, never given the turn it would have used to react — and the
wait itself was what ate the time the fix would have been pushed in. So
the registry hands the deadline to the handler on its ctx as well as
appending it to the answer, and the wait is clamped to what is left less
two minutes to act on the result, with the clamp stated on the report
(*"not the 15m0s asked for"*) since a clamp a run cannot see reads as CI
being slow. A timed-out clamped wait is told its own clock ran out
rather than to retry with a bigger `timeout_seconds`, and a call with
less than the shortest wait's worth of run left answers immediately with
*"there is no time to wait on CI"* and what to do with the turn instead.
`claude`'s `MCP_TOOL_TIMEOUT` is unchanged by this: it is set once, with
the whole run ahead of it, and still has to cover the longest wait a run
with time to spare may ask for.

The deadline reaches that server the way the branch already does. Each
`Framework.Run` receives the very context `RunDispatch` derived with the
deadline on it, so `agent.RunDeadlineArgs` reads `ctx.Deadline()` and
passes it to the forked `mcpserver` as `-run-deadline <RFC3339>` — an
absolute instant, not a budget, because that process is forked some way
into the run it serves and a budget measured from its own start would
hand the run back time it has already spent. There is deliberately no
second copy of the number on `RunConfig` to drift out of step with the
context that actually ends the run. A context with no deadline passes no
flag, and those runs' tool results read exactly as they always have.

## What `run_command` says when it, not the command, ended it

Every `run_command` is bounded. `mcp.defaultRunCommandTimeout` is five
minutes and applies even when the call passed no `timeout` at all, which
is the whole point of it — and until now neither transport said so when
that bound was what ended the command. Locally the answer was `exit=-1`,
which is also exactly what "the command could not be started" looks like
(a process killed by a signal has no exit status of its own). Remotely it
was a bare `exit=124` out of the guest's own `timeout` coreutil. Neither
named the number, and when the number is the default it is one the run
never chose and cannot see, so the move that follows is re-running the
same long command verbatim, twice, before concluding the sandbox is
broken.

Both now append a line: `Killed after 300s by run_command's default
bound, which this call did not choose — it passed no timeout … Pass a
larger timeout (milliseconds, up to 600000) or narrow the command.` A
caller that did choose the bound is told that instead, because sending
that run to look for a grain default that had nothing to do with it is
its own wasted turn. Locally the trigger is `ctx.Err()` being
`DeadlineExceeded` *and* the bound having actually elapsed, so a run
whose two-hour wall clock fired first is not told the wrong number;
remotely it is `timeout`'s own reserved 124.

The remote bound is also a bound now, which it was not. Measured on a
real grain guest: plain `timeout N` against a command that traps SIGTERM
waits for that command to finish of its own accord — sixty seconds, for a
sixty-second sleep — and only then reports 124, so a `run_command` whose
command ignores SIGTERM held its tool call open for as long as it liked,
and a run only advances a turn when its current call returns. It gets
`--kill-after=5s`, and the `exit=137` that escalation produces gets its
own explanation, hedged, since 128+SIGKILL is also what the kernel's OOM
killer leaves behind. It deliberately does *not* get `--foreground`:
plain `timeout` runs the command in its own process group and signals the
whole group, which is `procgroup.Prepare`'s guarantee for the local
transport arriving by another route — confirmed on a real guest, where a
backgrounded grandchild does not survive the bound — and `--foreground`
means, in its own documentation's words, that "children of COMMAND will
not be timed out".

What the remote path genuinely lacked is a bound on the *call* rather
than on the command: nothing on this side can make a guest answer. So it
now also carries a local deadline of the bound plus 30s
(`sshRunCommandGrace`), whose notice says the guest never answered, that
what the command did is therefore unknown, and that `recreate_sandbox` is
the escape hatch if it repeats. The hang that was suspected to need it —
a command backgrounding something that inherits the channel's stdout,
which is enough to wedge an OpenSSH client waiting for EOF — does not
happen here: `./bgtest.sh &` returns in about two milliseconds on a real
guest, because the Go SSH client `kontur exec` uses returns on the
exit-status message rather than waiting for the streams to close.

## No single answer may eat the run's context

`run_command` returned the whole of stdout and stderr; `read_file` with
no `limit` returned the whole file, line-numbered. One `go test ./...` on
a failing suite, one `git log -p`, one `grep -r` that matched more than
expected, one 250 KB README — each was a large fraction of a run's
context spent in one turn, and the run cannot get it back.

Past that point the behaviour was whichever CLI happens to be driving the
run: `claude` caps an MCP tool's output itself and rejects a result that
exceeds the cap, so an oversized answer costs the run the wall clock the
command took and buys it nothing at all; antigravity is a different CLI
with its own answer. grain already reaches into exactly this set of knobs
where it needs a guarantee — `mcpToolTimeout` sets `MCP_TOOL_TIMEOUT` so
a `wait_for_checks` call is not killed before it can report — and set
nothing for size. So the point at which a tool result stopped working was
a per-framework default grain had not chosen, did not know, and could not
explain to the agent when it was hit.

`mcp.maxToolResultBytes` is that choice: 16 KB, applied where the answer
is formatted, so it holds for every framework at once. Both `run_command`
streams share the one budget rather than getting one each — a build that
fails prints kilobytes of stderr and megabytes of stdout, or the reverse,
and a stream that fits keeps all of it and lends the rest to the other.
The head and the tail are kept, never just the head: a command's verdict
is in its last lines and its invocation in the first, and the middle is
what nobody needed. What goes between them is the part only the server
can write — how much was dropped, that grain dropped it rather than the
command, and the next call that gets it back: narrow the command or
redirect it to a file, and for `read_file` the `cat -n` numbering either
side of the cut is exactly the `offset` and `limit` to ask for. The cut
snaps to line boundaries so that numbering stays whole.

**16 KB is a measurement, not taste.** The cap started at 64 KB with the
code saying outright that the number was a guess, deliberately a `var`
until the result-size telemetry (`docs/agent-ergonomics.md`'s finding
11) could say what runs actually ask for. This is that window — the 90
days to 2026-09-04, over the 23 runs of grain's own deployment that
recorded a census, from `grain metrics -window 90d`:

```
tool use (23 run(s) in the window recorded what they called)
  calls                      1736  (76 per run at the median, 118 at p90)
  errored calls                73  (4.2% of them -- a handful is the ordinary shape of this work)
  tool                 runs    calls   errors timed out  mean bytes   p95 bytes
  run_command            23     1162       4%    1 (0%)        2163      <=8191
  edit_file              21      321       1%         -          48       <=127
  read_file              17       92      22%         -        6736     <=32767
  wait_for_checks        21       34       3%         -         729      <=1023
  pull_request_status       20       29       0%         -         838      <=1023
  write_file             14       26      19%         -          71       <=255
  open_pull_request       21       21       0%         -         237       <=511
  read_grain_task         3       16       0%         -        2770     <=16383
  comment_on_issue       13       13       0%         -         184       <=511
  propose_task           10       12       0%         -         160       <=255
  list_grain_tasks        3        6       0%         -        7243     <=32767
  ask_question            2        2       0%         -         197       <=255
  list_grain_source        1        2       0%         -         183       <=255
  (p95 bytes is an upper bound: sizes are kept in base-2 buckets, so the real
   number is inside the octave below it. It is what should size the tool-result cap.)
```

with the p50, p99 and max of the two capped tools out of the same
report's `-json`: `run_command` `p50 <=1023`, `p99 <=32767`, `max
43460`; `read_file` `p50 <=4095`, `p99 <=65535`, `max 44860`.

Two things fall out of that. **64 KB never fired.** The largest answer of
any kind in those 1,254 capped calls was 44,860 bytes, so the cap was
inert for the whole window — and with nothing ever elided, whether a run
takes the notice's advice is a question this window cannot answer at all,
rather than one it answers badly. **16 KB is not tight either.** At least
95% of `run_command` answers pass whole under it and half are under 1 KB;
`read_file` is the heavier of the two distributions — its p95 is in the
16–32 KB octave, so a few in twenty are cut — and it is also the tool
whose notice recovers exactly, since the `cat -n` numbering either side
of the cut is the `offset` and `limit` that fetch the missing range.

16 KB is also antigravity's own per-result default, and `agy` is what
grain dispatches with unless a deployment says otherwise. That is what
settles one number rather than one per tool, in spite of the two
distributions differing: a framework's own limit is per *result*, so a
32 KB `read_file` answer grain let through is one `agy` cuts instead,
with none of the notice that says what went and how to get it. It stays
a `var` rather than becoming a stored setting for the same kind of
reason it stopped being a guess — one deployment's distribution is not
two deployments disagreeing, and a second one that disagrees, or that
runs a framework with a higher limit of its own, is what would make it a
setting.

## Telling attempt N what attempt N−1 did

A redispatch got the task, the conversation, its attachments and a
checkout continuing the previous attempt's branch (`prepareCheckout`,
which is the right thing to hand it), and nothing at all about the
attempt that made those commits. So attempt 2 opened on a branch it had
not written, with no account of what any of it was for, or of why the
attempt that wrote it stopped — and grain had all of it the whole time.
Every `task_run` row carries `outcome` and `detail`, and since `outcomeOf`
that detail describes a run that succeeded as well as one that failed.
The cheapest thing that costs is re-doing a diagnosis grain already paid
for. The dearest is re-attempting exactly the thing that ran out of wall
clock, which is the one ending a branch cannot reveal.

`BuildPrompt` now takes an `orchestrator.History`: the attempts before
this one, the commits they left on the branch, and the conversation. The
first comes from `store.Runs` with this run's own row filtered out —
`dispatch.Cycle` writes that row before `RunDispatch` ever sees the
dispatch, so a prompt that listed it would be telling a run about itself,
with an empty outcome because it has not happened yet. The second comes
from `checkoutCommits`, a `git log <base>..HEAD` run through the same
sandbox `run_command` tool `prepareCheckout` clones with — the only place
in a dispatch that has a repository rather than a string — with each line
marked so it can be picked back out of the tool's own `exit=`/`stdout:`
framing, exactly as `baseGoneMarker` already is.

It reads as a list: per attempt its number, outcome and `detail`, then
the branch's commits newest first. Bounded on purpose — three attempts,
240 bytes of one-line detail each, ten commits, and a pointer at `git
log` for the rest, which `RunDispatch` can say without a second read
because it asks `checkoutCommits` for one commit more than the list holds.
This is orientation, not a transcript store: the transcript stays in
`task_run.transcript` and the UI's pane over it, being prose,
per-framework and unbounded.

Where it sits is part of the fix. Both history sections go together,
immediately after the sentences saying where the checkout is and which
branch is in it — the facts they explain — and ahead of the
commit-message, CI and budget paragraphs, which is why
`commentThreadSection` moved out of `prepareCapabilities` and into
`BuildPrompt` alongside it. The conversation and the attempt history are
the same kind of fact, something a run would otherwise pay to
rediscover, and a run told to read the thread before doing anything else
should not have to find it three paragraphs past the one telling it how
to push.

Neither read can fail a dispatch for nothing. The store read is fatal for
the same reason the conversation and attachment reads either side of it
are: a store that cannot answer it cannot record how this run ends
either. The git read is best effort and silent — a missing base (it falls
back to `origin/HEAD`, the survivable case `baseCheck` already carries on
through), a branch with nothing on it, a sandbox with no checkout at all
— because the commits are on the branch for the agent to read either way,
and orientation that could fail a run would be a worse trade than the
re-diagnosis it saves. On a first attempt it is not read at all: there is
nothing on that branch its base does not already have.

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

**A phone gets a shell that fits it; a tablet gets the one above
(grain/task-19).** Every view here is built around a permanent 232px nav
rail standing beside a pane, which a tablet has the width for and a
phone does not: on a 390px screen the rail is more than half the window,
a task row truncates everything after its title, and a board column is a
third of a screen wide. `ui/src/phone.js` asks the one question — under
600px wide, or short and landscape, which is a phone turned sideways and
never a tablet — and both the layout and the stylesheet answer it the
same way, because `App.jsx` publishes the answer as `data-phone` on
`<html>` exactly as `AppThemeProvider` publishes the resolved light/dark
mode. That is what keeps `style.css` from carrying a second copy of the
media query, and it reaches the overlay panes, which MUI renders into a
portal outside the shell. What a phone gets is not a second UI: the same
`Sidebar` element moves, whole, into `PhoneNav`'s drawer behind a top bar
(and the drawer closes on any tap inside it, since every control in the
rail is a navigation), panes take the full width, centered dialogs go
full screen, and `style.css`'s phone block brings the page gutters in,
wraps a task row's chips onto lines of their own, widens a board column
to the screen and stacks the task detail's two columns.
`ui/e2e/phone.spec.js` is where that is checked, because it is the only
suite with a layout engine in it — jsdom can say which elements rendered
and nothing about how wide they came out — and it asserts the tablet half
of the bargain too: at 820px the rail is still beside the page and a pane
still starts exactly where the rail ends.

**Agent prose is rendered as the markdown it is written in
(grain/task-93).** Every framework grain dispatches answers in markdown
by default, so a relayed question or a `comment_on_issue` note arrives
with headings, bullet lists, backticked paths and fenced blocks in it,
and a task another run proposed arrives with a body written the same way.
Rendering those as one flat run of text is what made a long answer hard
to read, so `ui/src/components/Markdown.jsx` (react-markdown, with
remark-gfm for tables and task lists) is what a comment body and a task
description go through now, with `style.css`'s `.markdown` block carrying
the element styles. Three things about it are deliberate. It renders no
raw HTML — react-markdown skips embedded HTML without `rehype-raw`, which
is not installed, and drops a `javascript:` href — because these strings
reach the page straight off an agent or a GitHub user, and going through
a parser rather than a regex pass is the whole point. It keeps
remark-breaks, so a single newline stays a line break the way it does in
a GitHub comment and the way the plain-text rendering this replaced did:
without it a hand-typed reply's own line breaks would silently fold into
one paragraph, which would be a regression dressed up as a feature. And
it is deliberately *not* used for an attempt's transcript
(`AttemptTranscriptOverlay`), which is a log of thinking, text and tool
calls whose alignment is the whole of its legibility — that stays in its
`<pre>`.

**Opening a thing fills the pane beside the sidebar (grain/task-94).**
That markdown made a long agent answer legible; it did not make it fit.
A task's detail opened as a 900px box floating in the middle of the
screen, so an answer with headings, a fenced diff and a list in it was
still read through a porthole with the page's own margins on either side
of it. `Overlay.jsx` grows a third shape for that — `pane`, a full-height
`Dialog` sized to `100% - SIDEBAR_WIDTH` and pushed to the right edge, so
it covers exactly the content area and leaves the sidebar showing beside
it. The paper is the viewport's full height and `.overlay-pane` scrolls
its own content, the same split `.main-column` already draws for a list
page, and the property column beside the task (state, actions,
capabilities, dependencies) is `position: sticky` so a long timeline
scrolls past it rather than carrying the buttons off screen. The one
number the two pieces of chrome have to agree on, `SIDEBAR_WIDTH`, moved
into `theme.js` so neither the sidebar nor the pane owns the other's
width.

Schedules, templates and suites open the same way, which is the other
half of the ask: those three pages had already been split into a list
plus a sub-page overlay (bwsalmon/agents#545, #547, #642), so making
their overlays panes means one gesture — click a row — has one result
everywhere, rather than a task opening one shape and a schedule another.
What each pane puts *in* that room is capped, because a line of prose
running the width of a widescreen, or a one-word "Name" input an arm's
length wide, is no more readable than the porthole was: a form stops at
`.pane-form`'s 44rem, the task detail's two columns at 1200px. Both are
left-aligned rather than centered, so the content of an item you open
starts exactly where the list you opened it from started. Dialogs that
are an *action* rather than a thing you opened — New task, Run a suite,
an attempt's transcript — stay centered boxes.

**Settings and System are panes too (grain/task-115).** Those two were
the largest things still drawn as centered boxes, and neither is really
an action: Settings is six tabs of deployment configuration, System is
four panels of live diagnostics, and both are destinations with a URL of
their own (`/settings`, `/system`) that an operator navigates around
rather than a form to fill in and dismiss. The shapes were fighting the
content — a log tail and a sandbox-health table sharing the widest box
`Overlay.jsx` draws, a settings form scrolling its own tab strip off the
top the moment a tab ran long — so both take `pane` now, with the sidebar
still showing beside them. `Overlay.jsx` grows one thing for it, a
`header` slot: chrome pinned above the part that scrolls, so the title
and the tab strip stay put while the open tab's content moves under them.
Panes that open a *thing* (a task, a schedule) pass none and scroll their
whole body as before. Settings caps its tab bodies at the same
`.pane-form` width its forms already used — a "Poll interval" box an arm's
length wide is the porthole's opposite failure, not its fix — while
System deliberately caps nothing, since the tables and log lines in
there are what wanted the room in the first place. The sidebar marks
whichever pane is open the way it already marks the current view:
the rail is on screen the whole time now, so it should say what is
covering the rest of it.

**Metrics is its own destination on the sidebar (grain/task-173).** That
pane's nav entry was renamed from "Debugging" to "Debug" at the time,
and the throughput and latency report is no longer a tab inside it: it
sits beside Settings and that pane, at `/metrics`. Metrics landed there
originally because it is the same *kind* of thing Logs, Sandbox health
and Top are — a read-only, deployment-wide view rather than a knob — and
that is still true. It is not the same *question*, though, and the
question is what a nav entry sorts by. The other three panels are opened
because something is wrong right now: a task is stuck, the machine is
loaded, an agent is failing. They are read while that lasts and closed
when it is over. The report is the opposite: it is opened when nothing
is wrong, on whatever cadence somebody reviews how the deployment is
doing, and the numbers it moves are weekly ones. Filing it under the
word "debug" told an operator to open it at the wrong times, and two
clicks and a tab strip is the wrong price for something read on a
schedule. Splitting it out is also what makes it linkable: `/metrics`
is a URL that goes in a document or a standup note, where "open that
pane, click Metrics" is an instruction. The pane's header carries the
title now, so `MetricsPage` drops the "Metrics" heading it used to print
above its own window picker, and `SystemOverlay` no longer takes an
`onOpenTask` — the one link out of any of these panels (the backlog's
oldest queued task) belongs to the report, and it is App that closes the
metrics pane on the way to that task, since two stacked panes would put
it behind the one the click came from.

**That pane is called System now (grain/task-12).** "Debugging", then
"Debug", named the mood somebody opens it in rather than what is behind
it, and the thing behind it is the machine this deployment runs on: its
logs, its sandboxes, its processes, its power switch. An operator reads
those to see what the deployment is *doing* — is the daemon still
writing, what is the pool holding, what is spending the machine — and
not only once they have decided something is broken. Naming it for the
state it shows says the same thing about it that "Metrics" already says
about the report next door. It is also the one word this codebase was
spending twice: `self-debug` is a capability that lets a run read
grain's own source, and Settings had a "Debug" tab of its own before
this pane moved out of it (bwsalmon/agents#623), so "the debug pane"
named none of the three unambiguously. The rename is the nav entry, the
pane's own heading, `SystemOverlay.jsx` and the path: `/system` rather
than `/debug`. A bookmark to the old path falls back to the task list
the way `/logs` and `/sandboxes` — the two nav entries this pane was
made out of — already do, rather than redirecting; nothing here is
linked from outside the deployment, and `paths.js` has never carried a
redirect for a path it retired.

**A repo is a page of its own, the way a task is (grain/task-111).**
Everything grain knew about one repo used to hang off that repo's row in
the repo list: a chevron that folded its tasks out in place, a "+" that
filed one against it, and New branch, Capabilities, Releases and Remove
buttons -- two of which folded a form out between rows, and every one of
which needed its own `stopPropagation` so it wouldn't also fire the row's
navigation. Five controls per row is a toolbar, not a list, and there was
nowhere for the sixth thing a repo grows to go. So a repo opens the way a
task does. A row is a name and its per-state counts; clicking it lands on
`/repos/{owner}/{name}` (`RepoPage.jsx`, `paths.js` parsing the two
segments a repo name takes), and everything about that repo is on that
page -- its branches and its default capabilities as plain forms rather
than fold-outs behind a button, the way into its releases
(`/repos/{owner}/{name}/releases`), Remove, filing a task against it, and
the repo's own tasks. Adding a repo is the one control that stays on the
list, since it is the only one that isn't about a repo already listed.

The repo's task list is `TaskList` itself, scoped to the repo and passed
in as children by `App.jsx` rather than reimplemented on the page, so a
repo's tasks get the same search, sort, multi-select and drag-reorder the
flat list has instead of a poorer second list. That is also what retired
the task view's `repoFilter` chip: "the tasks of one repo" is a place to
go now, not a filter left standing on another place.

**Everything you open leaves by a back button on the left
(grain/task-177).** Those two halves grew their own way out. A repo's
page and its releases were plain pages, so they took the obvious one: a
"← Repos" button in the top-left corner, above the title. Every pane --
a task, a schedule, a template, a suite, Settings, System, Metrics -- was
a `Dialog` before it was a destination, so it kept the dialog's own X
floating in the top-right. That left one product with two gestures for
the same "go back where I came from", in opposite corners of the screen,
chosen by which of the two you happened to open. They are the same kind
of thing: each fills the content area beside the sidebar, and each has a
URL of its own. So `Overlay.jsx`'s pane shape drops the X for a back
button in the corner a repo page already puts one in, in the flow above
the pane's `header` rather than floating over it -- which also gives a
tab strip back the 3rem of right padding it needed to stay clear of the
old button. `backLabel` names where the button lands for the panes that
only ever open from one list ("← Schedules", "← Templates", "← Suites"),
the way the repo page's own says "← Repos"; the panes reachable from
more than one place -- a task opens from the flat list, from a repo's
page and from the Metrics backlog -- say just "← Back" rather than
naming a destination they cannot know. Escape and a backdrop click still
close a pane, since it is still a `Dialog`. The centered shape keeps its
X: New task, Run a suite and an attempt's transcript are actions taken
over the page you are already on, not somewhere you navigated to, and
there is nothing there to go back to.

**A task list narrows on any attribute a task has (grain/task-288).**
The toolbar could ask two questions -- a word in the title or the id, and
one of four orders -- and the sidebar a third, the state. Everything else
a row shows was visible but not askable: which repo, which base branch,
which capabilities, who filed it, whether a schedule or the merge queue
filed it rather than a person, whether it is a live chat, whether it
merges itself. On a deployment with a few hundred tasks that is the
difference between finding the four `gcp-key` tasks somebody filed last
week and scrolling for them. So `TaskList.jsx` grows a `FILTERS` table --
one entry per attribute, each saying only how to read that attribute off
a task, what to call a value, and what "has none of these" is worth
calling -- and everything else happens once for all of them: the menus
are built from the tasks currently in view, so a menu never offers a
repo whose every task the state filter is already hiding; an attribute
every task in view shares is not offered at all, which is why a repo's
own page shows no Repo menu; and a choice that goes out of range when
the sidebar moves reads as "any" again rather than as a filter matching
nothing. One "Clear" undoes the search and every menu together, since
getting out of six of them one at a time is the same work as getting in.
The sort menu grows state, repo and author alongside the orders it had,
all of them stable, so an order groups the backlog rather than
reshuffling it inside each group -- and dragging a row is still disabled
outside backlog order, for the reason it always was.

The same question is worth asking from a terminal, so `grain list` grew
the same vocabulary (`cmd/grain/list.go`): `-state`, `-repo`,
`-base`, `-capability`, `-author`, `-origin`, `-search`, `-blocked`,
`-auto-merge`, `-interactive` and `-sort`, with the words meaning exactly
what the menus mean -- `-origin schedule` is the Origin menu's
"Scheduled". Three of those are tri-state rather than boolean, since
"only the tasks that merge themselves", "only the ones that need a human"
and "no opinion" are three answers and a Go bool flag is two: `FlagSet.
Visit` is what tells a flag left at its default from one explicitly set
to it. It narrows in the CLI rather than on the server because `GET
/api/tasks` answers with the whole list either way and the frontend
narrows that same answer in the browser -- a query parameter would move
the loop across the wire without removing it from either caller, and
leave two places to look for what a word like "origin" covers. A value
nobody can act on (`-state in_progress`) stops the command with the list
of values that work, before the request, rather than printing an empty
listing that reads like a deployment with no such tasks.

**The same tasks as a Kanban board, with the operator's own columns
(grain/task-287).** The flat list answers "what is the backlog, in
order". The other question asked of a deployment all day -- "where is
everything right now" -- is answered by scrolling one list and reading
eight different state badges off it. So `TaskBoard.jsx` lays the same
tasks out in columns, one card each, reached from `Board` on the rail
and from `/board`.

Two things make it a board rather than a second list. The columns are
the operator's: `BoardColumnsOverlay.jsx` names them, says which states
each collects and what order they run in, and a state in no column is
off the board altogether -- which is how closed tasks stay out of the
way by default, and how "which tasks show up" is answered without a
second vocabulary. And the toolbar over it is the list's own, moved to
`taskFilters.js` so both views narrow by the same attributes with the
same wording rather than growing two drifting copies of one toolbar; the
menus are built from the tasks the columns admit, so a board with no
Closed column never offers a repo only closed tasks carry.

The layout lives in `localStorage`, next to the theme mode
(`ThemeModeContext.jsx`), for the same reason: this is how one person
wants to look at the deployment, not a fact about it -- nothing behind
`/api/config` changes when a column is renamed, and it keeps the whole
feature a frontend change with no settings key or migration behind it.
`board.js` is what makes that safe to read back: a stored layout naming
a state this build has never heard of, claiming one state in two
columns, or holding something that isn't a board at all is normalized or
dropped on the way in rather than rendered.

What the board deliberately does not do is let a card be dragged into
another column. A task's state is derived by the daemon from what has
actually happened to it (`model.StateOf`, `docs/data-model.md`), not
stored as a field the UI could set, so there is no write behind "drop
this in Running"; the gestures that really move a task between these
columns are the ones on the task itself, which is why a card opens the
task. Dragging *within* a column is real, and is the list's own drag --
`Store.Reorder`, in the same "between these two neighbours" terms, and
offered only under backlog order. Cards carry the list's checkbox and
feed the same selection, so the batch-actions bar works from either
view: select a whole column and run it.

**And the third question: what is waiting on *me* (grain/task-20).** The
list answers "what is the backlog", the board answers "where is
everything". Neither answers the one an operator actually opens grain to
ask, which is what grain cannot get on with until they do something.
Five kinds of task are stopped on a person: one parked on an
`ask_question` call, one whose pull request nobody has submitted, one
the merge queue has given up on, one that has burnt through its retries,
and a proposal nobody has approved. They are scattered down a list
ordered by what grain is about to run next, wearing five different
badges, and a deployment left alone for a day accumulates all five.

`Inbox` is the first entry on the rail, above the backlog itself,
carrying a count of exactly those (`state.js`'s `waitsOnUser`, which the
page and the count in the rail share so the number and the page cannot
disagree). `/inbox` groups them by the answer each one needs rather than
by state -- "Answer a question", "Submit for merge", "Unblock a merge",
"Retry or close", "Approve a proposal" -- in the order of how stuck each
kind of wait is, with proposals last because "not now" is so often the
honest answer to one and a backlog of them should not stand between a
reader and a run parked mid-sentence.

What makes it more than a saved filter is that the answer is on the row.
A filtered list still costs an opened pane per task to find out what that
task wants -- Approve lives on one, Submit on another, and a question
needs the timeline at the bottom of a third -- which is a lot of clicking
for the tasks that by definition nobody has got round to. So each row
carries the responses that apply to its own wait, they fire in place and
refresh the list, and none of them opens anything (`App.jsx`'s
`inboxAct`, which is `act` minus the pane it would put over the list
somebody is working down). A parked question opens a reply box under its
own row, with the question it is answering above it.

The one row that gets no reply box is the one that most looks like it
should have one. A run parked on `request_secret` is in the same state
as a run parked on a question, and a reply is a comment -- stored in the
conversation, and handed to the next run in its prompt. A box there
invites somebody to paste a credential into exactly the place grain
promises never to put one, so the inbox reads the task's own detail when
the box is opened, and a task with a `pendingSecret` gets the sentence
saying what was asked for and a way through to the write-only field on
its own pane instead (see "Write-only secrets access when colocated"
below).

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

## The store is a git repository again

grain's settings -- templates, suites, per-repo configuration, prompt
extensions, schedules -- are exactly the kind of thing a task is forever
asking to change, and until now the only way to change one was a human
editing it through the UI. A row in a SQLite file is not something an
agent can propose a diff to, so the mechanism grain already has for
changing anything else (a branch, a pull request, a review, a merge) was
the one mechanism that could not reach grain's own configuration.

So the database is a git repository now: `pkg/staterepo`, a working tree
under `<data-dir>/state-repo` holding every table as
`tables/<name>.json`. The store itself is unchanged -- it is still the
embedded SQLite database `pkg/model/sqlite` opens, and every query in
`pkg/model` is untouched. What changed is that the database is a
materialisation of the repository rather than the thing itself.

Rows are sorted by primary key and columns emitted in the table's own
declared order, so exporting an unchanged database twice produces
byte-identical files. That is not tidiness. The daemon exports on a
timer, and a dump whose row order wandered would commit and push on
every cycle forever, burying the changes that matter.

### One writer, so no merges

grain is the only writer of these files while it runs -- the UI and the
CLI reach the daemon over REST rather than opening the store, which is
what makes that claim true -- so a sync is a commit and a push and never
a resolve, and a pull is fast-forward only. Divergence means something
this model does not describe, and failing loudly beats resolving a
conflict in a database dump by guesswork.

A change an agent makes arrives the other way: a pull request against
the state repository, reviewed and merged like any other, which the
daemon pulls on the same thirty-second timer it exports on. What it does
with what arrives is asymmetric, and deliberately so. Only the settings
tables are imported (`staterepo.SettingsTables`: the deployment's config
row, repo configuration, templates, suites, schedules, qualification
plans), and they are imported wholesale, so a merged deletion of a
template really does delete it. Those are the tables an agent proposes
changes to and the tables grain does not write for itself, which is what
makes them safe to replace live. A merged change to a task or a run is
not applied, and the next export writes the database's own version of it
back out: the database is authoritative for what grain itself did.

A restart imports exactly the same tables, and that is worth saying
because it did not use to. `Load` read "HEAD is not the commit this host
recorded" as "a merge arrived" and replaced the whole state tier --
`task`, `task_comment`, `task_attachment`, `branch`, `release`, every
table but the three churn ones -- from the dump. The dump is whatever the
last export wrote, so that deleted every row grain had written since:
half a minute of them in the ordinary case, and an operator lands in that
window by habit rather than by accident, merging a settings pull request
and restarting grain to make it take effect. Nothing was gained by it
either, since a merged edit to `task` only ever survived if the process
happened to restart before the next tick pulled the same commit down and
recorded it as loaded. The import a restart does is now the import a tick
does.

The whole-database import -- every table, churn included -- is left with
the cases that need it, and what they have in common is that there is no
database ahead of the dump to protect. A working tree with no marker is
one: a clone onto a host that has never loaded it, which is the restore
case, and what makes a clone a whole deployment rather than a settings
file. A database with nothing in it is the other, and it is the same case
reached from the other side -- a store deleted, a volume restored from a
backup that did not include it, `sqlite.Open` handed a path that has
never held one -- where the working tree survived and the marker with it.

That second one used to be the one shape of this that lost data rather
than recovering from it. The marker answers "has the repository moved
since this host last agreed with it" and knows nothing about the
database, so a host whose store was gone read `marker == HEAD`, imported
nothing, came up empty, and the next sync exported that empty database
over the dump, committed it and pushed it -- with nothing further out to
object, because the remote was not ahead: this host really had written
the commit under it. The repository that exists to be the off-host copy
of the deployment was deleted by the deployment. (The paired case was
already handled: `scripts/setup.sh` moves the working tree aside
alongside the store on a schema bump, so the daemon re-seeds. It was the
unpaired one -- store gone, tree kept -- that had no answer.) An empty
database now takes the whole repository, which costs nothing by
definition and is the restore an operator wanted, and the export in that
direction is refused outright and reported in the State pane rather than
committed (`staterepo.ErrDatabaseEmpty`) -- because a database that goes
away underneath a daemon that is already running cannot be restored on
the spot: an import that replaces every row is not something to do with
runs in flight holding the ids, so it waits for a restart. "Nothing in
it" means no row in any table, bar the schema stamp every database has
from the moment it is created; a database with a single task in it is one
this host is responsible for and takes the ordinary path.

If a pull arrives that cannot be applied -- a dump stamped with a schema
this build does not know, or rows that will not insert -- the daemon
stops exporting until it can. Writing the database over those files
would commit a revert of somebody's merged pull request and push it,
which is a worse answer than sitting still with the failure named in the
bootstrap pane.

Schema changes are destructive here, deliberately. A dump this build
cannot read is no more importable than a database it cannot open, so
`staterepo.Load` refuses an older or newer stamp outright rather than
guessing at a migration, and `scripts/setup.sh` moves the working tree
aside alongside the store on a schema bump so the daemon re-seeds it.
The encrypted secrets file is carried across rather than archived with
it: it is the one thing in there that cannot be regenerated.

Not reaching the remote is a different fact, and the daemon treats it as
one. "The repository holds something this build must not overwrite" --
a schema stamp it cannot read, rows that will not import, a history that
diverged and is somebody's to resolve -- stops the process, because
starting would export today's database over it and push that. "The fetch
did not complete" -- a network blip, an installation token that expired
overnight, a repository renamed on GitHub -- says nothing about what the
repository holds, and every deployment on GCP has a remote now, so
exiting over it would take the UI down with the daemon and let systemd
restart both into the same failure. So grain starts on the database it
already has, with the working tree as the last fetch left it: the sync
loop retries every 30s, and the State pane carries the reason until one
works. `staterepo.ErrUnreachable` is what marks the second kind, and
`fatalStateRepoLoad` in `cmd/grain` is where the decision is written
down. The exception is a host with no copy of the repository at all --
`ErrNoLocalCopy`, a fetch that failed with no commits in the working
tree. There is nothing to carry on with there, and seeding one would
commit a history the remote can never fast-forward onto.

### The remote is optional, by construction

A `Config` with no `Remote` is a plain `git init` in the data directory:
commits that go nowhere. A local install therefore needs no GitHub
account, no credential and no answer from the operator at all -- it is
not a degraded mode, it is the default one -- and `SetRemote` attaches a
real remote later without reformatting anything, so "I ran it locally
first" is not a decision anyone has to unmake.

Pushing an https remote authenticates through the same GitHub credential
ladder everything else here uses, resolved per push rather than cached
(an installation token lasts an hour). The token goes into a temporary
askpass script rather than argv or the remote URL: argv is world-readable
through `ps`, and a URL with a token in it would persist in
`.git/config`, inside the very repository being pushed.

### Two clocks, so the repository grows with the data and not with time

Exporting every table on a 30s timer is right for settings and wrong for
the tables grain writes to itself. A reconcile cycle stamps
`task_observation.observed_at` on every task whose pull request it is
watching, every cycle, whether or not anything moved; runs start, finish
and have their transcripts written all day. Nobody sends a pull request
against those rows. Committing them every 30 seconds rewrites the largest
files in the dump forever, and git keeps every version it is ever shown.

Measured rather than assumed (`pkg/staterepo/growth_test.go` drives a
real database and a real git repository through a simulated day of a busy
deployment -- 400 tasks of history, 25 tasks watched per cycle, 300 runs
and 60 new tasks over the day, transcripts at 30 KiB apiece):

| a simulated day                | grain#174 as merged | this build |
|--------------------------------|--------------------:|-----------:|
| commits                        |               2,880 |         84 |
| of those, about settings       |                  10 |         10 |
| `.git` growth, on disk         |             2.9 GiB |  227.6 MiB |
| `.git` growth, once packed     |            18.9 MiB |    9.1 MiB |
| CPU spent in `Sync`            |             43m 07s |     2m 15s |

Every one of those 2,880 commits touched `task_observation.json`; ten of
them were about anything a human would want to read. Three things
changed, and the numbers above are all three together.

**A churn tier on a slower clock.** Three tables -- `task_run`,
`task_observation` and `lease` -- are exported hourly rather
than every 30 seconds (`pkg/staterepo/tier.go` names them and says why
each one is on the list). Everything else, which is everything anybody
reads or reviews, is still exported on every sync and still commits
within 30 seconds of changing. They were not dropped from the dump: a
clone is still a complete restore, just up to an hour behind on runs,
which is the one real cost here and is what the alternative -- leaving
them out entirely -- would have made permanent.

`task_read` was a fourth, and it was a mistake read off the name: it is
not a record of which tasks a human has looked at, it is the read-only
repos a task may clone, written by `PutTask` in the same transaction as
the task row. An hour behind, it made a dump that disagreed with itself
and a restore that handed a task back with no read scope, so it is on
the state clock with `task_grant`, `task_link` and `task_tag`, which are
the rows it belongs beside.

**Packing.** Every commit writes a whole new blob per file it touched, so
a repository committed to on a timer accumulates loose objects that are
each a complete copy of a dump nearly identical to the last one. Nothing
ever packed them. `git gc --auto` now runs after a sync that committed,
at a `gc.auto` threshold set for a repository whose objects are database
dumps rather than source files. On its own that is the 2.9 GiB → 18.9 MiB
column above.

**An import that does not roll grain's own record back.** Since the
database is ahead of the repository on churn by up to an hour by design,
and ahead of it on everything else by up to an export interval for the
same reason, a merged pull request arriving at startup imports the
settings out of it and leaves grain's own record of what it did alone. A
start with nothing to lose -- a clone with no marker, or a database with
nothing in it -- still imports all of it.

Squashing history periodically was the other candidate and is not what
happened: it means force-pushing a branch that people open pull requests
against, which trades a disk problem for a workflow one.

### Secrets are encrypted, and they are not in it

Secrets live under `<data-dir>/secrets`, as `secrets.enc` beside the
private key that opens it. They used to live in the state repository
itself -- one thing to clone, one thing to back up -- and that stopped
being safe the moment a task could be dispatched at that repository (see
"A task can change the settings", below): the git proxy authorizes per
repository and streams a packfile it does not parse, so a sandbox that
may clone the state repository gets every object in it. Ciphertext an
agent can carry off is still ciphertext an agent can carry off.

Nothing is lost by the move. The private key was never in the repository
and is copied nowhere, so an off-host clone could never decrypt anything
anyway; a restore always needed `<data-dir>/secrets`, and that directory
is now the whole of what a restore needs.

It is sealed to a public key whose private half grain reads from one file
under `<data-dir>/secrets` and copies nowhere else. The scheme is X25519
to an ephemeral key pair, HKDF-SHA256 for the
message key and AES-256-GCM over the plaintext, built out of the standard
library: adding a dependency to encrypt one file grain alone ever reads
is a larger commitment than composing three primitives that ship with
the compiler. Neither values nor secret names appear in the ciphertext.

Agents cannot read it, and nothing about this changed that: a run still
gets a secret only through the secret input a human asked for.

An installation written by an earlier build has its copy moved out
automatically, the first time anything opens the secret store. That
removes the file from the tip of the branch and not from the history,
which a clone reads just as easily -- so the git proxy refuses a state
repository that has ever held one to every sandbox, whatever a task's
scope says (`gitproxy.ModelAuthorizer.Forbidden`, wired from
`forbiddenRepos`). Dispatching settings changes at such a repository
means adopting one that has never carried the file.

A fresh install mints its own key, which is what lets a local-only grain
start with no input from anybody. A key that goes missing while an
encrypted file remains is reported as the unrecoverable state it is,
never replaced with a new one -- minting silently there would leave an
undecryptable file behind and look like it had worked.

Which makes that key the one file a redeploy has to carry, and the one
thing about a fresh install that is urgent: nothing else can stand in for
it, and a host restored onto a fresh data directory that mints itself a
new key cannot read a line of the secrets file put back beside it.
`<data-dir>/secrets` restored onto a new host without its key is every
secret grain has in a form nothing on that host can open.

So there are two ways to hand it back, and every deployment uses one of
them. A deploy seeds it the way it seeds every other credential it needs
-- `GRAIN_SECRETS_KEY`, through `scripts/setup.sh`'s own seed-once
contract, pushed into instance metadata on the GCP path alongside the
GitHub PAT and the minter's key. Seed-once rather than converging,
because a key already on the host is the key that host's own secrets were
encrypted to. An operator carrying the key themselves puts it back by
hand instead -- `grain state key import`, `grain state adopt
-secrets-key-file`, or the field in the pane, each reading it from a file
or a form and never from a command line anyone with `ps` can read.

A key that cannot open the file is refused rather than installed, naming
both public halves so an operator holding several knows which one this
deployment wants; a key it replaces is kept beside it with a timestamped
name, because key material is the one thing here that cannot be
regenerated. And where the key is, and whether this host can read its own
secrets with it, is reported where an operator is already looking rather
than left to surface later as a run that could not resolve a credential:
at startup, in the pane, in the installer's readiness summary on every
run, and by `grain state status`, which prints the file and its public
half.

### The bootstrap

Three answers, offered in the UI's Settings pane (its State tab) and by
`grain state` from a terminal: local only, an existing repository whose
contents replace this installation's database, or an empty repository
grain seeds from what it has. The last two are one operation, because
adopting cannot tell them apart up front and does not need to.

Local only is a real answer and the one a fresh install already has, but
it is also the only one with no copy of anything anywhere else, so the
pane warns rather than merely reporting it: every commit in the
repository -- tasks, settings, capabilities, repo configuration -- is on
one disk, and so is the secrets key beside it that nothing can
regenerate. The warning names both moves out, because they cover
different halves: adopt a remote, and keep a copy of the key file off
this host (`GRAIN_SECRETS_KEY` on a later deploy, or the import field in
the pane). `grain state status` has said this at a terminal for as long
as it has existed, and an operator who runs a deployment through the UI
never sees a terminal.

An empty repository is worth formatting before it is adopted:
`grain state format`, in a clone of it, writes the README, the
`.gitignore` and the CI step that validates every later pull request
against it (see "A task can change the settings" below). It is optional
-- grain seeds a repository that has had nothing done to it just the
same, and installs that CI step itself on the first sync after it does.

Adopting is destructive in one direction on purpose -- the repository is
the source of truth, and adopting one means taking its answer -- so the
previous working tree is moved aside with a timestamped name rather than
deleted, and the pane says as much before you press the button.

Nothing has to be carried out of that archived tree. Everything in it is
a materialisation of a database that is still right here, and the one
file that was not -- `secrets.enc` -- lives under `<data-dir>/secrets`
now rather than in the repository, so adopting leaves this installation's
secrets exactly where they were. What an adopted repository can still
need is the key: a deployment restored onto this host from somewhere else
has its secrets sealed to a key that did not come with the tables, which
is what `-secrets-key-file` and the pane's own field are for.

A deployment answers the same question without anyone opening the pane:
`GRAIN_STATE_REPO_URL` (`terraform/gcp`'s `state_repo_url`) writes
`<data-dir>/state-repo.json` from the deploy itself, so a host comes up
pointed at its own state rather than at whatever the last person to open
a browser said. Pointing it somewhere else moves the working tree aside
exactly as adopting does.

### A task can change the settings

The point of all of the above is a settings change that arrives the way
every other change does. `grain create -repo owner/grain-state ...`
files a task against the state repository; the run clones it through the
same git proxy every other dispatch uses, branches, and opens a pull
request against grain's own configuration, which a human reviews and
merges. The next daemon start imports it.

Three things make that work rather than merely be possible.

The prompt says what the tree is. A checkout holding `tables/` beside a
`schema-version` stamp is a state repository and nothing else looks like
one, so a dispatch into it carries a paragraph naming the layout (one
file per table, one object per row, columns in the table's declared
order, rows by primary key), and -- the part no file in there states --
which tables are settings and which are grain's own record of what it
did. The settings list is `staterepo.SettingsTables` itself, not a copy
of it: `template`, `suite`, `schedule`, `repo_config`, the qualification
plan tables and `grain_config`, which is the same list `Apply` imports
into a running daemon, so the prompt cannot call a table settings that
grain will not treat as such. `task`, `task_run`, `task_comment`,
`task_observation`, `lease`, `branch` and `release` are observations, and
a run that edits one produces a diff that is either overwritten by the
next export or merged into a history that then disagrees with what
happened.

The prompt also says *whose* tree it is, which the tree itself cannot. A
dump is the same shape whoever exported it, so `tables/` beside a
`schema-version` stamp marks another installation's state, or a copy
somebody is editing offline, exactly as readily as this deployment's
own; and nothing in an ordinary repo says where the settings an agent
might want changed actually live. Neither is discoverable from inside a
sandbox, where the git proxy serves every repository from one address.
So the deployment answers it: `orchestrator.Config.StateRepo` is
`cmd/grain`'s `stateManager.settingsRepo`, read per dispatch rather than
snapshotted so that adopting a different repository reaches the next run
rather than the next restart, and `settingsRepoSection` tells every
dispatch one of three things — that the repo it is working in *is* this
deployment's settings, so what merges here changes the grain that
dispatched it; that they are in `owner/name` and this is not them, which
is what an ordinary dispatch hears, with a warning of its own for a
checkout that does hold a dump; or, for an installation whose state is
local-only and so has no repository to name, nothing at all. A run
without that fact opens its pull request against a repository that
changes nothing here, or spends its turns looking for settings that were
never in the tree it was handed.

`grain state check DIR` says whether the result will load. It imports
the directory into a database it throws away and reports what broke, by
file and by row. `staterepo.Import` was always the validator -- one
transaction, rolled back whole on any inconsistency -- but the only
thing that ever ran it was a daemon starting up, so a malformed dump or
a row missing a NOT NULL column failed after the merge, on the
deployment. As a CI step in the state repository (`grain state check .`
needs no `-data-dir`, no store and no daemon) it fails the pull request
instead.

grain installs that step itself, so it runs whether or not anybody
remembered to add it. `Seed` and every `Sync` write
`.github/workflows/grain-state-check.yml` when it is not there -- a
repository grain seeds gets it on the way in, and one a merged pull
request dropped it out of gets it back on the next tick -- and the file
is a checkout and one `docker run` of grain's own published image, on
pull requests alone: grain commits to that repository every time its
database changes, and validating those pushes would spend a CI run per
task state change on a dump grain had just written out of the database
the check imports it back into. The image is grain's own container
because grain publishes no bare binaries and that package is public, so
the step needs no credential of any kind.

*Which* of grain's containers is the part a deployment cannot be wrong
about. The check only means anything against a build that knows the same
schema as the deployment — `grain state check` refuses a dump stamped
with any other, and says so in those words — so a workflow pointed at
the tag that follows main is right for a deployment tracking main and
wrong for every other one: a deployment held at an older tag got a check
that failed every pull request against its own settings for a reason
that had nothing to do with the change proposed in it, until an operator
noticed and pinned it by hand. So the deployment answers it, out of the
same trick that tells it which sandbox to run: the grain image carries
its own reference, stamped in at link time
(`cmd/grain/grainimage.go`, the Dockerfile's `GRAIN_IMAGE_REF` build
arg, `grain image` to print it), and that is what it writes into the
workflow. An unstamped build — `make build` on a laptop — falls back to
the tag CI keeps pointed at main, which is a less precise answer than
the stamp and a much better one than none.

Unlike the README, which is grain's text and is rewritten on every sync,
a workflow somebody has edited is never touched again. The rule that
makes that safe is byte equality: grain only touches a file that is
still word for word one of its own renderings, so a runner, a trigger or
a step of somebody's own hands the whole file back to whoever wrote it,
image included. A file grain rewrote every thirty seconds would be a
file whose editor is fighting a timer, which is the same reason the
export must not fight a hand edit to a table file. Deleting it is not an
opt-out, because grain writes back what is missing: `"noWorkflow": true`
in `state-repo.json` is, and `"checkImage"` pins the image from the host
side — grain then writes and maintains that value instead of its own, so
a pin made there is one a later sync does not take back.

What grain does maintain in a file nobody has edited is the image line,
which it keeps in step with the build it is running. That line is a fact
about the deployment rather than about the repository, and it goes stale
on its own every time the deployment is upgraded, so a sync after an
upgrade repoints it on a one-line commit of its own and the syncs after
that have nothing to do.

Byte equality against *this build's* template alone would have made that
promise good only until the next time the template was reworded: a
comment edited here and every file grain had already installed stops
being recognised, keeps whatever image it was installed with, and is
never maintained again — no worse than a grain that never repointed
anything, but a fix that reaches no deployment that already exists. So
`pkg/staterepo/workflow_history.go` keeps the renderings earlier grains
wrote, and a file matching one of them word for word is adopted: grain
replaces it with this build's wording, on one commit that says as much,
and maintains its image from then on. The whole file rather than its
image line, because the comment block of an older rendering describes
rules grain has since changed. Nothing is loosened by this — each entry
is still an exact match against text grain itself wrote, and a file with
one character of anybody else's in it matches none of them — and the
list only grows: a template edited here goes into that file, and
`TestTheCurrentTemplateIsRecorded` fails until it does, because the
alternative is stranding every repository already carrying the old text
with nothing to say so.

That leaves the reason grain did not commit this file until now, which
is real: a push adding a file under `.github/workflows` is refused
unless the credential making it may write workflows, and grain's own
installation token need not be able to. So the workflow is a commit of
its own, pushed on its own, and a refusal -- GitHub says "refusing to
allow ... to create or update workflow" -- is undone in full: the commit
is dropped, the file put back as it was — removed when grain had just
written it, restored when what was refused was a change to a check the
repository already had — the export goes on untouched,
and the journal says to run `grain state ci` in a clone and commit the
file with a credential that may. grain tries again a day later, so
granting the permission needs no restart. A local-only repository gets
no workflow at all: there is no GitHub to run one.

The Settings pane's State tab says so too, and not only the journal. The
refusal is not a sync failure -- everything about that deployment is
working, one file aside -- so a pane that reported only failures would
show a repository syncing perfectly happily with nothing validating a
change to it, which is the failure the check exists to prevent, and the
line naming `grain state ci` would have been seen only by whoever was
reading the journal that minute. It says when grain was last refused,
names the file, and offers the same two ways out: install it by hand, or
set `"noWorkflow": true`. It stops saying it the moment the file is in
the repository, however it got there.

`grain state status` says it too, out of that same marker rather than out
of anything of its own, so the terminal and the pane cannot come to
describe one repository differently. It is the same question asked by
whoever is on the host, and without the line every other one that command
prints -- the remote, the head, the schema, the dispatch verdict -- is
word for word what a repository whose check runs on every pull request
prints.

`grain state ci DIR` is that manual path, and the one for a repository
whose deployment cannot push workflows. It writes the same file into a
clone of the state repository, with `-image` to pin (defaulting, like
everything else here, to the image the binary running it was published
as) and `-force` to replace one that is already there, and commits
nothing: it runs in somebody's own checkout, and the commit is theirs to
make.

`grain state format DIR` is the step before adopting: an operator has
made an empty repository on GitHub and cloned it, and this lays out the
parts of a state repository that are not the dump -- the README that says
what the tree is, the `.gitignore` that keeps a stray key out of it, and
that same CI step. It writes no `tables/` and no `schema-version`, which
is what keeps the bootstrap's two cases apart: a repository with a dump
in it is one adopting *imports over the database*, so a format that wrote
an empty dump would turn "grain seeds this empty repository from what it
has" into "this empty repository replaces grain's state", and an operator
who formatted one and then adopted it would have emptied their own
deployment. It refuses a directory that already holds a dump and points
at `grain state ci`, which is the command that has something to do there.

And the encrypted secrets file is not in there any more, for the reason
the section above gives: a repository a sandbox may clone is a
repository a sandbox may read whole.

### A merged change is not something to commit over

The export runs on a timer, so the daemon is writing to the repository
while a pull request against it is open. A tick pulls before it exports,
and that is usually the whole story: the merge arrives, its settings go
live, and the export commits on top of it. This is about the tick where
the pull does not get that far -- a history that will not fast-forward,
or a merge that lands in the seconds between the pull and the push. The
remote then holds a commit this deployment does not, and grain committing
its own dump on top of that used to strand the installation: the push
rejected as a non-fast-forward on that tick and on every tick afterwards,
and a next start that finds the two diverged and refuses to load at all.
A grain that will not come up because somebody merged the change it asked
for is a bad answer to the mechanism this whole repository exists to
allow.

So the export asks the remote first, which costs one `ls-remote`, and
declines to run at all while something is waiting. The database is still
the live state and grain goes on running against it, unaffected; what is
waiting is loaded at the next start, whole, the way every other thing a
tick could not apply is. The pane says so in those terms -- a merge to
load and a restart to load it with -- rather than showing a git error,
and the journal says it once rather than every thirty seconds.

### A divergence grain made is a divergence grain can clear

Asking first closes the window, and does not close it completely: grain
commits its export and then pushes it, and a push that fails -- an
installation token that expired between the two, a remote that was
briefly unreachable -- leaves the commit on this host. If a pull request
is merged before that push is retried, the remote holds a commit on the
other side of the same parent. `Pull` is fast-forward only and refuses,
naming both branches, rather than resolving a conflict in a database dump
by guesswork.

That refusal is right and nothing used to clear it. Every tick logged the
same divergence, the pane showed it, no merged change was ever applied
and no export ever reached the remote until an operator went to the host
and fixed the working tree by hand. Before the daemon pulled on its own
timer that was a once-per-restart problem; afterwards it was a permanent
one, and a start that hit it did not come up at all.

There is exactly one case grain can resolve without deciding anything on
anyone's behalf, and `RecoverDiverged` (`pkg/staterepo/diverge.go`) is
it: every local commit the remote has not got is grain's own export --
grain's author, and a diff touching only the files an export writes
(`tables/`, the schema stamp, the README, the `.gitignore`). Those
commits hold a dump of a database that is still sitting right here, so
resetting the working tree onto the remote's branch loses nothing by
construction: the settings that were merged are applied, the database is
exported again on top of them, and both directions are moving again
within one cycle. It runs from the sync loop and from the load at
startup, so a restart is not the fix and is not made worse by being one.

Everything else stays a refusal. A commit somebody made by hand in the
working tree might hold something that exists nowhere else, and a merge
commit is somebody's earlier resolution; neither is grain's to throw
away, so the divergence is reported with the commit and its author named,
and the pane says the thing an operator actually needs to hear -- that
this deployment has diverged from its remote and is not syncing -- rather
than leaving it to be read out of a git error.

### The copying itself is soaked rather than reasoned about

Everything above is a rule about *when* rows move. What was missing was a
test of the thing underneath all of them -- that a value written into the
database and read back out of a clone is the same value -- and of what
happens when the rules meet each other in an order nobody chose.

`pkg/staterepo/soak_test.go` is that: rounds of tasks filed, runs started
and finished, observations stamped, settings edited in the UI, pull
requests merged against the repository (a template retitled, added,
deleted, a file that is not part of the dump), pushes that fail and
strand grain's own export on the host, merges landing on top of those so
the two diverge, restarts taking the daemon's own startup path, the clock
crossing a churn interval, and restores onto a fresh clone with a fresh
database. Every round it checks that the working tree is clean, that
grain has made no merge commit, that the dump names no task it does not
have, that every task ever filed is still in the database with the repos
it may read, that the settings live are the ones last merged, that the
repository's state tier is the database's, and that an export of an
untouched database commits nothing. At the end it restores the remote
into an empty database and compares the two row by row -- and storage
class by storage class, since SQLite types values rather than columns and
a round trip that turned the text `42` into the integer 42 would compare
equal on the value alone.

Sixty rounds run on every commit; `make soak` turns it up to a couple of
thousand. It found, or would have found, four things that each produced a
repository that looked right and handed back a database that was not the
one exported: an export that was not a snapshot (a table read per
statement, so a task filed between two of them left a run in the dump
whose task was not there), text that was not valid UTF-8 silently
replaced with U+FFFD by `encoding/json`, a start that recovered a
divergence importing the remote's older dump over everything the failed
pushes had stranded in the database, and `task_read` -- the repos a task
may clone, not a record of what a human has read -- exported on the churn
clock, an hour after the task it belongs to.

## Deployment configuration lives in the store too

bwsalmon/agents#320 asked the same "the store is the record" question
"Input is a model update, not a GitHub issue" (above) already answered
for tasks, aimed at the daemon's own flags this time: `-max-workers`,
`-poll-interval`, `-gemini-model`, `-gemini-effort`, `-claude-model`, `-max-agent-turns`, `-github-host`,
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
already live. Unset, all three print as the shape actually in effect —
grain's own default, carried alongside the stored value as
`sandboxCpusDefault`/`sandboxMemoryMbDefault`/`sandboxDiskGbDefault` —
rather than as the bare `0` that is stored, since a literal `0` reads as
a deliberately empty VM (see "Every sandbox is built at a size grain
chose", which is where disk got a default to print).

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
`RunCycle` re-reads `max-workers`/`max-mergers`, `max-agent-turns` *and*
`prompt-extension`;
`dispatchConfig` re-reads `agent-framework`, `gemini-model`,
`gemini-effort` and `claude-model` when a run's framework is built (which is per dispatch,
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

### What time it is here

`model.Config.TimeZone` (grain/task-368) is the wall clock a deployment
keeps — an IANA zone name, defaulting to `America/Los_Angeles` and
backfilled to it for a deployment upgrading across the column. Before it
existed there was no such answer: the daemon runs in a container with no
zone of its own, so every wall-clock computation was done against UTC. A
schedule set for "daily at 09:00" fired at two in the morning where its
operator is, and every timestamp the UI printed was in whatever zone the
*browser* was in — a different clock from the one the daemon was firing
against, so the two could not be compared.

Both halves read the one setting now. `model.Recurrence.Next` takes a
`*time.Location`, which `orchestrator.reconcileSchedule` resolves once
per cycle from the `grain_config` row `RunCycle` already refreshes — so
changing the zone retimes the next occurrence of every schedule rather
than waiting for a restart. A zone name rather than a fixed offset,
because the point is that a schedule keeps its wall-clock time across
daylight saving instead of sliding an hour: `nextDaily` walks whole days
on that calendar (`AddDate`, which preserves the time of day) rather
than adding 24 hours. `RecurrenceEveryNHours` is the deliberate
exception — a duration between instants is the same duration in every
zone, which is exactly how "every 24 hours" differs from "daily at
09:00". `pkg/model` imports `time/tzdata`, so a zone resolves the same
inside a slimmed container image as on a laptop.

The frontend reads it off `GET /api/config` (like `environmentName`
above, and for a stronger reason: every screen prints times from the
first paint), hands it down through `TimeZoneContext`, and formats every
absolute time through `ui/src/time.js` against it. Settings' General tab
is where it is chosen, from `Intl.supportedValuesOf("timeZone")`, and the
schedule form's time field is labelled with the zone and its current
abbreviation rather than the flat "UTC" it used to claim.
`ui.UpdateSettings` validates the name against the zone database this
build carries, so a typo is refused while whoever typed it is still
looking at it rather than leaving the pane showing one clock and the
daemon firing on another.

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

### Debugging `gemini-key`: the picker didn't know what Settings knew

"The gemini key capability fails when I attempt to add it to a task."
Attaching it is not what fails — `POST /api/tasks/{id}/capabilities`
writes the grant and returns the task, exactly as it should. What fails
is the task, at its next dispatch, and the reason it fails is one of
three things that were each invisible from where the mistake was made.

`gemini-key` needs a GCP project (`capabilityProviders` registers no
provider without one, and a grant nothing answers to is refused as `no
provider is registered for capability "gemini-key"`), a
`gcp-key-minter` secret to mint under, and a minter holding
`roles/serviceusage.apiKeysAdmin` in a project with
`apikeys.googleapis.com` enabled. Settings' Capabilities tab already
reported the first two, as **Not ready** with a `Needs:` line. The
capability picker on a task — the pane where somebody actually ticks the
box — was built from `ui.OfferedCapabilities`, a static listing with no
deployment behind it, and offered the row regardless. Two panes, two
independent answers, and the disagreement only surfaced minutes later as
a failed run.

`GET /api/config` now carries `ready` and `needs` per picker row, joined
to the same `capabilityStatuses` Settings is built from rather than
re-derived, so the two cannot drift into saying different things again.
The row warns and stays tickable: filing the task first and pasting the
secret second is an ordinary order to do things in, and a picker that
refused would also leave a capability already attached with no row to
untick it from.

The third gap is the one no configuration pane can see, because it lives
in GCP. `grain setup gcp` defaults `-enable-gemini-key` to *off* while
`terraform/gcp`'s own `enable_gemini_key` defaults to *on*, so a
deployment installed by script rather than by that module has a project,
a minter credential, a **Ready** badge — and a minter that cannot
administer API keys. The API answers both that and "this API was never
enabled" with an indistinguishable 403, whose message (`Permission
'apikeys.keys.create' denied on resource ... (or it may not exist)`)
reads like a bug in grain rather than like one unrun setup flag.
`geminikey.advise` now names which of the two it is — from the error's
own `SERVICE_DISABLED` reason — and names the command that fixes either.

Two smaller things fell out of writing a fake API Keys server to test
that. `apiKeysMinter`, the code that actually runs the moment somebody
attaches this capability, was covered by nothing but the live test that
skips without a real GCP project; it has real tests now, for the
long-running-operation polling, the request paths and the cleanup after
a key that is created but unreadable. And an empty key string coming
back from `GetKeyString` was being placed at `KeyPath` and described to
the agent as a working key — the one failure here that looks like
success, now a failed mint like any other.

`Resolve` also refuses when the standing credential resolves to nothing,
rather than leaving `Materialize` to discover it a moment later. Nothing
new is caught: what changes is that a refusal's `Reason` is posted to
the task verbatim, so an operator reads a sentence naming the secret to
set, where a failed materialize reads as `materializing capabilities:
geminikey: resolving credential ...` — grain describing its own
internals.

There is a fourth gap no configuration pane can see, and it is the one
"Debugging `gcp-key` again" below is about: `gemini-key` mints through
the *same* standing minter credential `gcp-key` does
(`cmd/grain/daemon.go` hands `geminikey.New`
`gcpkey.DefaultMinterCredential`), so a minter key deleted or rotated
away in GCP breaks every Gemini mint in exactly the same way, as
`invalid_grant` / "Invalid JWT Signature" from Google's token endpoint.
That failure arrives before any request reaches
`apikeys.googleapis.com`, carrying no `googleapi.Error` at all, so
`advise` — which classifies a 403 the API answered with — never sees it.
`geminikey` has its own `isCredentialRefused` and
`explainRefusedCredential` now, deliberately word-for-word with
`gcpkey`'s past the name of the API nothing reached, so an operator who
has read one of those sentences recognises the other. The mint, the
revoke, the hourly reap and `MintOperatingKey` all go through it — that
last one runs during a deploy (`scripts/setup.sh`'s
`mint_gemini_operating_key`), where the reader of the failure is a line
in a deploy log with no task and no Settings pane to look at.

### Debugging `gcp-key`: a diagnosis that was confidently wrong

"Attempting to add a gcp key fails tasks." Same shape as `gemini-key`
above, and the picker half of that fix already covers this capability —
`GET /api/config` carries `ready` and `needs` for every row, and
`gcp-key` needs one thing more than `gemini-key` does, an agent service
account for keys to be minted *for*, which `capabilityProviders` also
gates on. What was left was everything this capability says once it has
been ticked, and each of those sentences was wrong in a different way.

`Resolve`'s refusal — the text posted to the task verbatim, which is the
whole reason a refusal carries prose rather than a code — told an
operator to run `grain controller configure
--gcp-agent-service-account-email <email> --gcp-project-id <project>`.
No build of grain has ever had that command or those flags; the real
ones are `grain settings -gcp-project -gcp-agent-service-account`, and
the same pair of fields sits on Settings → Capabilities. It opened by
telling them their *issue* carries a label, from a version of grain with
neither issues nor labels left in it. And it never fired for the missing
`gcp-key-minter` secret at all: that arrived a moment later as
`materializing capabilities: gcpkey: resolving minter credential ...`,
grain describing its own internals in the place a task's own comment
should have named the secret to paste and the pane to paste it into.
`Resolve` now refuses for that too, the way `geminikey.Resolve` already
did.

The one no configuration pane can see is in GCP, and here grain was not
merely unhelpful but confidently wrong. Creating a service-account key
can fail as a `FAILED_PRECONDITION`, and `explainCreateFailure` answered
*every* one of those with the key-quota explanation
(bwsalmon/agents#140, where a per-retry leak really did fill a project's
10-key cap in minutes). The far more likely cause on a project set up
since is the organization policy
`constraints/iam.disableServiceAccountKeyCreation`, which forbids
user-managed service-account keys outright and is enforced by default in
organizations created since 2024. An operator whose org forbids keys was
being told — with a count of **0** in the same sentence — that keys were
being created faster than grain releases them, and sent looking for a
leak that does not exist, in the one case where nothing about grain can
help. The key count decides it now: the quota explanation is given only
when the account is actually at the cap, and otherwise the constraint is
named. The three other ways a half-set-up project refuses a mint get
their own sentence too — `iam.googleapis.com` never enabled, a minter
holding no `roles/iam.serviceAccountKeyAdmin` on the agent account, and
a service account email that names nothing in this project — each with
the command or the pane that fixes that one, and each wrapping GCP's own
message rather than replacing it.

`iamMinter`, like `apiKeysMinter` before it, is the code that runs the
moment somebody attaches this capability and was covered by nothing at
all; it has tests now, against a fake IAM API. They found the same
looks-like-success failure the Gemini side had: a key created with no
private key material was base64-decoded to nothing, placed at
`SandboxKeyPath`, and described to the agent as a working key. It is a
failed mint now, and the useless key is deleted rather than left to
count against the very cap above.

### Debugging `gcp-key` again: the deploy rotated the key out from under the host

"Attempting to add a gcp key to a task fails to provision the task."
The same sentence as last time, a different failure underneath it, and
this one was never grain's classification of a GCP error — it was the
deployment invalidating its own credential on a schedule.

```
materializing capabilities: model: materializing capability "gcp-key":
gcpkey: minting a key for projects/…/serviceAccounts/grain-main-agent@…:
gcpkey: minting a key for projects/…/serviceAccounts/grain-main-agent@…:
Post "https://iam.googleapis.com/…/keys?alt=json": auth: cannot fetch
token: 400 Response: {"error":"invalid_grant","error_description":"Invalid
JWT Signature."}
```

`invalid_grant` is Google's token endpoint refusing to exchange the JWT
`gcp-key-minter`'s stored key signed, which means Google no longer holds
the public half of that key. Nothing in the message says so: it names the
*agent* account being minted for, never the minter credential doing the
minting, and it arrives before any request reaches `iam.googleapis.com`
— so it carries no `googleapi.Error`, no status code, and none of
`explainCreateFailure`'s four explanations fire. It is also, as printed,
the same forty characters of resource name twice, because `iamMinter`
re-labelled every failure with the context `Materialize` had already
wrapped it in.

**Where the key went is `terraform/gcp/deploy/push-secrets.sh`.** It
mints a *fresh* minter key on every run, pushes it into instance
metadata, and then deletes every key on the minter account beyond the
newest two — a deliberate rotate-and-prune, ported from v1. The other
half never held up its end: `scripts/setup.sh`'s `seed_gcp_minter_key`
returned early whenever `grain secrets list` already showed a
`gcp-key-minter` entry, on the same never-overwrite rule every plain-file
secret beside it follows. So the host seeded its copy on the first deploy
and refused every replacement afterward, and the third `push-secrets.sh`
run deleted, in GCP, the key the daemon was still authenticating with.
The rotation was not a safety measure that happened to be inconvenient;
it was a countdown. The documented remedy was to delete the entry by hand
over IAP and bump `deploy_generation` — a manual step to repair a state
the deployment created for itself.

The seed converges now: a key handed to `setup.sh` is the key the daemon
ends up holding, every run, so a rotation lands on the next deploy the
way every other pushed secret does. The one early return left is for a
deploy carrying no key at all, which still leaves an operator's own
`grain secrets set` alone. It writes to whichever key the secret already
holds rather than always to `key.json`, for the reason "Secrets sit with
what uses them" below gives from the other side: `Resolve` answers the
bare `gcp-key-minter` name only while the secret holds exactly one key,
so a fixed name would break a secret first written from Settings (as
`value`) in a second way while fixing the first.

**And grain says which secret is dead when it happens**, since a key
deleted in GCP by anything at all lands here, not just this deployment's
own rotation. `isCredentialRefused` matches the token endpoint's body,
and `explainRefusedCredential` — on `Provider`, the only thing that knows
which secret the material came out of, and that `Revoke` authenticates as
whatever the *lease* names rather than what `Config` says today — names
that secret, the ways a stored key stops being one GCP will accept, and
the two places a current one is pasted. `Materialize`, `Revoke` and
`Reap` all go through it: a dead minter credential breaks releasing and
reaping a key exactly as it breaks minting one. This is the third thing
about `gcp-key` that no configuration pane can see — the tab reads
**Ready** throughout, because the secret is set and only GCP knows the
key inside it stopped working.

### Testing a credential, from the pane that calls it Ready

That last sentence is what task-172 is about. Every fix above improves
what an operator is told *once something has already failed*; none of
them lets anyone find out before a task does. **Ready** on Settings →
Capabilities means configured — a project, an agent service account and
a `gcp-key-minter` secret are all set — and presence is the whole of
what a configuration pane can see. So a capability row now carries an
action as well as a badge: `POST /api/capabilities/{id}/check`, which
authenticates as that capability's standing credential, makes one cheap
and harmless call with it, and reports what came back.

**The check is each provider's own**, through `model.CredentialChecker`
— an optional interface shaped exactly like `model.Reaper`, implemented
by the three capabilities that hold a standing credential and skipped by
the ones that hold none. `gcp-key` lists the agent account's own keys
(`Minter.ListKeys`, which `Reap` already calls hourly and which needs no
permission minting does not); `gemini-key` lists the project's API keys,
which also answers the pair of 403s `advise` exists to tell apart — a
minter that may not administer API keys, and a project where
`apikeys.googleapis.com` was never enabled — on a deployment whose
minter key is perfectly live; `github-sandbox` asks GitHub which
installation its App has, the same `FindInstallation` every
`Materialize` starts with. None of them mints, writes or deletes
anything, because this is a button somebody is expected to press twice.
Doing it three ways rather than one is deliberate: only the provider
knows which call is cheap, which is harmless, and which sentence to
answer a refusal with — `explainRefusedCredential`'s, naming the dead
secret and the two places a current key is pasted, rather than Google's
bare `invalid_grant`.

**On demand, not on a timer.** A background poll would keep the badge
itself truthful, which is the more appealing design right up until the
costs are written down: a request to somebody else's API per capability
per tick, forever, whether or not anyone is looking; and a **Ready**
that changes with nobody touching Settings, so a badge found red carries
no answer to "compared to when, and did anything here change?". A button
costs one round trip a human asked for and gives an answer stamped with
the moment it was true. The answer is deliberately not stored and not
folded back into `Ready`: a reload clears it, because a remembered
"checked ok" is the same unfalsifiable reassurance the badge alone
already was.

**A refusal reads as this deployment's, not as grain's.** It comes back
as an ordinary 200 with `ok: false` and the provider's sentence, because
a configured deployment whose credential the far end refused is a real
answer to the question asked and has a remedy on this pane — the same
reasoning that keeps `Grantable` reported beside `Ready` rather than
folded into it, one naming a gap configuration fixes and the other a gap
it cannot. The errors that *are* grain's — an unwired UI, an id no build
knows, a capability with nothing standing behind it — stay errors, 404
and 400.

`CapabilityStatus.Checkable` is what decides whether a pane offers the
action at all: grain ships a check for this capability *and* this
deployment is wired to something that can run one (`ui.Config.
CapabilityChecks`, the same nil-means-unavailable contract every other
optional field there has). Two drift tests hold the two halves together
— every capability with a `Requires` entry owes a checker, and every
capability without one must not claim to have a check — so a fourth
capability that grows a standing credential cannot quietly go back to
being a **Ready** badge and nothing else. `grain settings
-check-capability <id>` asks the same question from the host, which is
where whoever is reading a failed task's error is usually already
standing.

### The question no button can ask: standing something up

The check above is deliberately cheap and harmless, and that is also its
ceiling. `gcp-key`'s is `Minter.ListKeys` — it proves the minter
credential is alive and that GCP will still talk to it. It does not
prove the *agent* account can create a VM, and those are different
questions with different answers: the roles are different, the APIs are
different, and an org policy or a dropped role binding can move one
without touching the other.

**Two scripts answer the larger question, and for a while nothing ran
them.** `scripts/gce-vm-smoke.sh` creates a throwaway VM, ssh's into it
and deletes it (about ninety seconds); `scripts/gke-cluster-smoke.sh`
creates a cluster, runs a workload on it, resizes the node pool, relabels
it and deletes it (about twelve minutes, a control plane and two
`e2-medium` nodes). Both pass against a real project as
`grain-main-agent`, and both are worth more than any assertion about
configuration, because they exercise the same three verbs
`terraform/gcp` and a `gcp-key` sandbox do. They also only ever answered
when a human typed them, which is reliably *after* something has already
failed in a way that reads as something else — the two `gcp-key`
debugging sections above, twice over.

**`.github/workflows/gcp-smoke.yml` runs both, nightly.** Nightly rather
than weekly for `live-agent.yml`'s reason: what this samples drifts on
somebody else's schedule — an org policy lands, a role binding is
dropped, `constraints/iam.disableServiceAccountKeyCreation` gets
enforced, a quota changes — so a longer interval buys nothing but a
longer window in which every `gcp-key` task fails for a reason nobody has
been told. A few cents a night, in a repository whose `tests.yml` already
spends a 45-minute KVM job on every pull request.

Two other homes were weighed. **Not a preflight in
`terraform/gcp/deploy`**, which is the only shape that would *block* a
deployment about to fail anyway: it would put twelve minutes in front of
an interactive deploy to re-answer a question a few hours old, and it
would answer it with the operator's own credential rather than with the
agent account whose drift is the actual risk. **Not a grain schedule
dispatching a task** either, tempting as it is to have grain watch its
own credential: the answer would land as a task in the queue rather than
as a red check, and a deployment whose `gcp-key` has stopped working is
exactly the deployment that cannot dispatch the task that would say so.

**Where the credential lives is the same boundary `live-agent.yml`
draws**, and for the same reason — most branches here are pushed by
grain's own agent runs, so a workflow file on a branch is code no human
has read, and a *repository* secret is readable by any workflow on any
branch push whichever file declares it. So a maintainer creates a
`gcp-smoke` environment whose deployment branch policy names the default
branch alone, holding `GCP_KEY_MINTER_KEY` as a secret and `GCP_PROJECT`
and `GCP_AGENT_SERVICE_ACCOUNT` as variables. Until all three exist the
"Require a credential" step fails every night with the instructions in
the error, on purpose: it blocks no merge, and a job that skipped quietly
would be this same gap in a new place.

**It mints its own key rather than being handed one**, which is the part
worth arguing. The obvious arrangement is a standing key for the agent
account pasted into a secret; this authenticates as the same minter
`pkg/capability/gcpkey` uses, mints a key for the agent account exactly
the way a `gcp-key` grant does, and gives it back at the end of the run.
That is what makes key creation itself part of what is sampled: the day
`constraints/iam.disableServiceAccountKeyCreation` is enforced, every
`gcp-key` task starts failing, and a job holding a static key would sail
through it and report green. It also keeps CI's copy of the agent's power
to the length of one run — and if the revoke never happens because a
runner died, `gcpkey.Provider.Reap` deletes agent keys past
`DefaultMaxKeyAge` on the deployment's own hourly sweep and does not care
who minted them.

**The `actAs` question is exposed rather than inherited.** Both scripts
default to the self-acting shape — no service account on the VM, the
calling account on the nodes — because the agent account has
`iam.serviceAccounts.actAs` on itself alone, and attaching any other
identity (including the default compute account gcloud reaches for on its
own) needs `roles/iam.serviceAccountUser` **on that account**. That is
what a real `gcp-key` sandbox can do, so it is what the nightly run
samples. A `workflow_dispatch` input runs either leg the
production-shaped way instead, which fails until somebody makes that
grant — being able to ask on demand is the point, and making the nightly
depend on a grant nobody has made yet is not.

**Cleanup is the workflow's, not the scripts'.** Both delete what they
made on every path out of themselves, Ctrl-C included, but a bash `EXIT`
trap does not run when the runner kills the shell — which is what a
cancelled job or a blown timeout does, and a leaked cluster bills all
week. An `always()` step deletes anything carrying the scripts' own
labels on either of two tests: created after this run minted its key, so
it is this run's and goes now rather than billing until tomorrow; or more
than two hours old, which no run of this job (about fifteen minutes) ever
is, so that bound only ever decides about somebody else's leftovers — and
is long enough not to take a cluster a human is standing in front of with
`--keep`. By label rather than by name, so tonight's run clears last
night's leak.

`tests/deploy` holds the arrangement rather than trusting it — the
trigger (no `push`, no `pull_request`), the `environment:`, both
cleanup steps, and that the workflow names scripts that exist, are
executable, parse under `bash -n`, and take the flags it passes them.
What no test here can check is the one thing the workflow is for, which
is why it is a schedule and not an assertion.

### The fourth credential: a named GitHub token

task-172 stopped at three, and named the one it left out: a named GitHub
token (`github-credential:<name>`). Those rows are reported `Ready` by
construction — a row exists only because an operator's own file under
`secrets/github` does — and a token revoked, expired or rotated at
GitHub's end changes nothing about that file, so the pane went on
agreeing with itself while the first symptom was a push failing through
the git proxy, mid-run, as a sandbox's own error. task-189 is that
fourth check, and the two questions it turned on are worth recording.

**What it authenticates with, given the provider holds no client.** A
named token is a SELECT capability: it mints nothing, places nothing,
and during a dispatch resolves no credential at all — the proxy looks
the material up per request. So unlike the other three there was no
client to reuse, and the choice was between teaching the provider to
read credential files itself and handing it the thing that already
does. It is handed the ladder: `githubtoken.Config.Credentials`, a
one-method interface over `gitproxy.CredentialSet` (the adapter lives in
`cmd/grain/daemon.go`, beside the one that already adapts the same
ladder onto `github.TokenSource`). That is what keeps "what does this
token authenticate as" answered in exactly one place, and it is what
makes the check test the material a push would actually carry — a
`<name>.token` read as-is, or a `<name>.app.json` whose installation
token the ladder re-mints, which are two different auth flows a check of
its own would have had to learn separately. It is also why the UI now
shares run()'s ladder rather than loading a copy: the Settings pane that
writes a token is what makes a ladder forget the old one, so a check
through any other copy would go on testing the token an operator had
just replaced on that very pane.

**What the cheap call is.** `GET /rate_limit`, then one `GET
/repos/{owner}/{repo}` per repo this deployment targets.
`/rate_limit` is the cheapest live-or-dead answer GitHub gives — it
costs nothing against the limit it reports — and unlike `GET /user` it
is answerable by both forms of credential, since an App installation
token has no user. But "the token is alive" is rarely the interesting
failure: a token that is alive and has lost access to the repo it exists
to push to fails in exactly the same place a dead one does, and
`permissions.push` on the repo lookup is the difference between "it can
still push there" and "it can see it and nothing more".

**Repo reach is evidence, not the verdict — with one exception.** A
named token is deliberately narrower than the deployment default; that
is what it is for. Failing the check for a repo it was never meant to
reach would paint a correctly-scoped token permanently red, so what it
cannot see is reported in the detail beside what it can. The exception
is a token that can reach *none* of them: it is live and useless for
anything any task here could ask of it, which is the same news to an
operator as a dead token, and it is reported refused in the same words.

`Ready` keeps meaning what it meant. It is still true by construction
for these rows, because the two gates it asks about — a deployment
setting, a secrets-store entry — are gates a GitHub token has neither
of; what changed is that `Checkable` is now true beside it, which is
task-172's own rule that a checked-and-refused credential is reported
next to **Ready** and never folded into it. `pkg/ui`'s drift tests grew
the matching half: the catalog test cannot see these rows, since which
tokens exist is not a property of any build, so a second test holds the
named-token listing to the same bar — the provider must implement
`model.CredentialChecker`, and the row must report itself checkable, or
the pane offers no way to reach it.

### The same set, per repo

The ask task-14 came from also said "we will also want this to be
possible on individual repos in the future," and this is that: a repo can
name capabilities of its own that a task filed against it starts holding,
on top of whatever the deployment already defaults.

**Where it is stored is a new `repo_config` table**, keyed `(owner,
name)` the same way `qualification_config` already is, holding
`model.RepoConfig` — `DefaultCapabilities`, with the
same comma-separated storage `grain_config.default_capabilities` uses,
and since grain/task-114 `PromptExtension` beside it ("Standing
instructions: the prompt extension" below).
A new table rather than a column somewhere: `base`, `preamble` and
`max_concurrent` are docs/data-model.md's own next three per-repo
settings, and this is the row they would join — as the second of them
since has. A repo has a row only
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
and `remove`, joined since by `prompt-extension [-set text]
<owner/name>` — over `ui.HTTPClient` methods mirroring the
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
place they are invisible. The repos *page* rows such a repo too, from the
same sources, so the two agree on which repos this deployment knows
about.

### Standing instructions: the prompt extension

`orchestrator.BuildPrompt` is deliberately only the facts that are
grain's own — the task, the branch it must push, the repos it may read,
the push/check/repair loop — because everything it says has to be true of
every deployment there will ever be. A deployment has facts of its own
that grain cannot know and that a task author should not retype on every
task: a house style, the command that actually runs the tests, "this
repo's migrations live in `db/`, read `db/README.md` first." With nowhere
to put those, the only place they fit is each task's body, which is the
same paragraph written again per task and drifting one task at a time.

The prompt extension (grain/task-114) is that place, in three layers —
`model.Config.PromptExtension`, `model.RepoConfig.PromptExtension` and
`model.Task.PromptExtension`, composed by
`model.PromptExtensionFor` and appended to the prompt as its last
section:

- **the deployment's**, on every run it dispatches;
- **the target repo's**, appended after it for a task that targets that
  repo;
- **the task's own**, which *replaces* both for that one task.

**Deployment and repo append; a task replaces.** The first is the rule
per-repo default capabilities already follow, for the same reason: two
people write these at different times, and a repo silently discarding
what the deployment said would be a setting that fails where nobody is
looking. The second is what "overridable for specific tasks" has to mean
— an override that could only append leaves no way to run one task
without instructions that are wrong for it, and a repo-wide "never touch
the generated client" is exactly what a task regenerating that client has
to be exempt from. The cost is that a task keeping the deployment's text
and adding a line restates it, and the forms show what is being replaced
while that choice is made, which is where restating it is cheap. What
there is deliberately no spelling for is "this task gets nothing at all":
empty means "no override", the same `zero means unset` every other
per-task override uses.

**Read at dispatch, not seeded at creation** — the opposite of the
default capability layers next to it, and the difference is what each
thing is. A capability is a grant, which belongs on the task so it can be
seen and detached; standing instructions are text an operator tunes by
watching runs go wrong, and a seed would leave every task already queued
carrying the wording that was wrong. So `RunCycle` refreshes the
deployment layer out of `grain_config` every tick (beside
`max-agent-turns`), `RunDispatch` reads the repo's row from the task's
own target, and the task's own is already on the task. Editing any of
them reaches the next run rather than the next restart — and not a run
already live, whose prompt was built when it began.

**Where each is edited is where the thing being edited lives**: the
deployment's on Settings → Agents (beside which agent and which model,
since it is the same question of what the agent is told), a repo's on
that repo's own page beside its default capabilities (`RepoPage.jsx`,
grain/task-111; `GET`/`PUT /api/repos/{owner}/{name}/prompt-extension`,
reporting the same whole-defaults document the capabilities route answers
with, so a page showing one field can show what the other holds), and a
task's on the new-task form under Advanced
options, beside the deployment text it would replace. `grain settings
-prompt-extension` and `grain repo prompt-extension [-set text]
<owner/name>` are the same two from a shell, printing all three layers
and the composition they produce.

Because a repo can be configured without being allow-listed and without
any task targeting it, `GET /api/config` also names the repos that carry
one (`reposWithPromptExtension` — the names, never the text, which would
be a paragraph per repo on a response every open tab polls). That is what
puts up a row for a repo whose only configuration is standing
instructions, so text that reaches every run against it is not text with
nowhere to read it.

### Setting a repo up before the first turn

Standing instructions are the wrong shape for one thing a repo often
needs to say: *run this before you do anything*. `make deps`, an `npm
ci`, a virtualenv, a generated file the tests will not build without —
written as prose in the prompt extension, that is an instruction every
run has to spend a turn obeying, and a run that obeys it second has
already read the wrong failure out of a tree that does not build. So
`model.RepoConfig.SetupCommand` (grain/task-154) is a command grain runs
rather than an instruction grain relays.

**It runs where the checkout is made.** `orchestrator.prepareCheckout`
runs it after the clone and before the agent's first turn, through the
same `run_command` tool the clone itself goes through — which is what
makes it work on either sandbox backend, a local directory or a kontur
VM, with no second route into the sandbox to keep in step with the first.
`recreate_sandbox` runs it again on the way back (`restoreCheckout`): a
rebuild takes the `node_modules` and the virtualenv with it, and handing
a run back a checkout in exactly the state this field exists to prevent
is worse the second time, because the run was told at turn 1 that setup
was done for it.

**What it did goes in the prompt**, which is the whole point of running
it here rather than hiding it: the command, its exit status, and the tail
of what it printed, plus — for a failure — the sentence that separates a
broken checkout from a broken change ("a build or test failing for that
reason is this, not your change"). A run told nothing debugs the repo's
toolchain from a failure it has no context for, or "fixes" the code until
the broken tree stops complaining.

**A setup that fails is the run's problem; a setup that never finishes is
grain's.** Non-zero exit does not fail the dispatch — grain cannot know
whether a broken `make deps` is fatal to the task in hand, and the run
can find out in one command, or may be the very task filed to fix it. But
a command still running at `setupCommandTimeout` (ten minutes, which is
`mcp`'s own ceiling for any sandbox tool call) has told nobody anything
and would otherwise spend the run's whole wall-clock budget inside a
single tool call the agent never sees, ending with the sandbox destroyed
and nothing to show. That one fails the dispatch, which leaves a human a
run whose detail names the timeout.

It is edited where the other two per-repo settings are — that repo's own
page, `GET`/`PUT /api/repos/{owner}/{name}/setup-command` answering with
the same whole-defaults document, `grain repo setup-command [-set
command] <owner/name>` from a shell — and `GET /api/config` names the
repos that have one (`reposWithSetupCommand`, names only) for the same
reason it names the ones with standing instructions. Nothing validates
the shell: it is an arbitrary command for an arbitrary toolchain, and the
only thing that can say whether it works is running it in a checkout,
which every run then reports on.

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
frontend's "Other secrets" list (below, "Secrets sit with what uses
them") can hide its controls behind a note rather than show ones that
would only ever 404; `PUT`/`DELETE
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

### Secrets sit with what uses them, not in a pane of their own

The endpoints above gave the UI a Secrets tab: every secret in the store,
each key a deletable chip, and a secret/key/value form under it. That is
the whole store faithfully rendered, and it is nearly useless as an
operator surface — a value is only ever meaningful to whatever resolves
it, and nothing on that tab said which name belonged to what. Knowing
that `gcp-key-minter` is the credential `gcp-key` mints *through*, or
that `github-app/private-key` is half of what `github-sandbox`
authenticates as, was knowledge you brought to the pane rather than
anything it told you. Meanwhile the pane that *did* know — the
Capabilities tab, which already reported "Missing secrets:
`gcp-key-minter`" — could only point somewhere else.

grain/task-110 puts each one where it is used. `CapabilityStatus.Secrets`
reports every `CapabilitySpec.Requires` entry, set or not, resolved into
the `{secret, key}` a write would address it by, and the Capabilities tab
renders a write-only field per entry on the capability's own row: the
pane that says what is missing is the pane that fills it in. The agent
credentials were already this shape, on the Agents tab beside the choice
of framework (below, "Three agent frameworks"), which is where the
argument came from.

Two details are worth naming. `Requires` comes in the two forms
`Store.Resolve` accepts, and the bare `<secret>` one names no key at all
— so for that form the reported key is whichever key the secret already
holds, when it holds exactly one, and `secrets.AgentCredentialKey`
otherwise. Writing to the key already there is what keeps a value set
from the UI from *adding* a second key to a secret seeded under some
other name (`scripts/setup.sh` writes the minter key as
`gcp-key-minter/key.json`), which is precisely the two-key state the bare
form can no longer be resolved out of. And nothing is offered at all when
`Config.Secrets` is nil: no store, no field, the same
nil-means-unavailable reading `missingSecrets` already took.

What is left is the remainder — a secret in the store that no capability
requires and no framework runs as, which is a state a renamed
`gcpkey.Config.MinterCredential` or a future capability can put a
deployment in. That keeps the old flat list and its form, under "Other
secrets" at the foot of the Capabilities tab, filtered to exactly what
nothing above claims. It is deliberately not a tab: it is the leftovers,
and nothing grain itself resolves should ever appear in it.

## Three agent frameworks, any of them per task

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

The credential each framework runs as moved with it. Each is stored in
this deployment's own secrets database now, under the well-known names
`pkg/secrets` exports (`GeminiAPIKeySecret`, `ClaudeOAuthTokenSecret`,
`OpenAIAPIKeySecret`), and the Settings pane writes them: a
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

Every framework needs a binary on the host, and that requirement
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
installs every agent CLI now (bwsalmon/agents#645), in CI: an image
carrying some of them is an image that fails every run choosing another,
and which one a run chooses is a live per-task decision. claude and agy
come from their own installers; codex comes from its published release
archive, at a version pinned in the `Dockerfile` itself -- an image built
twice a week apart would otherwise carry two different agents with no
record of which, and the whole point of baking the CLIs in is that what
an image runs is settled at build time. `scripts/setup.sh` checks the
image for all three (`verify_agent_cli`) and reports each in its
readiness summary; `GRAIN_AGY_PATH`/`GRAIN_CLAUDE_PATH`/`GRAIN_CODEX_PATH`
still override any of them with a copy on the host, bind-mounted in at
the path they name. Each `build*Framework` fails the same way when its
binary is missing: naming the install, not the `$PATH` lookup, so an
operator reads a missing binary rather than a broken grain.

`grain-daemon.service` also exports a `HOME` that exists now
(`$GRAIN_DATA_DIR/home`). `$GRAIN_USER` is created `--no-create-home`,
so systemd would otherwise hand the daemon the `/home/grain` its passwd
entry names and nothing ever creates -- which the daemon itself never
minded and the claude CLI, which writes its own state under `$HOME`,
would. `agy` needs nothing from it: `agent/antigravity` hands every run
a private `HOME` of its own, for the per-run MCP isolation described
above.

One consequence worth naming: several frameworks writing into one
`TranscriptDir` means several transcript formats in it at once, so
`ui.Config.LiveTranscripts` can no longer be whichever reader matched
the deployment's framework at startup. `cmd/grain`'s
`liveTranscripts.Tail` picks per file instead.

*How* it picks changed with the runtime replacement. While one framework
tee'd an already-readable narrative, "does the file open with a JSON
object" separated them. Each mirrors its subprocess's own NDJSON now --
claude's `--output-format stream-json`, agy's, codex's `--json` -- so
the discriminator is the shape each vocabulary tags its events with
instead: agy's carry `event`, codex's carry either a dotted `type`
(`item.completed`, `turn.failed`) or a nested `msg`, and claude's carry
a bare `type` (`transcriptFramework`). It sniffs the first line that
*parses* rather than the first line, since reading a file the framework
is still appending to routinely catches a half-written one. A run's
finished transcript needs none of this, since
`agent.Result.Transcript` is already rendered text by the time the store
sees it.

### The third one: `agent/codex`

`agent/codex` runs OpenAI's `codex` CLI, and adding it was mostly a
matter of the seam already being the right shape: `agent.Framework`,
the per-dispatch factory above, one secret name, one model setting, one
more radio button. Where it needed thought is the three things the CLI
does not have.

It has no `--mcp-config`. Its MCP servers live in a `config.toml` inside
its config directory, which is `~/.codex` unless `CODEX_HOME` names
another -- a per-user registration, which is exactly the wrong shape for
a deployment running two sandboxes at once (`agent/antigravity`'s own
doc comment makes this argument about `agy mcp add`). So every run gets
a private `CODEX_HOME` holding nothing but the config this run needs,
and it goes when the run does.

It has no `--max-turns`, so `RunConfig.MaxTurns` is enforced here, off
the live stream, exactly as `agent/antigravity` enforces it: `turnCap`
counts completed assistant messages as they stream past and cancels the
run's context, which `procgroup.Prepare` turns into a kill of codex and
its MCP child both.

And it has no `--allowedTools` to empty its native roster with. What the
per-run config does instead is deny that roster anything worth having:
`sandbox_mode = "read-only"` and `approval_policy = "never"`, so codex's
own shell and patch tools cannot write to the controller and cannot stop
an unattended run to ask a human for permission to. There is a third
line, `features.code_mode_host = false`, which is about the *record*
rather than about permissions: codex's code mode has the model reach its
MCP tools from code it writes and runs, rather than as tool calls of its
own -- and `mcp_tool_call` events are exactly what `Result.ToolCalls` is
built from, which is what `orchestrator.ProcessResult` reads to find
that a run asked a question or left a closing comment. The deployment
image carries only the `codex` binary, so that mode is already off;
saying so in the config makes it a decision rather than a property of
how the image was built.

Two smaller things the CLI's actual behaviour settled. A bare
`{"type":"error"}` event is *not* terminal -- codex emits one per
attempt while it retries a dropped connection, and a run that reconnects
goes on to finish normally, so reading the first as the end of the run
would report a working run as a failure; what ends a run is
`turn.failed`, which repeats the last of them as its own message. And an
error *item* is codex reporting something about itself (the optional
code-mode host it was told not to use, on every run) rather than the run
failing: it belongs in the narrative and nowhere else.

The credential is an `OPENAI_API_KEY`, passed through the subprocess's
environment like the other two and never in argv. An API key rather than
a ChatGPT sign-in, because the private `CODEX_HOME` is also why a
`codex login` on the host is not visible to a run -- and because a key
is something a deployment can replace from the UI between one run and
the next, which a login file is not.

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
(`agent.PullRequestFramework`, implemented by `pkg/agent/claude`,
`pkg/agent/antigravity` and `pkg/agent/codex` as "was I built
`WithGrainServer`?");
`RunDispatch` asks, and passes the answer to `BuildPrompt`. A `Framework`
that does not implement it at all answers no, which is the safe
direction: a run never told about a tool it happens to have loses one
convenience, where a run told to call one it does not have burns turns on
an error it cannot fix.

Whether runs actually start calling it, and whether a run that sees a
failing check fixes it instead of opening the pull request and stopping
there, are measured rather than assumed — see "Measuring what a run does
with its tools" below, whose `mid-run pull requests` section is exactly
those two questions. One caveat belongs here rather than there: that
number is the tool's uptake overall and not prompt wording against a bare
tool description. The change that added the tool and the change that made
`BuildPrompt` name it landed half an hour apart, so no run in this
deployment's history has ever had the one without the other, and the
contrast the question originally wanted is not available from production
data at all.

## What a pull request says it does

For a while grain's own pull requests said nothing. `EnsurePullRequest`
wrote one line -- "Automated change for grain task 131." -- and a
reviewer opening one got a title, a diff, and no account of what the
change was for. That was a regression, and an old one: v1 built a
freshly opened pull request's body out of the pushed branch's own head
commit message (`bwsalmon/agents#79`), and the port to this repository
kept the plumbing -- `github.BranchHead` still carries the tip commit's
message, and its doc comment still says why -- while dropping the use.

The description is the agent's own words again, but from the whole
branch rather than its tip (`orchestrator/description.go`,
`github.Client.CompareCommits`). The tip alone was good enough for v1,
where a run pushed once. It is not good enough now: "Letting a run watch
its own CI" (above) sends every run round a push/check/repair loop, so
the newest commit on a finished branch is routinely "Fix the vet
warning". So the description leads with the first commit whose message
has a body -- a message with a body was written to explain something,
which is exactly the one a reviewer wants -- and lists every other
commit underneath by its summary line, with merge commits dropped, since
"Merge main into grain/task-131" is a fact about the branch's shape and
not about the change.

Opening a pull request early would otherwise freeze that description at
whatever the branch said at the time, which for a run that opens one to
see CI is often a single "wip" commit. So the description is rewritten,
not written once: every `EnsurePullRequest` on a pull request that
already exists -- the next `open_pull_request` call, and the finish path
after them all -- rebuilds it from the branch as it now stands. What a
reviewer eventually reads is the finished change's own account of
itself.

Two things it will not do. It never overwrites a body a human has
touched: grain's own footer is the marker, and a description that does
not end in it belongs to a reviewer, who wrote something no commit
message can reconstruct. And a failed read never downgrades a good
description -- if the commits cannot be read, the pull request opens
with the one-line body grain can always write, and an existing
description is left exactly where it was. A description is worth an API
call and never worth failing a finish over: the pull request is the
thing that matters, and it opens either way.

The last piece is telling the agent, since a commit message is the one
thing here grain cannot write itself. `BuildPrompt` says outright that
the pull request's description is built from this branch's commit
messages and asks for a summary line, a blank line, and a paragraph on
what changed and why -- the same sentence v1's prompt carried, and for
the same reason. A run that is not told writes "wip" and "fix tests",
which are perfectly good `git log` entries and a description-free pull
request.

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

## A run can say what it is doing

A dispatched task reads `running` and nothing else for as long as its run
lasts. That is the honest state -- `task_state` derives it from a live
`task_run` row and nothing more -- but it is the same word whether the
last half hour went on a slow test suite, a wait for CI, or an agent
going in circles, and the only way to tell was to open the transcript
once the run was over, by which point the question had answered itself.

`update_status` (`pkg/mcp`'s `NewStatusTools`) lets the run answer it. It
takes one short phrase -- "waiting for CI on the second push", "running
the test suite", "reading the dispatch path" -- and grain shows it on
that task's row for as long as the run lasts, replaced each time the run
calls it again. It travels the same hop `open_pull_request` and
`recreate_sandbox` take, to `POST /api/tasks/{id}/activity`, and for the
plainest of their reasons: the row being written is in the daemon's
store, and the `mcpserver` process holds nothing but a transport into a
sandbox. Which task it lands on is fixed by `-task` at process start, so
a run can only ever narrate itself.

Four decisions shape it, and each is a way it is *less* than the two
tools beside it:

- **It changes nothing.** grain shows the phrase and never reads it back:
  no dispatch decision, no state, no merge gate consults it. That is what
  makes it the one tool here safe to call as often as the run has
  something new to say -- and what makes the tool's own description say
  so, since a run that mistook it for `comment_on_issue` would put its
  answer somewhere nobody is served it.
- **It is a fact about now, not a log.** The phrase and the moment it was
  written are two nullable columns on `task_run`, and each call replaces
  the last. `Store.TaskActivity` reads only *live* runs, so a finished
  run keeps whatever it last said -- which is how a cancelled run leaves a
  record of where it got to -- while nothing renders it as current.
- **The age travels with it.** "Waiting for CI" ten seconds old and the
  same words an hour old mean opposite things, so the task row renders
  `activityAt` beside the phrase (`state.js`'s `runActivity`). A run that
  says one thing and goes quiet is legible as exactly that, rather than
  as a run still doing what it said.
- **A call that arrives late is not an error.** A run whose task grain
  has already finished gets a plain answer saying nothing is listening,
  not a failure: it raced the end of its own run, which is nobody's
  mistake, and the phrase is the one thing here whose loss costs the work
  nothing.

Unlike `recreate_sandbox`, `BuildPrompt` does name this one, on exactly
the condition that registers it. The reason is the mirror of that tool's:
this is the only tool a run has whose whole value is to somebody
*outside* the run, which makes it the one a run working on its own task
would never go looking for. The paragraph also says when *not* to call it
-- a status costs a turn like any other call, and a run narrating every
file it opens has spent its budget on narration.

### grain says what it is doing too

A tool can only narrate the part of a run an agent is driving. Everything
before the agent's first turn -- `orchestrator.RunDispatch`'s sandbox
acquisition, the VM boot behind it, the clone, the repo's setup command,
the capability mints -- is precisely the stretch a run *cannot* describe,
because there is no run yet to describe it, and on a kontur deployment it
is minutes long. A task that had just been dispatched therefore read
`running` with nothing beside it for the one part of its life where "what
is it doing?" has a precise, known answer, and the thing holding that
answer is grain.

So grain stamps its own phrases there (`orchestrator.setupNotes`), over
the same `Store.SetTaskActivity` the tool reaches: "building a sandbox",
"giving the sandbox its git credentials", "cloning acme/widgets",
"running the repo's setup command", "minting the task's credentials".
Same column, same row, same renderer -- there is nothing extra to read
and nothing extra to show. Each is best-effort, like every other
bookkeeping write on that path: a run must never fail because grain could
not say what it was doing.

Two things had to be decided rather than left to fall out:

- **Whose sentence it is, is marked.** Everything that had ever appeared
  in that field was something an agent wrote, and a reader who has learnt
  to read the phrase in the agent's voice should not have to guess. It is
  not a prefix in the text -- an agent could type that too. It is read
  off the row: a phrase standing while `task_run.agent_started_at` is
  still NULL is grain's, since no agent existed to have written it
  (`model.RunActivity.BySetup`, `ui.Task.ActivityBySetup`), and the UI
  puts a small "grain" mark before it. The other half of that is the
  handover: `RunDispatch` clears grain's last phrase in the same breath
  as `SetRunAgentStarted`, so nothing grain wrote is ever left standing
  where it would read as the agent's -- and so an agent's first quiet
  half-hour shows an empty row, which is the truth, rather than "minting
  the task's credentials" from forty minutes ago.
- **A failed setup keeps its last phrase.** A run whose sandbox never
  came up finishes `setup-failed`, and whatever grain was doing when it
  gave up stays on the finished row. Nothing renders it as current
  (`Store.TaskActivity` reads live runs only), so it contradicts nothing
  -- and beside a detail that says the sandbox could not be prepared,
  "cloning acme/widgets" is the half the detail does not carry.

A sandbox rebuilt mid-run (`recreate_sandbox`) is deliberately *not*
narrated this way: the run is live, the row's phrase is the agent's own,
and grain does not talk over it.

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

The same build stamps in the grain image's *own* reference
(`cmd/grain/grainimage.go`, the `GRAIN_IMAGE_REF` build arg, printed by
`grain image`), for the one place a deployment has to write down which
build it is: the CI step it installs in its own state repository, whose
`grain state check` refuses a dump stamped with a schema it does not
know. Same rules, same reasons — the sha- tag, so a deployment held at
an older one names itself rather than main, and the tag CI keeps pointed
at main as the fallback for a build that was never stamped at all.

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

One thing containerizing the deployment did not change is how anyone
reaches it. The daemon binds `127.0.0.1:80` and nothing here opens a
firewall hole, so getting at the UI has always been an SSH tunnel or
"whatever the operator's network already puts in front of the host — a
loopback port, an SSH tunnel, Tailscale, IAP". `GRAIN_TAILSCALE_ENABLE=1`
(default `0`) is `setup.sh` building the third of those rather than only
naming it: it installs `tailscale` from Tailscale's own apt repository,
brings the node up with `GRAIN_TAILSCALE_AUTH_KEY` so an unattended
deploy needs no browser at a login URL, and runs `tailscale serve --bg
--yes --http=80 http://127.0.0.1:80` — the UI's own port, published on
this node's tailnet address. `GRAIN_TAILSCALE_SERVE_PORT=443` serves it
over HTTPS with a tailnet certificate instead, for a tailnet with
certificates enabled; `GRAIN_TAILSCALE_HOSTNAME` and
`GRAIN_TAILSCALE_UP_ARGS` name the node and pass anything else
`tailscale up` takes (`--advertise-tags=tag:grain`, say).

What it deliberately does not do is move the UI. `-ui-addr` stays on
loopback, the container keeps host networking, and `tailscaled` — a host
process, outside the container, since a tailnet address belongs to the
machine and has to outlive any one `docker run` — is what proxies the
tailnet to it. The alternative, binding the UI to every address and
letting a tailnet ACL sort it out, would reach the same browser and open
the same port to everything else on the network on the way. The
repository is packaged rather than a distribution's own because
`tailscale serve`'s current flag syntax is only spoken by releases from
1.50 onwards, and only the two files that repository needs are fetched
here — with the *image's* `curl`, through `image_run`, since there is
none on the host by design.

The auth key is the one credential `setup.sh` handles that never reaches
`$GRAIN_DATA_DIR`: it authenticates this *host* to a tailnet rather than
this deployment to anything, so it is `tailscaled`'s to keep under
`/var/lib/tailscale`, and a re-run with no key at all leaves a node that
is already up exactly as it is (`tailscale up` over a live node with a
since-expired key would take a working deployment off the tailnet for no
gain). Every step of this converges rather than failing, the way the
kontur prerequisites do: an unsupported distribution, a missing key, a
tailnet without certificates each end with the feature off, the reason
logged, and the readiness report saying `GRAIN_TAILSCALE_ENABLE=1 was
requested but this run could not finish it` — reachability is not what
makes a deployment work, so it must not be what makes one fail.

The security note above is the whole reason this is a flag rather than a
default: the UI and the API it serves carry no auth of their own, so
publishing them to a tailnet makes everyone on that tailnet this
deployment's one configured actor. A tailnet ACL restricting the node is
the answer, the closing report says so on every run that turns this on,
and choosing it is not something a deploy script can do for an operator.

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
secrets endpoints already use for their own optional `-server-data-dir`
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
deployment default, and `konturctl vm create -disk-size-mb` at the one
moment a sandbox's size is decided. Zero kept meaning "unset" — the flag
was left off the create entirely, so a deployment that never set one
passed exactly the arguments it passed before. (It no longer is: see
"Every sandbox is built at a size grain chose".)

Two things about it were genuinely unlike CPUs and memory when it landed,
and both were visible in the code:

- **There was no default to show.** `ui.Settings` reports
  `sandboxCpusDefault`/`sandboxMemoryMbDefault` so an unset box can show
  what is really in effect rather than a misleading literal 0. Disk had
  no such constant: an unset disk was however large *this deployment's*
  guest image is, which is a property of an image somebody built, not a
  number this build can name. The field had no placeholder, and said so
  in words instead — until grain started naming that number too.
- **A bigger disk is not by itself more space.** The image's filesystem
  ends where it ended; the extra is unallocated until something grows it.
  `scripts/kontur/guest-setup.sh` installs a `grain-growfs` unit that
  runs `resize2fs /dev/vda` on each boot, which is a no-op on a VM whose
  disk was not enlarged and a one-line grow on one whose was.

**The `konturctl` half of it exists now, and is vendored here.** For a
while it did not: this setting reached a flag no `konturctl` had, so a
deployment that set one got a failed create and one that left it at zero
passed the arguments it always did, which is why nothing noticed.
`konturctl vm create -disk-size-mb` (bwsalmon/kontur#39) is that flag —
the VM's own container sizes the qcow2 overlay to it before
cloud-hypervisor opens it, growing one an earlier boot left behind rather
than only sizing a fresh one, and refusing to shrink it or to go below
the guest image it reads through to. It landed on `bwsalmon/kontur`'s
`main` and reached this repo by a resync rather than as a local patch,
which is what `third_party/kontur/VENDORED.md` asks for.

It is MiB where this setting is GiB, so
`orchestrator.KonturConfig.createArgs` converts at that one point, and it
is `-disk-mode=overlay` only — the guest image underneath is shared with
every other VM booting it, so nothing ever resizes that. grain's VMs are
in that mode already: `scripts/setup.sh` asks for it by name.

The real-KVM suite asserts the whole chain from inside a guest: a VM
created a gigabyte larger than the image it boots comes up with a
`/dev/vda` that size and a root filesystem grown onto it. A fake
`konturctl` can only ever prove the flag was passed.

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
and counting only the unfinished states, since every task whose run is
over, submitted or not, is a census rather than a queue), and occupancy
as a fraction of
`max_concurrent`. Idle capacity next to a
deep queue is a scheduling problem; saturated capacity next to a deep
queue is a capacity one. They are the first two numbers any optimization
here should have to move.

**The UI reads the same report,** in a pane of its own at `/metrics`,
reached from the sidebar beside Settings and System. (It was a tab of
the System pane until grain/task-173 — see "Metrics is its own
destination on the sidebar" above for why it left.) The window picker sends the same
strings `-window` takes, the throughput
buckets are drawn as sparklines, and the two presentation rules above
are enforced rather than described: the latency stages are a table of
independent distributions and never a stacked bar, which would draw a
claim about them adding up that the numbers do not make, and the backlog
is a section of its own headed "right now, not over the window". Each
stage's `n` sits beside its percentiles, and a percentile with too few
samples behind it to mean what its name says — fewer than 10 for a p90,
100 for a p99 — is dimmed and footnoted rather than shown as if it were
one. Unlike the System pane's own panels there is no poll: a report costs
a full scan every time it is asked for, so it loads once and reloads
when the window changes or Refresh is clicked.

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

## Measuring what a run does with its tools

The two sections above measure the outside of a run: when it started, how
long it took, how it was recorded. Everything that makes a run *hard* is
on the inside of it — a tool that answers unhelpfully, a search-and-edit
loop that keeps missing, a command that gets killed by a bound nobody
chose, a CI wait that never resolves — and none of it was measured
anywhere. `docs/agent-ergonomics.md` is a whole review of that surface
argued entirely from code-reading and single transcripts, because there
was no aggregate to argue from.

The counting was already happening. `orchestrator.toolCallSummary` has
always tallied every tool a run called and how many of those calls came
back as errors, for every run including the successful ones — and then
rendered it into English in `task_run.detail`, a column a human reads one
row at a time. Nothing could aggregate it, because it was prose.

So the census is stored as data, and `grain metrics` prints it under the
latency table:

```console
tool use (37 run(s) in the window recorded what they called)
  calls                      4127  (98 per run at the median, 212 at p90)
  errored calls               372  (9.0% of them -- a handful is the ordinary shape of this work)
  tool                 runs    calls   errors timed out  mean bytes   p95 bytes
  run_command            37     2894       7%    12 (0%)        1841     <=65535
  edit_file              31      704      22%         -          118       <=511
  read_file              29      402       4%         -        4210     <=32767
  wait_for_checks        18       84       0%         -        1024      <=2047
  (p95 bytes is an upper bound: sizes are kept in base-2 buckets, so the real
   number is inside the octave below it. It is what should size the tool-result cap.)

CI waits (84 wait(s) across 18 run(s))
  verdicts                 passed=41 failed=28 timed_out=13 no_checks=2
  blocked                  p50 3m20s, p90 14m0s, max 15m0s
  pushes before green      2.4 on average, 7 at worst (over 16 run(s) that went green)

mid-run pull requests (34 run(s) in the window could have opened one)
  opened one themselves        21  (62% of them, 48 call(s) in all)
  repaired by the queue after  10% of the 21 that did, 31% of the 13 that did not
  (a repair is the merge queue cleaning up a red build the run left behind.
   It is recorded on the task, not the attempt: read the difference, not either rate.)
```

**This is the one measurement that had to be stored,** and it is stored
because it is not derivable from anything that survives the run.
`agent.Result` carries every call and every answer, and is discarded when
`RunDispatch` returns; the transcript is per-framework prose that a
tool's own output can forge (`orchestrator.outcomeOf` says why counting
calls out of it would be unsound); `task_run.detail` is a sentence. Two
tables, written once per run after its outcome, best-effort and logged
rather than surfaced on failure — a measurement that cannot be taken is
not a reason to fail the run being measured. `task_run_tool` is the
census, one row per run per tool; `task_run_check_wait` is one row per
`wait_for_checks` call, because the CI loop is a sequence rather than a
total. Neither needed a `SchemaVersion` bump: `CREATE TABLE IF NOT
EXISTS` creates a *missing* table on an existing store perfectly well,
and an older store simply starts filling them.

**Counts and sizes only.** No argument, no result and no fragment of
output crosses into a measurement table. What a run's commands said stays
in the transcript, which has its own bounds and its own reasons to exist.

**A result-size percentile is a bound, and says so.** An exact one needs
every sample, which means a stored row per tool *call* — hundreds a run,
read whole on every report. Each census row instead carries a base-2
histogram of its result sizes, so `p95 <=65535` means "95% of those
answers were at most 64 KB", with the real number inside the octave
below. That is precise enough for the question it exists to answer: what
should `mcp.maxToolResultBytes` be — a question it has now answered, and
the 64 KB guess it was left at is 16 KB because of what it said (see "No
single answer may eat the run's context" for the window it was read from).

**Two facts had to be read back out of a tool's own text,** because
nothing else carries them: whether a `run_command` was ended by its bound
rather than by the command (a bounded-out call is an error like any
other), and which of its four verdicts a `wait_for_checks` reached (every
wait_for_checks answer is a success). `mcp.RunCommandTimedOut` and
`mcp.ReadCheckWait` live beside the code that writes those sentences and
match markers that the sentences are *built* from, which is the only
arrangement where rewording one cannot silently zero the other. Only
grain's own trailing notices are searched, so a command whose output
quotes the notice is not a command that timed out; the hedged `exit=137`
notice (which may be the OOM killer) and the stalled-transport notice are
counted as neither, since neither is a bound running out.

**Attempt outcomes are also split by what they mean.** `task_run.outcome`
is coarse enough that `cancelled` is both "a human closed the task" and
"the run hit its two-hour wall", and `failed` is both "the CLI died" and
"the run exhausted `-max-agent-turns`" — pairs with different fixes,
distinguishable only by the sentence recorded beside them. `grain
metrics` prints a second line under the outcomes for how those attempts
actually ended, `no_action` among them: a run that had tools, used them
and produced nothing is the purest measure of this surface failing, and
it used to be one key in a map beside `succeeded`. The sentences are
built in `pkg/model` and read back there (`model.EndingOf`), so the
writer and the reader cannot drift apart.

**The CI loop is measured end to end** because the prompt sends every run
around it: push, `wait_for_checks`, fix, push again. A deployment where
most waits end `timed_out` has `mcp.DefaultWaitForChecksTimeout` set
wrong for its CI; one where most end `no_checks` is sending runs to wait
for CI that does not exist; and "pushes before green" is what the loop
costs in rework — 1.0 is CI right first time. A push is counted from the
`git push` in a `run_command`'s own arguments, and only from a call that
did not error, because a rejected push is not a push: it under-counts
rather than over-counts, which is the right direction for a number whose
point is how much rework there was.

**The last stretch of that loop is `open_pull_request`,** and it gets a
section of its own because its two questions are different shapes. The
first is a share: of the runs that were offered the loop at all, how many
took it. "Offered" is the whole of what makes that number mean anything,
so the denominator is stated beside the rate and is narrow — a run that
finished inside the window, recorded a census, and belonged to a task
with a repo to push to, since `BuildPrompt` puts the push/check/repair
paragraph and the sentence naming the tool inside `task.Target != nil`
and a task with no target was never asked.

The second is a comparison and is deliberately not reported as a rate.
"A run opened its pull request and the merge queue still had to repair
it" means nothing alone: some share of branches go red for reasons no
amount of watching would have caught. So both cohorts are printed with
their own denominators — the runs that called the tool and the runs that
did not, each with the share whose task the merge queue later had to
repair (`Observation.MergeQueueRepairAt`, or the older `model.LinkFixTask`
for a task repaired before grain/task-271; both are counted, so a report
spanning that change does not show the rate falling to zero on the day
the recording moved), which is the recorded form of a red build outliving
the run that pushed it. Read the difference, not either number. The
signal is coarse in two known ways, both stated where the number is read:
it is recorded on the task rather than the attempt, so every finished
attempt of a task that ever went red counts as having gone red; and a
repair can only happen to a task that got as far as a pull request, which
flatters any cohort holding runs that pushed nothing.

**The fine-grained version of that question is not counted in bulk, on
purpose.** "Did a push follow the last failing report" is answerable by
eye from one run's persisted transcript, which is ordered — but a
transcript renders a call as a `> name(args)` line that a tool's own
*output* can contain verbatim (`agent/claude/transcript.go`), so a count
taken over transcripts would measure forgeries alongside calls. Both
numbers above come from the census rows instead, which are written by
grain from `agent.Result` and are not text an agent can author. Nothing
new is stored for either: `model.TaskTiming` reads two derived columns it
did not before — whether the task had a target, and whether it carries a
fix-task link — because a run's own row says nothing about the task it
was for and both questions are about the task.

**Everything else about it follows this package's existing rules.** A
census row has no moment of its own, so it belongs to a window when its
*run* finished inside it. A run that recorded none contributes to nothing
rather than to a zero — which is why `tool use` states how many runs are
behind it, and why none of the three sections renders at all, in the CLI
or the UI, until something has recorded one. A deployment upgraded into this reports
no tool use for its older runs, and that is the honest answer: nobody was
measuring.

## Merge capacity is its own number

Concurrency was one number: `max_concurrent`, the count of runs
`dispatch.Cycle` would let be in flight. Every kind of run drew on it
equally, which put the two least alike kinds in direct competition. Most
runs are new work, at the start of its life. A few are the merge queue's
own repairs of a pull request that will not land — the last step of work
that is already committed, pushed and reviewed. A saturated deployment
starved exactly the second kind, and starving it is expensive twice over:
the branch a repair targets keeps moving while the repair waits, so one
delayed long enough has to start over, and the queue behind it waits
too.

What counts as a merger is `Store.mergerTaskSQL`, written once and read
by all three places that decide it. It has two arms: a task the queue has
sent back to work on its own branch (`Observation.MergeQueueRepairAt`
with no `completed_at`, which is what a repair is since grain/task-271),
or one of the separate fix tasks the queue used to file for that
(`Origin.Reason == ReasonFix`), which a database may still hold one of.

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
ordinary work at the head of the backlog no longer hides the repair
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
where it was: the merge queue's own repairs contending for worker
capacity like anything else.

## A review, before the merge

A task can carry a review: `Task.ReviewTemplateID`, the id of a
`model.Template` whose title, body and grants are what a *second* agent
is dispatched with once the first one's work is done. The "reviews"
reconciler files it (`orchestrator.SyncReviews`), the merge queue waits
for it, and everything else about it is machinery that already existed.

The shape is the separate fix task the merge queue used to file before
grain/task-271, reused deliberately: a repair now runs as another attempt
of the task itself, but stacking a second agent on a first one's branch
is still exactly what a review needs. A
review task's `Base` is the reviewed task's branch (`model.BranchName`,
derived, so nothing has to agree with anything), its `AutoMerge` is set,
and it is filed already approved at the head of the backlog. So it is a
stacked branch: the reviewing agent reads the diff on the branch under
review, commits its fixes onto its own branch off that one, and
`syncEntry` merges its pull request straight back into the branch it
reviewed the moment it reads clean. That is as close to "a new agent on
the same branch" as grain can be, given that a run's branch is a function
of its task id and always will be. Like one of those fix tasks, a review
is never a
merge queue entry in its own right (`isQueueMember`): it merges into
another task's branch, not into the repo's base, so it neither takes a
head position nor waits for one.

What is new is the wait. A reviewed task does not merge while its review
is outstanding, so the order is work, review, merge — not work, merge,
and a review of something that has already landed. The wait is on the
*declaration* rather than on the review task existing, which matters more
than it looks: filing the review and advancing the merge queue are two
reconcilers in one cycle, and if the gate were "has a review task been
filed" then whether a change merged before its review existed would come
down to which of them ran first. `Task.ReviewTemplateID` alone holds the
merge; filing the review starts the clock rather than starting the wait.

And the wait is bounded, for the reason the wait on an automatic repair
is (`defaultRepairDeadline`): a task holding its merge for a review reads
PrClean, not PrPending, so the CI stall clock never times it, and the
review's own pull request is never a queue head, so nothing times that
either. A review neither finished nor abandoned after six hours is said
so on the task, which is then blocked in the merge queue's own sense —
it stops being anyone's head, so the tasks behind it move, and it merges
as soon as it reads clean like any other task the queue has given up
driving. That is the deliberate choice between the two ways to be wrong:
merging a change whose review never arrived, having said so, is
recoverable; holding a finished change out of the repo forever on a
review that is not coming is not.

A template rather than a body typed onto the task, for the same reason
`Schedule.TemplateID` is one: "read this branch and fix what you find" is
the same paragraph on every task it is attached to, and the instructions
worth improving once. Nothing about a template says it is a review; what
makes it one is being named as a task's own. Grain appends the subject
the template cannot know — which task, which branch, which pull request —
and supplies nothing else, so what the reviewing agent is told is what
somebody wrote in Templates.

It is attachable from the new-task form, from a task's own edit form (the
usual case: a task is filed, and somewhere before it finishes somebody
decides its change wants a second pass), from `PATCH /api/tasks/{id}`,
and from `grain create -review-template`/`grain update -review-template`.
An edit reaches any review not yet filed, since the template is resolved
when the work is done rather than when the task is created — so a review
can be attached to a task that is queued, running, or already finished
and waiting to merge. Once the review task exists, changing it changes
nothing: exactly one review is ever filed per task (`LinkReviewTask` is
both the record of that and what the merge queue waits on), and the
picker locks and says which task is carrying it out. A review task
carries no review of its own, which is what stops reviews nesting.

The review task itself nests under the task it reviews, the way an
automatic fix nests under the one it repairs, and says which of the two
it is rather than borrowing the other's name: a "review" chip in the task
list, and `grain list -origin review` beside `-origin fix`. Both are
stacked on another task's branch; only one of them is a repair of a red
build.

## Pausing when the agent runs out of budget

A coding agent's credential has a budget, and it runs out: Claude Code
reports `Claude AI usage limit reached|<epoch>` for an OAuth account
whose five-hour window is spent, the Anthropic API answers `429` with a
`rate_limit_error`, and Gemini answers `RESOURCE_EXHAUSTED` with a
`retryDelay`. None of that is a fact about the run that met it.

Read as an ordinary framework failure, though, that is exactly what it
became. The run was recorded `failed`, with the provider's refusal as its
diagnosis; the task took a place in its own failure streak for it and
backed off; every other run in flight kept going until it met the same
wall and did the same; and the next tick dispatched the next queued task
straight into it. A single window's exhaustion could walk the whole
backlog — each task spending an attempt, a sandbox and a VM boot to
discover something the deployment already knew — and take tasks to
`model.MaxConsecutiveFailures` and out of the queue for good, on the
strength of an outage none of them caused.

So a limit is now its own kind of failure, end to end.

`agent.UsageLimitError` is the type. Every framework recognises its own
provider's refusal and returns one instead of a plain error
(`pkg/agent/claude/usagelimit.go`, `pkg/agent/antigravity/usagelimit.go`,
`pkg/agent/codex/usagelimit.go`),
carrying what the provider said and, where it named one, when it will
serve again — an absolute instant (Claude's epoch) or a delay (Gemini's
`retryDelay`). What each package matches on is deliberately narrow, and
narrower still where a *successful* run is being read: `claude` reports
an account limit as readily in a run's final answer as in a failure, and
an agent that greps its own sandbox for the phrase must not be able to
put the deployment to sleep, so the successful path insists on the CLI's
own machine-readable `|<epoch>` form and reads only the terminal result
event, never tool output.

`orchestrator.Pause` is what the deployment does about it. The run that
meets a limit closes the gate until the window resets, and every run in
flight is cancelled on the spot rather than left to grind through the
same refusal — that is what the registry of live runs in there is for,
each `RunDispatch` parking its own cancel func for as long as its agent
is running. `reconcileDispatch` asks the gate before it dispatches
anything and does nothing while it is shut; every other reconciler runs
as usual, since syncing pull requests, advancing the merge queue and
cutting releases cost no agent tokens and are exactly what should keep
working. The gate opens again on its own: it is level-triggered like
everything else in a cycle, so the first tick to find the instant passed
clears it and dispatches. If the window has not really reopened, the run
that finds out pauses it again, and no further traffic follows.

What the provider says is bounded rather than trusted: a refusal naming
no reset waits 15 minutes, one naming seconds waits a minute, and one
naming a reset days out (or an epoch this code misread) waits at most six
hours. A deployment idling for a wrong quarter of an hour is recoverable;
one idling until 2286 is not something anybody would think to look for.

Runs ended either way record `model.PausedOutcome` — "paused", its own
word beside "setup-failed" and "finish-failed", with the provider's own
sentence and the resume time in the run's detail. Both implementations of
the failure streak skip that word (`Store.FailureStreak` in Go,
`task_streak` in SQL), so an outage costs a task nothing: no backoff, no
progress toward `MaxConsecutiveFailures`, and dispatch offers it again
the moment the gate opens. Whatever such a run pushed before it was cut
off is still salvaged into a pull request the way any other interrupted
run's branch is.

The gate is in-process state, not a row — the same reasoning
`CycleTimes` is (see "Why this one measurement is in memory"). A daemon
restarted inside a paused window dispatches one run, meets the same
limit, and pauses again: one wasted attempt, against a durable row that
would have to be reconciled with a credential that may by then have been
changed by the very operator doing the restarting.

### Saying so, and lifting it by hand

None of that was visible from the UI. An operator opening grain in the
middle of a five-hour window saw a queue of ready tasks and nothing
running, and the only places that said why were the daemon's journal and
the detail of the attempts the pause had ended — which you have to know
to open, on a task you have to know to pick.

So the gate is wired to the UI as `ui.Config.AgentPause`, the way
`Config.Cycles` is wired to the metrics report: an interface named in
`pkg/ui`, satisfied by `*orchestrator.Pause` itself, handed over by
`cmd/grain/daemon.go` — which now allocates the one `Pause` at process
start, package-level beside `cycleTimes`, so the UI/API server and the
reconcile loop that comes up after it are talking about the same gate.
`GET /api/config` carries it as `agentPause` and a standing banner says
what the provider said, when dispatch resumes, and how long that is from
now; `GET /api/pause` is the same reading on its own for anything else
polling.

Every read goes through `Pause.Until`, never `Pause.Blocked`: `Blocked`
is what *clears* an expired pause, as the reconcile loop's own read, and
a browser poll must not be able to open the gate out from under the loop
that owns it. A window whose instant has passed simply reads as not
paused.

It is deliberately not a section of `GET /api/metrics`. That report is
computed over rows for a window that has ended; a pause is a gauge of
what this process is doing right now, with nothing in any table behind
it. The `cycles` section is in that report because it is an *input* to
the same `metrics.Compute` call the rest of it comes from — a pause is
not, and filing it there would mean learning to look for "why is nothing
dispatching?" in a latency report.

`DELETE /api/pause` — the banner's own "Resume now" — lifts a pause
early. An operator who has just topped a plan up, or moved the deployment
onto another agent framework, is holding information this process cannot
have: the credential behind the refusal is not the credential the next
run would spend, and there is no reason to sit out the rest of a window
that no longer applies. A lift opens the gate and nothing more — the runs
the pause cancelled are over, recorded as `model.PausedOutcome`, which no
streak counts — so what it buys is the next tick dispatching rather than
skipping. If the limit is in fact still in force, that run meets it and
pauses again, which is the same self-correcting shape as a window that
expires without having really reset.

Both halves are on the CLI too (`cmd/grain/pause.go`), since an operator
ssh'd into the deployment, or driving it with `grain -server`, has no
banner to read. `grain pause` prints the reading — what the provider
said, when dispatch resumes and how long that is from now — and `grain
pause -lift` is the same DELETE the button sends. It is spelled as a noun
with a flag, the way `grain settings` is, rather than as a `grain resume`
verb: every verb in this CLI acts on the task its argument names
(`approve`, `retry`, `reopen`), so a bare `grain resume` would read as
one of those with the id left off. A deployment whose UI was handed no
gate at all (`enabled: false` — a UI served without a reconcile loop
behind it) says exactly that rather than "nothing is paused", which is
the one wrong answer here: it is what an operator would act on to rule
the usage limit out. `grain metrics` still says nothing about any of
this, for the reason above.

## Every sandbox is built at a size grain chose

All three dimensions of a sandbox VM were opt-in: `sandbox-cpus`,
`sandbox-memory-mb` and `sandbox-disk-gb` each defaulted to zero, and
zero meant "leave the flag off the create". A deployment that configured
none of them — which is every deployment until somebody opens the
Sandbox tab — got whatever `konturctl vm create` decides on its own: 2
vCPU and 2048 MiB from `internal/staticpod.Defaults`, and a root disk
exactly as large as the guest image behind it, which
`scripts/kontur/build-guest.sh` packs to the rootfs plus 20% headroom.

Those are real numbers a run lives inside, and nothing chose them for the
job an agent's sandbox actually does. 2 GiB of memory is a small VM for
something that clones a repo, installs a toolchain, builds it and runs
its tests; a few hundred megabytes of disk slack is what a build-heavy
checkout spends before failing part way through with no space left,
on a VM with CPUs and memory to spare. Worse, two of the three were not
grain's answers at all — one belonged to whichever `konturctl` is
vendored under `third_party/`, the other to whichever guest image a
deployment last built — so the size of a sandbox could move under a
resync or an image rebuild without anything in this repo changing.

`pkg/kontur`'s `DefaultCPUs`/`DefaultMemoryMB`/`DefaultDiskGB` are
grain's own answer now — 2 vCPU, 8 GiB of memory, 30 GiB of disk — and
`orchestrator.KonturConfig.createArgs` passes all three on every create,
unconditionally. The resolution order is unchanged and still per
dimension: the task's own `SandboxCPUs`/`SandboxMemoryMB`/`SandboxDiskGB`
first, then the deployment-wide setting
(`KonturSandboxes.SetDefaultShape`, re-read each reconcile tick), then
`orchestrator.DefaultShape()`. What changed is the bottom of that chain:
it is a number rather than a missing flag, so `konturctl` is never asked
to pick a size and no VM's shape depends on which image or which vendored
kontur a host happens to have.

The 30 GiB is a floor rather than a ceiling, and `konturctl` is the one
enforcing that: it refuses a `-disk-size-mb` below the guest image the
overlay reads through to instead of truncating the guest's filesystem.
This repo's own guest is nowhere near it, but a deployment booting an
image larger than 30 GiB has to raise the setting to match — which fails
at create time, naming both sizes, rather than quietly building a VM the
guest does not fit in.

Zero still means "unset" everywhere it is stored and typed — in
`model.Config`, in `model.Task`, in the CLI flags and in an empty box on
the Sandbox tab — it just resolves to grain's default rather than to
kontur's. The visible consequence is that disk finally has a default to
show: `ui.Settings.SandboxDiskGBDefault` sits beside the two that already
existed, so the pane's disk field gets the same faint placeholder as
vCPUs and memory instead of a sentence explaining why it could not have
one, and `grain settings` prints `sandbox disk gb: 30 (grain default,
unset)` where it used to print `unset`. The memory placeholder moved with
the constant behind it, from 2048 to 8192.

A deployment that wants something else sets it, exactly as before: 8 GiB
and 30 GiB are a default chosen for a build-and-test agent, not a claim
about anyone's host. A per-task override is still the escape hatch for
the one job that needs more, and still the only way to ask for *less*
than the deployment's own shape.

## `top`, in the System pane

Sandbox health can say the daemon's machine is under pressure. It has
never been able to say by what. Load average, memory and disk are
aggregates by construction — `pkg/sysstat` reads `/proc/loadavg` and
`/proc/meminfo`, neither of which knows a process from another — so the
pane could show a load of 12 and leave the only question anybody actually
has at that point ("which process?") to an SSH session and a terminal.

The System pane has a Top tab now: `GET /api/host/top`, `pkg/hosttop`,
`top` itself. Shelling out rather than walking `/proc/[pid]/stat` and
reimplementing its accounting is the same call `pkg/systemlog.Journalctl`
already made for `journalctl` — `procps` is a package, the output is what
an operator already knows how to read, and there is nothing to keep in
step with the kernel. `Dockerfile` lists it among the binaries the image
carries for exactly this reason, and `tests/container` fails the image if
it is missing.

Three details that are not the obvious spelling:

- **Two iterations, not one.** `top -b -n 1` prints, for every process, its
  share of CPU time *since that process started* — there is no earlier
  sample to difference against. For a daemon up for a week that is a
  number about the week, and the busiest thing on the machine right now
  can sit at the bottom of it. `pkg/hosttop` asks for two samples half a
  second apart and returns only the second, which is a real delta;
  `lastSample` finds it by the `top - ` header that opens each iteration.
- **Cut from the end.** The sort is `%CPU`, descending, so truncating to
  the requested line count drops the idle processes and keeps the summary
  block and the rows worth reading. A caller that asks for nothing gets
  `hosttop.DefaultLines`; `?lines=` is capped at 500, the same guard
  `GET /api/logs` gives its own.
- **Auto-refresh is a checkbox.** Logs needs no such thing: a log is
  append-only, and a poll only adds lines below whatever is being read.
  Every poll here re-sorts the whole table under the cursor, which is
  exactly what one does not want while reading a row — and it costs a
  `top` run on the daemon's machine each time. So the poll can be stopped
  without leaving the tab, and stops on its own when the tab is not open.

What it lists is the daemon's own PID namespace, which in a container
deployment is the container's: `scripts/setup.sh` passes no `--pid=host`,
so this is `grain` and everything it forked, which is where daemon-side
load comes from. A sandbox's processes live in that sandbox's own VM and
are not here — the per-sandbox rows in the panel next door are what
report those. A deployment with no reader wired in (`grain demo`, an
image without `procps`) gets the same "not available" note every other
optional panel already shows, rather than a pane that could only ever
error.

## The kernel log, in the System pane

Top says which process is wearing the machine out. The failure it cannot
describe is the one where the process is *gone*: an agent CLI picked off
by the OOM killer mid-run, a disk erroring under a sandbox's checkout, an
interface dropping the git proxy's traffic. None of those write a word to
the daemon's own journal — the thing that would have written it is what
the kernel stopped — so the Logs pane could show three sources that all
went quiet at the same second and no way to ask why.

`dmesg` is a fourth source in that same dropdown now
(`GET /api/logs/dmesg`, `pkg/systemlog.Dmesg`). It is a `LogSource` like
the other three rather than a tab of its own: the kernel ring buffer is
lines of text with timestamps, which is exactly what the Logs pane
already is, and putting it beside `daemon` means comparing the two is a
change of dropdown rather than a change of pane.

It reads that log with `journalctl --dmesg`, not `dmesg(1)`. The two
print the same messages for the same boot — `--dmesg` implies
`journalctl`'s own `-b` — but only one of them is reachable from where
the daemon runs. `dmesg(1)` goes to the ring buffer through `/dev/kmsg`
or `syslog(2)`, and the daemon's container is given neither: it runs
unprivileged as `$GRAIN_USER`, with no `CAP_SYSLOG` and no such device
(`scripts/setup.sh`'s `docker_run_args`). Getting it working would mean
adding `util-linux` to the image *and* handing the container a capability
it has no other use for. journald has already collected those same
kernel messages into the host journal that `docker_run_args` bind-mounts
in read-only for `Journalctl`, `systemd-journal` group and all — so the
kernel log arrives through a mount and a group the deployment already
has, and `Dockerfile` needs nothing it does not already carry. Both
sources are one `journalctl` helper now, differing in the matcher they
pass and the name their errors carry.

The consequence worth knowing: this is the *host's* kernel log, not a
sandbox's. Every kontur guest runs its own kernel, and an OOM inside one
of those is that guest's business. What shows up here is what happened
to the machine the daemon and every sandbox share — which is the level
the questions that reach this pane are asked at.

## A root shell, from the UI

Every panel of the System pane answers one fixed question. Logs says what the
daemon, the proxy, config-sync and the kernel wrote down; Top says which
process is spending the machine; Sandbox health says how loaded it is and
what each slot is holding. The failure that reaches an operator anyway is
the one none of those has a column for — a disk filled by something no
panel counts, a unit that will not come back, a container the daemon
cannot see past — and the answer to it has always been the same: get a
shell on the host and look.

That is the pane: System → Root shell, one command at a time, run as root
on the machine the daemon runs on, with its output read back in the
browser (`POST /api/host/shell`, `pkg/rootshell`, `ui.Config.RootShell`).
A scrollback of everything run since the pane was opened, because the
output of the third command means nothing without the two above it. A
command that fails is an answer rather than an error: it comes back with
its exit code beside its output, since "the unit is dead and here is why"
is what the question was.

**Why it is in here at all**, given that an operator with an SSH session
needs none of it. Because the deployments worth opening this pane for are
the ones where that session is exactly what is missing: a host that
stopped accepting connections, a VM in a cloud project whose console is
three support tickets away, a machine reachable only over the tailnet
this same UI is served on (`GRAIN_TAILSCALE_ENABLE`). The UI is the one
surface that is definitionally still up when grain itself is up — it is
what stays serving when the reconcile loop has died
(`Config.ReconcilerDown`) — so it is the right place for the hatch that
gets used on the day nothing else works.

**The daemon cannot run the command itself.** It is a container process
running unprivileged as `$GRAIN_USER`, with no `sudo` in the image and no
`systemctl` that reaches a systemd that matters (`scripts/setup.sh`'s
`docker_run_args`); a `bash -c` from in there is neither root nor on the
host, which is both of the things this pane is opened for. So it asks
instead of acting, over the control channel the reboot button and the
Upgrade button's restart already use: the daemon writes the command to
`$GRAIN_DATA_DIR/control/shell`, a systemd `.path` unit out on the host
notices, and `grain-shell.service` runs it as root and writes back what
it printed.

The exchange is three files, and the ordering is the whole protocol:
`shell` (the command), `shell.out` (stdout and stderr interleaved, as a
terminal would show them), and `shell.status` (the exit status, written
*last* and renamed into place). A status file existing means the output
beside it is finished, so the daemon side never reads a half-written
answer and never has to guess. Both answer files are written under a
`077` umask and chowned to the control directory's owner, so what a root
command prints is readable by the daemon that asked for it and by root
and by nobody else on the box. The responder consumes the request before
running it, so a command that reboots the host is not run a second time
on the way back up, and `TimeoutStartSec` bounds one that never returns
at all.

**What this grants, said plainly.** The two units beside this one each
run one fixed command — `systemctl reboot`, `systemctl restart
grain-daemon.service` — and that narrowness was the argument for
replacing the `NOPASSWD` sudoers drop-ins they came from. This one runs
whatever it is handed. It is unrestricted root on the host, held by
whatever can reach the UI, and the UI carries no auth of its own: on a
default deployment that is loopback, and on a tailnet deployment it is
everyone the tailnet ACL lets in. There is no way to make a debug hatch
narrower than the failures it is for, which is the honest reason it is
this wide rather than a list of allowed commands.

It is on by default anyway (`GRAIN_ROOT_SHELL=0` turns it off, and a
re-run with `0` removes a responder an earlier run installed). A hatch
that has to be installed in advance is one that is not installed on the
day it is wanted — which is precisely the day the host stopped letting
anyone in to install it — and this same UI could already reboot the
machine out from under every running task. What the default buys is that
the pane is there when the deployment is broken; what it costs is the
paragraph above, which is why `scripts/setup.sh` says it too, at the
variable.

Two smaller decisions worth recording. Every command is logged to the
daemon's journal before it runs, and that is the only record there is:
the exchange leaves no file behind and the host-side unit's own journal
names the unit rather than the command. And the warning above the prompt
is standing, not a confirm per command — a confirm is clicked through by
the second command, and this pane is used a dozen short commands at a
time. Each of those is its own `bash -lc`, so a `cd` does not persist
between them; the pane says so rather than leaving it to be discovered.

A deployment with no responder behind the route says so on the tab, off
`GET /api/config`'s own `rootShellEnabled`, rather than offering a prompt
whose every command could only 404 — the same nil-means-unavailable
contract every other optional panel here follows. A host deployed before
any of this existed is in exactly that state until `sudo ./setup.sh` is
re-run, and `pkg/rootshell`'s timeout message names the units to install
rather than only reporting that nothing answered.

## Nested virtualization, and saying whether a sandbox has it

A sandbox is a cloud-hypervisor guest, so a VM started *inside* one is a
nested VM: `kind create cluster`, a docker runtime that wants `/dev/kvm`,
a second cloud-hypervisor for a task working on kontur itself. The
question "can a dispatched task do that here?" had no answer anywhere in
this repository, and it is a question with three separate ways of being
no, in three different layers, each of which looks identical from inside
a task.

The three, top to bottom:

- **The host's KVM has to have nesting on.** `kvm_intel` and `kvm_amd`
  both default to `nested=Y`, so a physical machine has this without
  anyone asking. A cloud VM has it only if its own hypervisor gave this
  kernel hardware virtualization to pass on — `terraform/gcp`'s
  `enable_nested_virtualization` is what buys that, and buys it once:
  a GCE VM's guests do not in turn get VMX to hand to *their* guests.
- **cloud-hypervisor has to leave `vmx`/`svm` in the guest's CPUID.** It
  does. `--cpus nested=` defaults to *on* in v53.0, the version
  `third_party/kontur`'s `Dockerfile` pins, and `nested=off` is the
  setting that masks the flag (`arch/src/x86_64`'s `configure_vcpu`).
  Nothing here passes it, so nothing had to change to get this — and
  nothing in the vendored kontur had to be patched, which is the outcome
  `third_party/kontur/VENDORED.md` asks for.
- **The guest has to load its `kvm` module and let the sandbox account
  open `/dev/kvm`.** udev autoloads `kvm_intel`/`kvm_amd` off the CPU's
  own feature bits, so the module half happens by itself. The device it
  creates is `root:kvm 0660`, and the `debian` account every tool call
  arrives as was in no such group.

That last one was the only thing actually wrong, and it was wrong
invisibly: a real sandbox on the physical deployment has `vmx` in
`/proc/cpuinfo`, has `kvm_intel` loaded, and will run a VM built with
`KVM_CREATE_VM`/`KVM_CREATE_VCPU`/`KVM_RUN` as root — while refusing the
same device to the account the agent is. `scripts/kontur/guest-setup.sh`
adds that account to the `kvm` group, so the next guest image CI
publishes has nested virtualization usable rather than merely present.

The rest of the change is the reporting, because a guest image is built
and published on its own schedule: a deployment can be running the old
image or the new one, and the sandbox health pane is where an operator
finds out which. `KonturSandboxes.Health` asks the guest one more
question inside the single command it already runs there — no extra
round trip against its 5-second budget — and reports one of four states
rather than a boolean, because the three failures want different fixes:

| state | means | fix |
| --- | --- | --- |
| `ready` | `/dev/kvm` is there and the sandbox account can open it | — |
| `denied` | the device is there, the account cannot open it | a guest image from before the `kvm` group grant |
| `no-device` | the CPU offers `vmx`/`svm`, nothing loaded the module | a guest image without `kvm_intel`/`kvm_amd` |
| `unsupported` | no `vmx`/`svm` in the guest's CPU at all | the layer below: the host's own nesting |

The host's own half is read separately — `pkg/sysstat`'s
`NestedVirtualization`, straight off
`/sys/module/kvm_{intel,amd}/parameters/nested` — and shown on the same
pane beside the load average, because it is what tells an `unsupported`
sandbox from a host that was never going to give it one. `scripts/setup.sh`'s
`ensure_kontur_nested_virt` logs the same reading at install time and, on
a host where something turned nesting off, writes the modprobe drop-in
that turns it back on, reloading the module only if nothing is using it
— never pulling KVM out from under a running sandbox, since that script
is the updater as well as the installer.

## A sandbox shape the backend ignores says so

`grain settings` and the Settings pane's Sandbox tab print three numbers
on every deployment — `sandbox cpus`, `sandbox memory mb`, `sandbox disk
gb` — because they are stored on every deployment. Only one kind of
deployment builds anything to them. Without `-kontur-sandboxes` a run
gets an `orchestrator.HostSandboxes` directory on the daemon's own
machine: `liveConfig.refresh` offers a changed shape to whatever
implements `defaultShaper`, that backend deliberately does not, and the
three settings size nothing. Read from inside such a sandbox, `cpu.max`
and `memory.max` are both `max` and `nproc` reports every core the host
has.

Shown unannotated, that is worse than showing nothing. "2 vCPUs, 2048
MiB" is a sentence about a cap, and it was describing a deployment whose
runs have no cap at all — an operator reading the pane would have
concluded the opposite of the truth, and would have gone looking for the
throttle when a run ate the machine. The per-task half of the same
setting was already honest about it: `HostSandboxes.Acquire` *refuses* a
non-zero shape rather than ignoring it, naming both what was asked for
and why a host directory has none of its own. The deployment-wide
default was the one number still being dropped in silence.

`ui.Config.SandboxShapeIgnored`, reported onward as
`ui.Settings.SandboxShapeIgnored`, is that fact where the numbers are
shown: the Sandbox tab carries a warning above the three fields, and
`grain settings` ends each of the three lines with `-- not in effect:
host-directory sandboxes have no shape`. Per line rather than once
underneath them, since whoever is misled here reads one line and stops.

The daemon works it out from the backend it built rather than from the
flag that chose it — `sandboxShapeIgnored` asks whether `sandboxes` is a
`defaultShaper`, which is exactly the test `refresh` applies before
handing a changed shape on — so what the pane calls "not in effect"
cannot drift from what is really applied. A backend that grew a
`SetDefaultShape` would stop being annotated on the same commit that
made the annotation false.

The fields stay editable and the values stay stored. They are what this
deployment would build VMs at the day it is pointed at kontur, and
configuring a shape ahead of that switch is a reasonable thing to do:
what was wrong was the silence, not the numbers. `false` is the quiet
answer, so a UI with no backend to speak for (`grain demo`'s throwaway
one) and an older daemon that predates the field both render the pane
exactly as it was.
