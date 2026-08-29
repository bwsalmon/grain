# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur),
pulled through the grain git proxy at commit `a13a8cc0c5f81fff676ea4c6455818533c354c8c`
(2026-08-28) for bwsalmon/agents#351. `bwsalmon/kontur` is private, and an
ordinary dispatched task's sandbox has no route to it -- the proxy is
default-deny per repo (`grain/proxy/allowlist.py`), and prior tasks
(bwsalmon/agents#267, #274) found it unreachable for exactly that reason.
This copy exists so that limitation stops blocking work in this repo: a
task that needs to confirm `kontur`/`konturctl`'s actual CLI flags, its VM
state file shape, or anything else about how it behaves can read the
source here (e.g. `internal/cli/vm.go`) instead.

It is a plain copy, not a git submodule or subtree, and it is not wired
into any build here -- `v2/go.mod` does not depend on it, and nothing under
`v2/` imports `github.com/bwsalmon/kontur`. `v2/pkg/kontur`'s own package
doc comment already explains why: reading kontur's on-disk state and
shelling out to its CLI is a shallower dependency than importing its
module graph would be, and that reasoning doesn't change just because the
source is now sitting in this repo too. Treat this as documentation, not a
dependency -- it can go stale (there is no automation re-pulling it), so
anything safety- or correctness-critical should still be confirmed against
a live `kontur vm create -h` (or a fresh vendor) rather than assumed
current.
