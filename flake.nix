{
  description = "Docker Compose for Apple container";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # Apple container only runs on Apple silicon Macs.
      systems = [ "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      packages = forAllSystems (pkgs: {
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.container-compose;
        container-compose = pkgs.buildGoModule {
          pname = "container-compose";
          inherit version;
          src = self;
          vendorHash = "sha256-DpQC5/5MlkTzksC88TU3AZkJW4DxUAsp8EEzUztfPlo=";
          subPackages = [ "cmd/container-compose" ];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X github.com/pedrospdc/apple-container-compose/internal/compose.Version=${version}"
          ];
          meta = {
            description = "Docker Compose for Apple container";
            homepage = "https://github.com/pedrospdc/apple-container-compose";
            license = pkgs.lib.licenses.mit;
            mainProgram = "container-compose";
            platforms = systems;
          };
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.goreleaser
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
