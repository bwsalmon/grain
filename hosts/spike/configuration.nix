# Spike 0 host: one sandbox microVM, nothing else.
#
# Purpose: answer open question 1 from docs/design.md — does a microvm.nix
# guest kernel support docker and kind? Everything else in the design is
# wasted effort if this fails, so this deliberately contains no orchestrator,
# no proxies, and no pool.
#
# VALIDATED: evaluates against nixpkgs nixos-unstable (26.11pre) and
# microvm.nix HEAD 71beea0, once a hardware-configuration.nix supplies the
# root filesystem and bootloader (see below). NOT booted — no KVM was
# available in the authoring environment. See docs/spike-0.md.
{ config, lib, pkgs, ... }:

# REQUIRED: this config has no root filesystem or bootloader of its own,
# because both are machine-specific. Generate them on the target host with
#   nixos-generate-config --dir /tmp/hw
# and add ./hardware-configuration.nix to the flake's module list.
# Without it evaluation fails with "The 'fileSystems' option does not
# specify your root file system".

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
    # sandbox-spike.nix takes these args and returns a NixOS module; the
    # module system supplies config/lib/pkgs itself.
    config = import ../../modules/sandbox-spike.nix {
      inherit tap guestIp hostIp prefix;
    };
  };

  environment.systemPackages = with pkgs; [ bridge-utils tcpdump ];

  system.stateVersion = "26.05"; # match the nixpkgs release you pin.
}
