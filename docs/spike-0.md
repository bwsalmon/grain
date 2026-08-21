# Spike 0 — does a microVM guest run docker and kind?

This is [open question 1](design.md#open-questions) and it gates everything
else. The design assumes agents run `docker` and `kind` *inside* a sandbox
microVM, because [running them in containers is ruled
out](design.md#the-one-genuinely-custom-component). If a microvm.nix guest
kernel can't support them, that assumption needs rethinking rather than
patching — so this is the first thing to run, before any of the
orchestrator, proxy, or pool work.

## What's here

| File | Purpose |
|---|---|
| `flake.nix` | nixpkgs + microvm.nix, one host output |
| `hosts/spike/configuration.nix` | host: bridge, NAT, one VM |
| `modules/sandbox-spike.nix` | the guest: cloud-hypervisor, docker, kind |

Deliberately *not* here: the orchestrator, the git proxy, the metadata
server, the pool broker, the writable store overlay, ephemeral encryption.
Those come after this passes.

## Before running

1. **Set the uplink.** In `hosts/spike/configuration.nix`, uncomment and set
   `networking.nat.externalInterface` to the host's real outbound interface.
   Without it the guest can't pull container images.
2. **Check the nixpkgs pin.** `flake.nix` tracks `nixos-unstable` for the
   spike; pin a stable release before this becomes anything real. Both
   `system.stateVersion` values are guesses — set them to match.
3. **KVM is required.** cloud-hypervisor needs `/dev/kvm`.

> **This Nix has never been evaluated.** It was written without a Nix
> evaluator available, so expect option-name fixes on the first
> `nix flake check`. Lines most likely to need adjustment are marked
> `VERIFY` in the source.

## Running it

```sh
nix flake check                     # expect to fix option names here first
nixos-rebuild switch --flake .#spike-host
systemctl start microvm@sandbox-0
```

Then get a console on the guest and run the check:

```sh
/etc/spike-check
```

## What it tests, and what failure means

`spike-check` walks the chain in dependency order, so the first failure
localises the problem:

| Check | If it fails |
|---|---|
| cgroup v2 mounted | Guest init/cgroup config wrong — fixable in the guest module |
| `overlay`, `br_netfilter`, `nf_conntrack` present | **The real risk.** Guest kernel lacks what docker/kind need — needs a fuller guest kernel, or reconsider the hypervisor |
| inotify limits raised | sysctl didn't apply — fixable |
| `docker run hello-world` | Daemon can't start: usually storage driver or cgroup delegation |
| `kind create cluster` | The actual question. Investigate before proceeding |

**`ALL CHECKS PASSED` means the architecture holds** and the rest of
[the plan](design.md#implementation-plan) can proceed as written.

A kernel-module failure is the one worth taking seriously: it means
microvm.nix's guest kernel is too slim for this workload, and the options
are a custom kernel config, a different guest kernel package, or —
if neither works — revisiting whether microVMs are the right primitive.

## Also worth capturing while here

Since this is the first thing on real hardware, it's cheap to answer two
other open questions at the same time:

- **Boot time.** [On-demand start](design.md#start-sandboxes-on-demand)
  assumes roughly a second. Time `systemctl start microvm@sandbox-0` and
  find out.
- **Idle and peak memory.** [Sizing](design.md#sizing) assumes ~8 GB per
  active sandbox with a kind cluster up. Measure it, since
  `sandboxCount` follows directly from that number.
