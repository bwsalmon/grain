# v2

The rewrite, in Go. `grain/` is v1 and still the thing that runs; nothing
here is wired into it.

```
pkg/model/      the task model of ../docs/data-model.md
pkg/model/dolt/ opening an embedded Dolt database — the only package that
                imports Dolt
pkg/dispatch/   which task takes which slot: what one cycle decides to
                do with the store, with no side effect beyond that
                decision. It does not loop itself -- cmd/graind's timer
                does, through pkg/orchestrator -- and it carries no
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
cmd/mcpserver/  the server as a standalone stdio binary -- -sandbox-root
                for NewSandboxTools, or -kontur-vm (plus pkg/kontur, below)
                for NewSSHSandboxTools against a real kontur-managed VM
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
                this points --mcp-config at a built cmd/mcpserver binary
                the same way v1's dispatch.py pointed it at
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
pkg/orchestrator/  v1's core.py/Orchestrator equivalent: polls a task
                repo's labelled issues into model.Store tasks, runs
                dispatch.Cycle's own dispatches (resolving and
                materializing each one's capabilities first, and revoking
                what was minted once it finishes), turns a finished run's
                tool
                calls into real GitHub effects (a comment, a pull
                request, a filed follow-up task), and closes out a
                pull request once GitHub reports it merged or closed.
                See "What this does not have yet" below for what it
                still stops short of.
e2e/            issues filed the way a user would, carried through
                dispatch.Cycle, a real agent/gemini run, and a real
                gitproxy push, against a real embedded Dolt store and a
                local git
                server standing in for GitHub — fixed scenarios plus a
                randomized multi-user simulation (bwsalmon/agents#233).
                See "What this does not have yet" below for where it
                stops.
cmd/graind/     the daemon: pkg/orchestrator's RunCycle run on a timer
                against one real embedded Dolt store, until
                SIGINT/SIGTERM, with an in-process gitproxy and a real
                github.RESTClient wired in
pkg/ui/         a JSON API, and the static frontend it serves, for
                creating and managing tasks and their capability grants
                by hand (bwsalmon/agents#237) -- see "The UI" below for
                why it talks straight to GitHub rather than through a
                store or an orchestrator
cmd/ui/         the UI as one binary: pkg/ui.Server behind a local HTTP
                listener, opening the system's default browser
cmd/grain/      a CLI over pkg/ui.Client -- the same model code cmd/ui's
                Server wraps in JSON and HTTP, driven from a terminal
                instead: list/get/create/update a task, approve, attach
                or detach a capability, comment, close ("delete" -- a
                GitHub issue has no such endpoint through an ordinary
                token) or reopen one (bwsalmon/agents#271)
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
and the GCP Go SDK would retire the `gcloud` exception) but `libicu74`
and the C++ runtime take their place.

**Embedded Dolt serves one database per directory**, so naming it in the
DSN before it exists fails with "database not found". `Open` therefore
connects twice: once with no database selected purely to create it, then
again for real. Not a `CREATE`-then-`USE` on one connection, which would
be correct only while `MaxOpenConns` is 1 and silently wrong afterwards.

## What this does not have yet

A real host adapter, primarily. There used to be a second gap here too:
two independent packages, `pkg/orchestrate` (bwsalmon/agents#254) and
`pkg/orchestrator` (bwsalmon/agents#249), each decided *when* to call
GitHub's REST API from a running `dispatch.Cycle`, built in parallel
without either knowing about the other. bwsalmon/agents#263 reconciled
them — `pkg/orchestrator` kept its own name and its more complete
GitHub-facing
half (issue intake via `PollIssues`, a finished run's tool calls turned
into a comment/PR/follow-up issue via `ProcessResult`, and closing out a
merged or closed PR via `SyncPullRequests`/`Store.OpenPullRequestLinks`),
and gained what only `pkg/orchestrate` had: `RunDispatch` now resolves and
materializes a dispatched task's capabilities, applies every placement
under its sandbox root, and revokes what was minted once the run
finishes, the same as `pkg/orchestrate`'s own `runDispatch` did — ported
onto `orchestrator.Config`'s new `Capabilities`/`Credentials`/
`MaxAgentTurns` fields. `cmd/graind` now drives `pkg/orchestrator` instead
(a small non-overlapping ticker around `RunCycle`, the same discipline
`pkg/orchestrate`'s own `Reconciler.Run` held to), and `pkg/orchestrate`
itself, along with the `model.TrackedTarget`/`Store.TrackedTargets` it
alone used, is deleted — `Store.OpenPullRequestLinks` (below) already
covered the same "which pull request should grain still be watching"
question, more precisely, so keeping both was pure duplication.

`graind` still defaults to the same "no host adapter" stand-in every
other package here does: one local directory per slot doing sandbox duty
(`orchestrator.HostSandboxes`). `orchestrator.KonturSandboxes`
(bwsalmon/agents#262) is the real alternative `Deps.Sandboxes` also
accepts — one bwsalmon/kontur-managed VM per dispatch slot, reached over
SSH via `mcp.NewSSHSandboxTools` instead of a local directory, created
via `kontur.Create` on first use and reused across cycles the same way
`HostSandboxes` reuses its directories — and `cmd/graind` can now be
pointed at it for real: `-kontur-vm-name-prefix` opts a deployment in,
with `-kontur-ssh-user`/`-kontur-ssh-key`/`-kontur-workspace` for the SSH
side and repeatable `-kontur-create-arg` flags building
`KonturConfig.CreateArgs` (bwsalmon/agents#274) — a deployment's own
`kontur vm create -h` decides what those are, most importantly whichever
flag points at a built guest image (`../packer/kontur/`, below), since
that flag's name is owned by bwsalmon/kontur's own CLI and still hasn't
been reachable to confirm from this repo. `KonturSandboxes.
ConfigureGitCredentials` (new alongside the flags) is the SSH equivalent
of the `mcp.ConfigureGitCredentials` call `graind` already made once per
slot for `HostSandboxes` — over `mcp.ConfigureGitCredentialsOverSSH`
instead of `os.WriteFile`, since an SSH-backed slot has no local directory
for `graind` to write into. A kontur VM's own guest image is still
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
fake. `pkg/orchestrator` decides when to call any of it: it polls a
labelled issue into a `Task` row, dispatches it through
`dispatch.Cycle`, opens or reuses a pull request once a run pushes, and
closes one out once GitHub reports it merged or closed. Its
`live_test.go` drives the same two
scenarios `e2e/e2e_test.go` already proved by hand (a push that becomes a
merged, closed PR; a question that parks a task and a labelled reply that
resumes it) through `orchestrator.RunCycle` and a real `github.Client`
against `githubsim` instead. This absorbed a second, independently-built
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
the gcp capability" — `cmd/graind` is now the executor that does that
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
and `cmd/graind` now calls it for real from `pkg/orchestrator`'s dispatch
loop rather than only from a test — `orchestrator.HostSandboxes` is the
only other thing `dispatch.Cycle`'s own dispatch path drives, and
neither
hands it more than a local directory to confine itself to yet
(`mcp.ConfigureGitCredentials` sets that directory's git credentials up
the same way v1's `configure_git_credentials` sets a real sandbox's up,
once per slot at `graind` startup). `cmd/mcpserver` itself can now be
pointed at a real remote VM instead — `-kontur-vm` resolves a
bwsalmon/kontur-managed VM's SSH endpoint (`pkg/kontur`: the external port
kontur persisted at `kontur vm create` time, plus the pod IP that port
answers on, asked of containerd via `crictl` since kontur has no
apiserver to have recorded it anywhere itself), `mcp.NewSSHSandboxTools`
runs the same four tools — `run_command`/`read_file`/`edit_file`/
`write_file` — against it instead of a local directory (`cmd/mcpserver`
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
`cmd/graind` still defaults `Deps.Sandboxes` to `HostSandboxes`, but
`-kontur-vm-name-prefix` (and the rest of its `-kontur-*`/
`-cri-runtime-endpoint` flags, see "What this does not have yet" above)
now opts a real deployment into `KonturSandboxes` instead — the flag that
picks the image lives in `-kontur-create-arg`, repeated once per
`kontur vm create` flag/value pair a deployment's own `kontur vm create
-h` calls for, rather than a name this repo guesses at.

A real `github.RESTClient` exists and is wired into `graind` too, driving
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

`pkg/ui`/`cmd/ui` (bwsalmon/agents#237) is a first cut at
[`docs/data-model.md`'s "first-party UI"
direction](../docs/data-model.md#direction-a-first-party-ui): create a
task, approve a proposed one, attach or remove a capability, comment,
close/reopen -- everything a human does by hand to a task issue today,
from a form instead of a body of directive lines and a label picker.

**It talks straight to GitHub, not through a store.** That direction's
own "the UI is not a fourth record" rule says a UI reads declarations
from the repo, grain's own acts from the store, and outside facts from
GitHub through grain -- but `pkg/ui` predates `cmd/graind` driving
`pkg/orchestrator.PollIssues` on a timer (bwsalmon/agents#263), and still
reads and writes a task issue directly rather than through a
`model.Store`, the same as it always has: an operator working through
`pkg/ui` and `graind`'s own reconcile loop are now two independent paths
that can both touch the same GitHub issue, with nothing reconciling the
two. `pkg/ui` reads and writes it directly, through the same
`github.Client` interface `cmd/graind` and `pkg/orchestrator` use -- one
`Config{TaskRepo, Labels, Capabilities}` naming which repo and which
label taxonomy (`grain/automation/labels.py`'s own defaults, ported as
`ui.Labels`/`ui.Capability`), not a copy of anything the store or the
repo owns. `State` is derived off labels on every read, the same "never
stored" discipline `model.StateOf`'s own doc comment describes for the
store-backed version. `pkg/ui`'s own `Config`/`Server` seam is exactly
where a `model.Store`-backed implementation would slot in behind the
same JSON API, without the frontend knowing the difference, if the two
paths are ever unified.

**No OAuth.** The direction document calls for GitHub OAuth plus
`author_association` as the permission gate; `cmd/ui` instead takes a
single GitHub token (`-github-token-file`, or `$GITHUB_TOKEN`) the way
every other `-github-*` flag across `v2/cmd` does, because this is a
single-operator tool run locally against a token that operator already
holds, not a hosted multi-user service -- the OAuth gate is worth
building the day this runs anywhere other than one person's own machine
(bwsalmon/agents#237's follow-up).

**Why a local web server, not Electron/Tauri/a native app.** `go build`
already produces one dependency-free binary per OS `cmd/ui` runs on
(Mac, Linux today); a `net/http` server that opens the system's default
browser gets "runs standalone on Mac and Linux" for free, in the one
language every other substrate here already commits to (see "Why Go"
above), with no second toolchain (Node, Rust, Xcode) for this repo to
carry. "Set up to run on iOS/Android in the future" is what shapes
`pkg/ui` into an HTTP+JSON API in the first place rather than
server-rendered pages or a Go-templated app: a future mobile client --
native, or a thin webview shell -- is just another caller of the same
`/api/*` surface `cmd/ui`'s own frontend (`pkg/ui/static/`, plain
HTML/CSS/JS, no build step) already uses, with nothing about the server
to rewrite.

**Freshness, not a cache.** Every mutation in the frontend
(`pkg/ui/static/app.js`'s `act`) re-fetches the task afterward rather
than assuming its own optimistic update is now true, matching the
direction document's "it shows freshness for anything" read live from
GitHub rather than presenting a stale value as current -- there is
nowhere here for staleness to hide since nothing is ever cached across
one request.

## Single writer

Embedded Dolt permits one writer, which suits a cron-driven controller
and does not suit a controller plus a UI plus a human at a CLI. When that
becomes real the answer is a Dolt SQL server, `pkg/model/dolt` grows a second
constructor, and nothing above it changes — which is why `Store` takes a
`*sql.DB` and imports no driver.
