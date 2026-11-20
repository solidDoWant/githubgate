{
  description = "githubgate development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    {
      nixosModules.githubgate-client = import ./nix/githubgate-client-module.nix;

      # A NixOS configuration that uses the module. Only its evaluated
      # settings are used (the container test reads them with `nix eval`), so
      # it never builds.
      nixosConfigurations.githubgate-test-vm = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.githubgate-client
          ({ pkgs, ... }: {
            boot.isContainer = true;
            system.stateVersion = "25.05";
            services.githubgate-client = {
              enable = true;
              package = pkgs.writeShellScriptBin "githubgate-client" "";
              proxyAddress = "10.96.0.50:443";
              proxyCA = "/var/run/githubgate-proxy/ca.crt";
              extraCA = "/var/run/githubgate-extra/ca.crt";
            };
          })
        ];
      };
    } // flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Evaluated as a path flake (`path:...`, or builtins.getFlake on a
        # bare path), Nix copies the whole directory into the store,
        # gitignored build output and caches included, on every evaluation of
        # a changed tree. Git sources carry rev (clean) or dirtyRev (dirty);
        # path sources carry neither, so the dev shell refuses them.
        requireGitSource = value:
          if self ? rev || self ? dirtyRev then
            value
          else
            throw ''
              githubgate: this flake was evaluated as a path flake, so Nix copied the whole directory to ${self.outPath}.
              Run `nix develop` inside the checkout or worktree instead, or use builtins.getFlake "git+file://''${toString ./.}".
              Reclaim the space with: nix store delete ${self.outPath}
            '';

        # The Go toolchain is nixpkgs' Go. go.mod's `toolchain` directive (or
        # its `go` line when there is none) must not ask for a newer one: CI
        # and the agents build with this Go, never with a downloaded
        # toolchain.
        goMod = builtins.readFile ./go.mod;
        goModToolchain = builtins.match ".*\ntoolchain go([0-9.]+)\n.*" goMod;
        goModGo = builtins.match ".*\ngo ([0-9.]+)\n.*" goMod;
        minimumGoVersion =
          if goModToolchain != null then builtins.head goModToolchain
          else if goModGo != null then builtins.head goModGo
          else throw "githubgate: go.mod has neither a `toolchain goX.Y.Z` nor a `go X.Y.Z` line";
        # Pinned to Go 1.26 (1.26.11 or later has the net/http fix from
        # GO-2026-6859). Return to the default `pkgs.go` once nixpkgs has
        # go1.27.4 or later.
        nixpkgsGo = pkgs.go_1_26;
        go =
          if pkgs.lib.versionAtLeast nixpkgsGo.version minimumGoVersion then nixpkgsGo
          else throw "githubgate: go.mod asks for Go ${minimumGoVersion}, but nixpkgs has Go ${nixpkgsGo.version}; update flake.lock (nix flake update nixpkgs) or lower go.mod.";

        # The control plane binaries for envtest
        # (sigs.k8s.io/controller-runtime/pkg/envtest): the dev shell points
        # KUBEBUILDER_ASSETS here, so tests never download binaries.
        # Only the etcd server is needed, built without its own test suite:
        # some of etcd's tests fail in the Nix build sandbox, and then
        # nixpkgs' binary cache has no build to download.
        etcdServer = pkgs.etcd_3_6.passthru.deps.etcdserver.overrideAttrs (_: {
          doCheck = false;
        });
        envtestAssets = pkgs.linkFarm "githubgate-envtest-assets" [
          { name = "kube-apiserver"; path = "${pkgs.kubernetes}/bin/kube-apiserver"; }
          { name = "etcd"; path = "${etcdServer}/bin/etcd"; }
          { name = "kubectl"; path = "${pkgs.kubectl}/bin/kubectl"; }
        ];
      in
      {
        devShells.default = requireGitSource (pkgs.mkShell {
          packages = [
            go
            # golangci-lint refuses to load code whose go.mod asks for a newer
            # Go than golangci-lint was built with. nixpkgs builds it with the
            # newest Go it supports, so it stays ahead of `go`.
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.gnumake
            # git pinned by flake.lock, so local runs and CI use one version.
            pkgs.git
            # gh is deliberately NOT provided here: the agents' VM ships a gh
            # wrapper in ~/.local/bin that fetches a fresh GitHub App token
            # per call, and a dev-shell gh would shadow it on PATH.
            pkgs.jq
            # Agents and the scheduler need python3; the VM image has none.
            pkgs.python3
            pkgs.shellcheck

            # Code generation (make generate): CRDs, RBAC and deepcopy.
            pkgs.kubernetes-controller-tools

            # Kubernetes: kind clusters for integration and e2e, the chart,
            # images.
            pkgs.kind
            pkgs.kubectl
            pkgs.kubernetes-helm
            pkgs.crane

            # Link and anchor check for the Markdown docs (make docs-check).
            pkgs.lychee
          ];

          KUBEBUILDER_ASSETS = "${envtestAssets}";
        });
      });
}
