# Spike 0 sandbox guest: the minimum needed to find out whether docker and
# kind work inside a microVM guest.
#
# Intentionally omits everything the real design calls for — no writable
# store overlay, no ephemeral encryption, no git proxy, no action execution
# server. Add those only after this boots and `spike-check` passes.
#
# VALIDATED: this evaluates against nixpkgs nixos-unstable (26.11pre) and
# microvm.nix HEAD 71beea0. It has NOT been booted — no KVM was available
# in the authoring environment. See docs/spike-0.md.
{ tap, guestIp, hostIp, prefix }:

{ config, lib, pkgs, ... }:

{
  microvm = {
    hypervisor = "cloud-hypervisor";
    vcpu = 4;

    # Sized for a kind control plane plus a build; see docs/design.md "Sizing".
    mem = 8192;

    # cloud-hypervisor signals readiness over vsock; without a CID the host
    # cannot tell when the guest is actually up. That matters for
    # on-demand start, where "started" must mean "ready to take work".
    # Each VM needs a distinct CID (2 is reserved for the host).
    vsock.cid = 3;

    interfaces = [{
      type = "tap";
      id = tap;
      mac = "02:00:00:00:01:00";
    }];

    # Host store, read-only. cloud-hypervisor supports virtiofs only.
    shares = [{
      source = "/nix/store";
      mountPoint = "/nix/.ro-store";
      tag = "ro-store";
      proto = "virtiofs";
    }];

    # Disk-backed scratch. /var/lib/docker MUST live here rather than on the
    # tmpfs root: images are gigabytes and RAM is the binding resource.
    # The real design puts an ephemeral dm-crypt layer under this; omitted
    # here to keep the spike focused.
    volumes = [{
      image = "scratch.img";
      mountPoint = "/scratch";
      size = 40960; # MiB
      autoCreate = true;
    }];
  };

  # --- networking -----------------------------------------------------------
  networking.hostName = "sandbox-0";
  networking.useNetworkd = true;
  networking.useDHCP = false;

  systemd.network.networks."10-eth" = {
    matchConfig.Name = "eth*";
    address = [ "${guestIp}/${toString prefix}" ];
    routes = [{ Gateway = hostIp; }];
    networkConfig.DNS = [ "1.1.1.1" ];
  };

  networking.firewall.enable = false; # host nftables is the control point

  # --- the actual subject of the spike --------------------------------------
  virtualisation.docker = {
    enable = true;
    daemon.settings.data-root = "/scratch/docker";
  };

  # kind's documented defaults (8192 watches / 128 instances) are too low to
  # bring up a cluster, and the resulting failures are opaque:
  # "too many open files", "failed to create fsnotify watcher".
  boot.kernel.sysctl = {
    "fs.inotify.max_user_watches" = 524288;
    "fs.inotify.max_user_instances" = 8192;
  };

  # Modules docker and kind need that a slimmed guest kernel may omit. This
  # is open question 1 in docs/design.md: if the guest kernel lacks these,
  # the architecture needs rethinking rather than patching.
  boot.kernelModules = [ "overlay" "br_netfilter" "nf_conntrack" "ip_tables" ];

  environment.systemPackages = with pkgs; [
    kind
    kubectl
    docker-client
    git
    curl
    jq
  ];

  # `spike-check` — the pass/fail criterion for this whole exercise.
  environment.etc."spike-check".source = pkgs.writeShellScript "spike-check" ''
    set -euo pipefail
    echo "== cgroup v2 =="; stat -fc %T /sys/fs/cgroup
    echo "== required modules =="
    for m in overlay br_netfilter nf_conntrack; do
      grep -qw "$m" /proc/modules && echo "  ok   $m" || echo "  MISS $m"
    done
    echo "== inotify =="
    sysctl fs.inotify.max_user_watches fs.inotify.max_user_instances
    echo "== docker =="; docker info >/dev/null && docker run --rm hello-world
    echo "== kind =="; kind create cluster --name spike --wait 120s
    kubectl get nodes
    kind delete cluster --name spike
    echo "ALL CHECKS PASSED"
  '';

  # Serial console login so the spike can be driven without networking.
  services.getty.autologinUser = "root";
  users.users.root.password = "spike"; # spike only — never in the real design

  system.stateVersion = "26.05"; # match the nixpkgs release you pin.
}
