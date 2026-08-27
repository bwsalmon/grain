# v2

The rewrite, in Go. `grain/` is v1 and still the thing that runs; nothing
here is wired into it.

```
model/          the task model of ../docs/data-model.md
model/dolt/     opening an embedded Dolt database — the only package that
                imports Dolt
loop/           the state transition loop: what one cycle decides to do
                with the store, with no side effect beyond that decision
mcp/            a port of grain/automation/mcp_server.py: a newline-
                delimited JSON-RPC server exposing the sandbox tools
                (run_command, read_file, edit_file, write_file) and the
                escape-hatch tools (ask_question, comment_on_issue,
                propose_task, add_review_comment)
mcp/cmd/mcpserver/  the server as a standalone stdio binary
agent/          the Framework interface an agent driver implements
agent/gemini/   Framework via the Gemini API, talking to its own in-process
                mcp/ server
```

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

`TrackedPullRequest`, folders, the capability provider contract, and
anything that reads or writes GitHub. `loop.Cycle` decides which task
takes which slot and calls `StartRun`, and nothing past that: no sandbox
gets created, no agent runs, no GitHub is touched. Actually dispatching —
along with the git proxy and the host adapter — is all still v1 Python —
15,903 lines of it, with 1,239 tests. Those tests are the asset in a
rewrite; the assertions port, the harness does not.

`agent/gemini` can run an agent end to end against `mcp/`'s tools today —
it just has nothing to call it yet. There is no host adapter to hand it a
real sandbox directory, so `mcp/`'s `run_command`/`read_file`/`edit_file`/
`write_file` are confined to a local directory rather than the remote VM
v1's versions of them SSH into, and there is no GitHub client, so
`ask_question`/`comment_on_issue`/`propose_task`/`add_review_comment`
record what they were asked to do (`mcp.MockSink`) instead of doing it.
Wiring either of those up for real, and calling `agent.Framework.Run` from
`loop.Cycle`, is follow-on work once v2 has a host adapter of its own.

## Single writer

Embedded Dolt permits one writer, which suits a cron-driven controller
and does not suit a controller plus a UI plus a human at a CLI. When that
becomes real the answer is a Dolt SQL server, `model/dolt` grows a second
constructor, and nothing above it changes — which is why `Store` takes a
`*sql.DB` and imports no driver.
