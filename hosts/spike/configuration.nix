# Spike 0 host: one sandbox microVM, nothing else.
#
# Purpose: answer open question 1 from docs/design.md — does a microvm.nix
# guest kernel support docker and kind? Everything else in the design is
# wasted effort if this fails, so this deliberately contains no orchestrator,
# no proxies, and no pool.
#
# UNVALIDATED: written without a nix evaluator available. Expect to fix option
# names on first `nix flake check`. Lines marked VERIFY are the ones most
# likely to need adjustment against the pinned microvm.nix.
{ config, lib, pkgs, ... }:

let
  bridge = "br-agents";
  tap = "vm-sb0";
  hostIp = "10.100.0.1";
  guestIp = "10.100.0.10";
  prefix = 24;
in
{
  networking.hostName = "spike-host";
  networking.useNetworkd = true;
  networking.useDHCP = false;

  # --- bridge the sandbox lives on -----------------------------------------
  systemd.network.netdevs."10-${bridge}" = {
    netdevConfig = {
      Name = bridge;
      Kind = "bridge";
    };
  };

  systemd.network.networks."10-${bridge}" = {
    matchConfig.Name = bridge;
    address = [ "${hostIp}/${toString prefix}" ];
    networkConfig.ConfigureWithoutCarrier = true;
  };

  # microvm.nix creates the tap; this enrolls it into the bridge.
  systemd.network.networks."11-${tap}" = {
    matchConfig.Name = tap;
    networkConfig.Bridge = bridge;
  };

  # Outbound NAT so the guest can pull container images. The real design
  # narrows this considerably (see docs/design.md, "Sandbox egress").
  networking.nat = {
    enable = true;
    internalInterfaces = [ bridge ];
    # externalInterface = "eth0";   # SET THIS to the host's real uplink.
  };

  networking.firewall.trustedInterfaces = [ bridge ];

  # --- the sandbox VM -------------------------------------------------------
  microvm.vms.sandbox-0 = {
    # VERIFY: `config` is the inline-guest option in current microvm.nix.
    # sandbox-spike.nix takes these args and returns a NixOS module; the
    # module system supplies config/lib/pkgs itself.
    config = import ../../modules/sandbox-spike.nix {
      inherit tap guestIp hostIp prefix;
    };
  };

  environment.systemPackages = with pkgs; [ bridge-utils tcpdump ];

  system.stateVersion = "24.11"; # VERIFY against the pinned nixpkgs.
}
