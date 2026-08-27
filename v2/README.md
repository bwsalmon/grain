# v2

The rewrite. `grain/` is v1 and still the thing that runs; nothing here is
wired into it.

## What is here

`model/` — the task model of [`docs/data-model.md`](../docs/data-model.md),
with a Dolt-backed store. `tests/` covers it, and the one test that needs
a real `dolt` binary skips without one.

## Why a separate tree rather than a package under `grain/`

Two reasons, and the second is the larger one.

The core is being rewritten rather than migrated, so v1 and v2 need to
coexist while that happens — v1 keeps dispatching real work, and a
half-migrated `grain/` would mean neither is trustworthy.

And **v2 may not be Python.** Every substrate this design has chosen is
Go: Dolt embeds only in Go, Incus ships a Go client, and beads is a Go
binary. `model/sql.py` exists *entirely* because a Python process cannot
embed Dolt — the hex-literal rendering, the stdin batching, the
defensive JSON parsing and the unverifiable CLI flags are all workarounds
for a language boundary, not for a design problem. A tree that might be
replaced wholesale should not be a package inside a Python one.

## The one edge back into v1

`model/dolt.py` imports `Runner` from `grain.run` — a ten-line Protocol
and a test seam that already exists, borrowed rather than duplicated.
That import is the boundary: it is where a rewrite in another language
would start.

## What survives a language change, and what does not

Worth knowing before more is built here.

| | If v2 becomes Go |
|---|---|
| `model/schema.py` | the DDL is reusable verbatim |
| `model/types.py` | translates mechanically; it is enums and structs |
| `tests/` | the semantics are the asset — the assertions port, the harness does not |
| `model/sql.py` | **deleted** — bind parameters replace it entirely |
| `model/dolt.py` | **mostly deleted** — embedded Dolt replaces the CLI, and transactions replace batching |

That the two modules which would disappear are exactly the two that exist
because of the language is the clearest argument this tree has for
changing it.
