# v2

The rewrite, in Go. `grain/` is v1 and still the thing that runs; nothing
here is wired into it.

```
pkg/model/      the task model of ../docs/data-model.md
pkg/model/dolt/ opening an embedded Dolt database — the only package that
                imports Dolt
pkg/loop/       the state transition loop: what one cycle decides to do
                with the store, with no side effect beyond that decision
pkg/mcp/        a port of grain/automation/mcp_server.py: a newline-
                delimited JSON-RPC server exposing the sandbox tools
                (run_command, read_file, edit_file, write_file) and the
                escape-hatch tools (ask_question, comment_on_issue,
                propose_task, add_review_comment)
cmd/mcpserver/  the server as a standalone stdio binary
pkg/agent/      the Framework interface an agent driver implements
pkg/agent/gemini/  Framework via the Gemini API, talking to its own
                in-process pkg/mcp/ server
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
e2e/            issues filed the way a user would, carried through
                loop.Cycle, a real agent/gemini run, and a real gitproxy
                push, against a real embedded Dolt store and a local git
                server standing in for GitHub — fixed scenarios plus a
                randomized multi-user simulation (bwsalmon/agents#233).
                See "What this does not have yet" below for where it
                stops.
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

`TrackedPullRequest`, folders, and anything that reads or writes
GitHub's REST API. `loop.Cycle` decides
which task takes which slot and calls `StartRun`, and nothing past that:
no sandbox gets created, no agent runs. Actually dispatching, and the
host adapter, are still v1 Python — 15,903 lines of it, with 1,239 tests.
Those tests are the asset in a rewrite; the assertions port, the harness
does not.

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
around dispatch. `loop.Cycle` also mints no leases yet — a run's
`Leases` field exists in the schema and `gitproxy` never reads it; the
git proxy authorizes straight off `Task.Target`/`Reads` instead, which
serves the same fail-closed purpose without depending on that field being
populated first.

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
the gcp capability" — but nothing here does that wiring yet: there is
still no executor that constructs a real `CapabilityContext` against a
live deployment, applies a `Placement` to a sandbox, or calls
`Revoke`/`Reap`, all of which need the same host adapter `loop.Cycle` is
still waiting on above.

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

`agent/gemini` can run an agent end to end against `mcp/`'s tools today —
it just has nothing to call it yet outside a test. There is no host
adapter to hand it a real sandbox directory, so `mcp/`'s `run_command`/
`read_file`/`edit_file`/`write_file` are confined to a local directory
rather than the remote VM v1's versions of them SSH into (`mcp.
ConfigureGitCredentials` now sets that local directory's git credentials
up the same way v1's `configure_git_credentials` sets a real sandbox's
up), and there is no GitHub client, so `ask_question`/`comment_on_issue`/
`propose_task`/`add_review_comment` record what they were asked to do
(`mcp.MockSink`) instead of doing it. Wiring either of those up for real,
and calling `agent.Framework.Run` from `loop.Cycle`, is follow-on work
once v2 has a host adapter of its own.

`e2e/` is that whole chain driven by hand, in a test, rather than by
`loop.Cycle` itself: it calls `loop.Cycle` to decide what runs, then
drives `agent/gemini` (scripted in most tests; the real API in
`live_test.go`, gated on `GEMINI_API_KEY`) through a sandbox-stand-in
directory against a real `gitproxy` in front of a local git server, and
plays the part of "the PR opened," "the PR merged" and "a human replied"
with the same `store.Observe` calls a real GitHub-sync component would
make. It proves the pieces already built compose correctly; it does not
close the gap above, since nothing there is wired to run on its own yet.

## Single writer

Embedded Dolt permits one writer, which suits a cron-driven controller
and does not suit a controller plus a UI plus a human at a CLI. When that
becomes real the answer is a Dolt SQL server, `pkg/model/dolt` grows a second
constructor, and nothing above it changes — which is why `Store` takes a
`*sql.DB` and imports no driver.
