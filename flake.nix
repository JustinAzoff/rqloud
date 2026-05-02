{
  description = "rqloud - replicated SQLite over Tailscale";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      rqloud = pkgs.buildGoModule {
        pname = "rqloud";
        version = "0.0.1";
        src = ./.;
        vendorHash = "sha256-leYwH/HgxaEQQq9DTlmGBGyEfRbCdRw08JW3zG7PD4s=";
        subPackages = [
          "cmd/rqloud"
          "examples/counter"
          "examples/todo"
        ];
        env.CGO_ENABLED = "1";

        postInstall = ''
          mv $out/bin/counter $out/bin/rqloud-counter
          mv $out/bin/todo $out/bin/rqloud-todo
        '';
      };
    in
    {
      packages.${system} = {
        inherit rqloud;
        default = rqloud;
      };

      checks.${system}.integration = pkgs.testers.nixosTest (import ./test.nix {
        counter = rqloud;
      });
    };
}
