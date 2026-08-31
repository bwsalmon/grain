# Vendored from bwsalmon/kontur

This directory is a source snapshot of [bwsalmon/kontur](https://github.com/bwsalmon/kontur)'s
`main` branch, pulled through the grain git proxy at commit
`9a43152b09807814ba1a364fab313a72183f9bac` (2026-08-31). Always vendor
from `main`: grain is kontur's primary consumer, so a change grain needs
belongs on kontur's `main` and reaches here by a resync, not by being
applied to this copy (see "Local patches" below). It was first
vendored at commit `a13a8cc` (2026-08-28, bwsalmon/agents#351), then
partially re-synced to `3cf4f9286402753add8390302cfb7c1fa82e4f81`
(2026-08-30, bwsalmon/agents#477, three files only), fully re-synced to
`71e277ac37e1d28a5e36ce18e9a4d80ae5a7615f` (2026-08-30,
bwsalmon/agents#504, alpine-base support and a native-Go rewrite of
`internal/netshim/setup.go`), fully re-synced again to
`486a8c9a43f4dba1cee5d15fb25cb6068df5c966` (2026-08-30,
bwsalmon/agents#510, a writable qcow2-overlay disk and `kontur exec`),
fully re-synced to `57bf95d223edc839ccfa0447a051024fe88229d9` (2026-08-30,
bwsalmon/agents#534, memory hotplug and a mem-agent), and fully re-synced
again to `5a63863262e9cfb0a5544f36f4e66d247c4058e5` (2026-08-30,
bwsalmon/agents#562, CPU hotplug and a baked-in guest kernel), and fully
re-synced to `5ed4e0017f5337bc3fde3ab8c29ef42dd1dac848` (2026-08-31, the
three local patches going upstream plus the build-time guest setup hook).

## This resync: flat networking mode

`9a43152` is the merge of bwsalmon/kontur#30 into that repo's `main`, and
carries #29 with it. **This snapshot still has no local patches** -- every
file here is byte-for-byte upstream, verified with `diff -r` against a
fresh checkout (which reports only this file). Keep it that way; see
"Local patches" below.

Two changes over `5ed4e00`:

- **Flat networking mode** (bwsalmon/kontur#29). kontur's original mode
  puts each guest on a private in-namespace subnet and shares the
  namespace's single IP between them with nftables DNAT and masquerade
  rules. Flat mode (`NETSHIM_MODE=flat`, `konturctl vm create -net flat`)
  instead splices one guest directly onto the segment the container
  runtime already put the namespace on, where it takes over the address
  *and* MAC assigned to it -- an ingress qdisc plus a `mirred egress
  redirect` filter on each of the external interface and the tap, so the
  kernel moves every frame between them with no bridge, no routing, no
  NAT and nothing copied. From outside, the VM is one ordinary container:
  `-p`, `--network` and DNS all work on the sandbox itself.

  What this repo gets from it: no `-ip`/`-port` to assign, because docker
  assigns the address; and, because a bridge cannot carry one MAC on two
  ports while a splice has no forwarding database to confuse, a guest
  whose MAC is the one the runtime authorized rather than a second one
  appearing behind the same port. `KonturConfig`'s `BaseIP`/`BasePort`
  slot derivation exists only to serve the NAT mode's shared bridge and
  is unused under it.

  It also adds a *second* guest NIC: since the guest now answers to the
  namespace's own address, anything inside the namespace dialing that
  address reaches its own stack, so `kontur exec` and the memory agent
  get a private control link instead. kontur configures the guest side of
  it in a new `kontur-control-net` overlay service -- which is only in
  this repo's guest image because `packer/kontur/build-guest.sh` now
  builds on kontur's rootfs rather than debootstrapping its own.

- **CI** (bwsalmon/kontur#30), including the first automated build of
  kontur's own OCI image. Note `.github/` is now part of this snapshot;
  it is inert here, since GitHub only reads workflows from a repository's
  root `.github/workflows`.

`go build ./...`, `go vet ./...` and `go test ./...` all pass against this
snapshot. Its own kernel-touching tests (the netshim splice and NAT
integration tests) skip without `CAP_NET_ADMIN` and `/dev/net/tun`; they
were run upstream, where CI now runs them as root with
`KONTUR_NETNS_TESTS=required` so they cannot skip silently.

## Why this copy exists

`bwsalmon/kontur` is private, and an ordinary dispatched task's sandbox
has no route to it -- the proxy is default-deny per repo
(`grain/proxy/allowlist.py`), and prior tasks (bwsalmon/agents#267, #274)
found it unreachable for exactly that reason. This copy exists so that
limitation stops blocking work in this repo: a task that needs to confirm
`kontur`/`konturctl`'s actual CLI flags, its VM state file shape, or
anything else about how it behaves can read the source here (e.g.
`internal/cli/vm.go`) instead.

It is a plain copy, not a git submodule or subtree. Nothing under `v2/`
depends on it as *code* -- `v2/go.mod` does not require it and nothing
imports `github.com/bwsalmon/kontur`. Its Dockerfile, however, is built:
`packer/kontur/build-oci-image.sh` builds the runtime image from this
directory, and `packer/kontur/build-guest.sh` builds the sandbox guest
from the same Dockerfile's `guest-artifacts` target. So a resync here
changes what a deployment actually runs, not just what a task can read.

The Go dependency is still deliberately absent, and `v2/pkg/kontur`'s own
package doc comment explains why: reading kontur's on-disk state and
shelling out to its CLI is a shallower dependency than importing its
module graph would be, and that reasoning doesn't change just because the
source is now sitting in this repo too. As *documentation* this copy can
still go stale -- there is no automation re-pulling it -- so anything
safety- or correctness-critical should be confirmed against a live
`kontur vm create -h` (or a fresh vendor) rather than assumed current.

## Local patches

None. Keep it that way: grain is kontur's primary consumer, so "upstream
wouldn't want this" is rarely true here, and a fix made against this copy
has to be re-diffed and re-applied by hand on every resync. Two of the
three just retired (`qcow2.go`, `docker.go`) went undocumented in this
file for a resync or more precisely because they were made against this
copy directly. Landing a change on `bwsalmon/kontur`'s `main` and
re-vendoring from it costs a round trip and nothing else.

If one is genuinely unavoidable -- something specific to how this repo
drives cloud-hypervisor and wrong for upstream -- record it here with
what breaks without it and how that was confirmed, so the next wholesale
copy notices it rather than silently dropping it.
