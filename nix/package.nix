{ lib, buildGoModule }:
buildGoModule {
  pname = "azukari";
  version = "0.0.0-dev";

  # One build holds the three binaries (ADR 0002).
  subPackages = [
    "cmd/azukari-server"
    "cmd/azukari-backup"
    "cmd/azukari"
  ];

  # Only what the Go build and its tests read: editing the docs or the Nix files doesn't rebuild.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      (lib.fileset.maybeMissing ../go.sum)
      (lib.fileset.fileFilter (f: f.hasExt "go") ../.)
    ];
  };

  # No external module yet. Once go.sum exists: set to lib.fakeHash and read the value from the build error.
  vendorHash = null;

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  # `nix flake check` runs this phase: vet first, then the default `go test ./...`.
  preCheck = ''
    go vet ./...
  '';

  meta = {
    description = "Store for the homelab's data, with typed buckets, starting with backups";
    homepage = "https://github.com/scorsi/azukari";
    mainProgram = "azukari";
  };
}
