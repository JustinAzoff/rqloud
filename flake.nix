{
  description = "rqloud - replicated SQLite over Tailscale";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      # Pre-built binary, built via `make counter` outside of nix.
      counter = pkgs.stdenv.mkDerivation {
        pname = "rqloud-counter";
        version = "0.0.1";
        src = ./.;
        dontBuild = true;
        installPhase = ''
          mkdir -p $out/bin
          cp $src/counter $out/bin/counter
        '';
      };
    in
    {
      packages.${system} = {
        inherit counter;
        default = counter;
      };

      checks.${system}.integration = pkgs.testers.nixosTest (import ./test.nix {
        inherit counter;
      });
    };
}
