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

1. **Add a `hardware-configuration.nix`.** This config deliberately has no
   root filesystem or bootloader, because both are machine-specific.
   Generate them with `nixos-generate-config` and add the result to the
   flake's module list, or evaluation fails with *"The `fileSystems` option
   does not specify your root file system"*.
2. **Set the uplink.** In `hosts/spike/configuration.nix`, uncomment and set
   `networking.nat.externalInterface` to the host's real outbound interface.
   Without it the guest can't pull container images.
3. **Check the nixpkgs pin.** `flake.nix` tracks `nixos-unstable`; pin a
   stable release before this becomes anything real, and set both
   `system.stateVersion` values to match it.
4. **KVM is required.** cloud-hypervisor has no software-emulation mode.

> **Status: evaluates, not yet booted.** Nix was installed in the authoring
> environment and this configuration now evaluates cleanly against nixpkgs
> `nixos-unstable` (26.11pre1058091) and microvm.nix HEAD `71beea0` —
> every microvm option name is confirmed correct. It has **not been
> booted**: that environment had no `/dev/kvm` and no nested virtualization,
> and cloud-hypervisor requires KVM. Booting is the part that still needs
> your hardware.

## What evaluation already told us

Three findings came out of evaluating it, without running anything:

1. **The guest kernel is stock.** microvm.nix does not build a slimmed
   kernel here — the guest gets nixpkgs' standard `linux-6.18.44`. That
   substantially de-risks
   [open question 1](design.md#open-questions); the fear was a minimal
   kernel config missing what docker and kind need.
2. **Kernel support is present**, checked directly against the guest's
   `linux-config-6.18.44`:

   | Group | Result |
   |---|---|
   | `OVERLAY_FS`, `BRIDGE`, `BRIDGE_NETFILTER`, `VETH`, `NF_CONNTRACK`, `NF_NAT` | present (modules) |
   | namespaces — `NET_NS`, `PID_NS`, `IPC_NS`, `UTS_NS`, `USER_NS` | built in |
   | cgroups — `CGROUPS`, `CGROUP_BPF`, `MEMCG`, `CPUSETS`, `CGROUP_PIDS`, `CGROUP_DEVICE`, `CGROUP_FREEZER` | built in |
   | `SECCOMP`, `SECCOMP_FILTER`, `KEYS`, `POSIX_MQUEUE`, `IP_VS` | present |

   **One gap to watch.** The legacy iptables symbols `IP_NF_FILTER`,
   `IP_NF_NAT`, and `IP_NF_TARGET_MASQUERADE` are absent from this kernel
   config, while `IP_NF_IPTABLES` and the whole `nf_tables` stack
   (`NF_TABLES`, `NFT_NAT`, `NFT_MASQ`, `NF_TABLES_IPV4`) are present.
   Docker and kube-proxy work over the nftables backend, so this is most
   likely fine — but it is the specific thing to watch if `docker run` or
   `kind create cluster` fails on networking, and it's why the check below
   still has to be run for real.
3. **cloud-hypervisor needs a vsock CID to report readiness.** Evaluation
   warned that without `microvm.vsock.cid` the host cannot tell when a
   guest is actually up. That matters directly for
   [on-demand start](design.md#start-sandboxes-on-demand), where "started"
   has to mean "ready to take work" — so it is now set, and each VM in the
   real pool needs a distinct CID.

## Running it

```sh
nix flake check                     # should pass once hardware config is added
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
| `overlay`, `br_netfilter`, `nf_conntrack` present | Confirmed present in the kernel config already; a failure here means they didn't load, not that they're missing |
| inotify limits raised | sysctl didn't apply — fixable |
| `docker run hello-world` | Daemon can't start: usually storage driver or cgroup delegation |
| `kind create cluster` | The actual question. Investigate before proceeding |

**`ALL CHECKS PASSED` means the architecture holds** and the rest of
[the plan](design.md#implementation-plan) can proceed as written.

The `docker run` and `kind create cluster` steps are now the ones carrying
real uncertainty. Static inspection got the kernel question most of the way
down; what it cannot tell us is whether the pieces work together at
runtime, and the legacy-iptables gap noted above is the most likely place
for that to surface.

## Also worth capturing while here

Since this is the first thing on real hardware, it's cheap to answer two
other open questions at the same time:

- **Boot time.** [On-demand start](design.md#start-sandboxes-on-demand)
  assumes roughly a second. Time `systemctl start microvm@sandbox-0` and
  find out.
- **Idle and peak memory.** [Sizing](design.md#sizing) assumes ~8 GB per
  active sandbox with a kind cluster up. Measure it, since
  `sandboxCount` follows directly from that number.
