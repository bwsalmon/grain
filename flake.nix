{
  # SUPERSEDED AS THE PRIMARY PATH — see docs/design.md. Retained for a
  # future Linux host, or to validate the Linux design under nested virt.
  description = "Agent cluster — spike 0 (Linux path): microVM guest with docker and kind";

  inputs = {
    # Deliberately unstable for the spike; pin to a stable release
    # (github:NixOS/nixpkgs/nixos-XX.YY) once the spike passes.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    microvm = {
      url = "github:microvm-nix/microvm.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, microvm, ... }: {
    nixosConfigurations.spike-host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        microvm.nixosModules.host
        ./hosts/spike/configuration.nix
      ];
    };
  };
}
