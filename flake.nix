{
  description = "nb-nix: declarative NetBird management settings from Nix (plan/apply)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # Must match the github.com/netbirdio/netbird version in go.mod (checked by checks.netbird-version).
    netbird-src = {
      url = "github:netbirdio/netbird/v0.81.4";
      flake = false;
    };
  };

  outputs = {
    self,
    nixpkgs,
    netbird-src,
  }: let
    inherit (nixpkgs) lib;
    systems = ["aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux"];
    forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

    # The NetBird version nb-nix is built against, read from go.mod.
    netbirdVersion = let
      m = builtins.match ".*github.com/netbirdio/netbird v([0-9.]+).*" (builtins.readFile ./go.mod);
    in
      builtins.head m;
  in {
    lib = {
      inherit netbirdVersion;
      nixosModule = ./nix/module.nix;
      # Evaluate nb-nix modules. Returns the module system result; the desired
      # state is config.netbird.desired (attrs) and config.netbird.desiredFile (JSON file).
      evalConfig = {
        pkgs,
        modules,
      }:
        lib.evalModules {
          modules = [./nix/module.nix] ++ modules;
          specialArgs = {inherit pkgs;};
        };
    };

    packages = forAllSystems (pkgs: let
      netbirdComponent = componentName:
        (pkgs.netbird.override {inherit componentName;}).overrideAttrs (_: {
          version = netbirdVersion;
          src = netbird-src // {name = "source";};
          vendorHash = "sha256-e6gG99OhfYcygQIqKyC9M4TajbH/6+L/IoMa3lzoq5k=";
          # nixpkgs' versionCheckHook expects the release version string.
          doInstallCheck = false;
        });
    in {
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.nb-nix;
      nb-nix = pkgs.buildGoModule {
        pname = "nb-nix";
        version = "0.1.0";
        src = lib.fileset.toSource {
          root = ./.;
          fileset = lib.fileset.unions [./go.mod ./go.sum ./cmd ./internal];
        };
        vendorHash = "sha256-UYRFE+68J/SrkGCvQ4uL0A8K23z8K03mrYBmjgDPRoQ=";
        proxyVendor = true;
        # Only cmd/nb-nix installs a binary; the others are listed so their tests run.
        subPackages = ["cmd/nb-nix" "internal/engine" "internal/nbapi"];
        env.CGO_ENABLED = "0";
        meta.mainProgram = "nb-nix";
      };
      # Combined NetBird server (management + signal + relay) at the pinned version,
      # used by `just server-test` and the VM integration test.
      netbird-server = netbirdComponent "combined";
      netbird-client = netbirdComponent "client";
      example-desired = (self.lib.evalConfig {
        inherit pkgs;
        modules = [./examples/basic.nix];
      }).config.netbird.desiredFile;
    });

    checks = forAllSystems (pkgs: let
      system = pkgs.stdenv.hostPlatform.system;
      p = self.packages.${system};
      genOptions = p.nb-nix.overrideAttrs {
        pname = "nb-nix-gen-options";
        subPackages = ["cmd/gen-options"];
      };
    in
      {
        inherit (p) nb-nix example-desired;
        # The flake's NetBird source and go.mod's NetBird module are the same
        # release, and the committed Nix options match that release's API.
        netbird-version = pkgs.runCommand "netbird-version" {nativeBuildInputs = [pkgs.unzip];} ''
          spec=shared/management/http/api/openapi.yml
          unzip -p ${p.nb-nix.goModules}/github.com/netbirdio/netbird/@v/v${netbirdVersion}.zip \
            "github.com/netbirdio/netbird@v${netbirdVersion}/$spec" > module.yml
          cmp module.yml ${netbird-src}/$spec
          ${genOptions}/bin/gen-options ${netbird-src}/$spec v${netbirdVersion} > options.nix
          diff -u ${./nix/options.generated.nix} options.nix
          touch $out
        '';
      }
      // lib.optionalAttrs (system == "x86_64-linux") {
        integration = import ./nix/vm-test.nix {
          inherit pkgs;
          inherit (p) nb-nix netbird-server netbird-client;
          inherit (self.lib) evalConfig;
        };
      });

    devShells = forAllSystems (pkgs: {
      default = pkgs.mkShell {
        packages = [pkgs.go pkgs.gopls pkgs.just pkgs.jq];
        env.GOTOOLCHAIN = "local";
      };
    });

    formatter = forAllSystems (pkgs: pkgs.alejandra);
  };
}
