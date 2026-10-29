{ lib, buildGoModule }:
buildGoModule (finalAttrs: {
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
      ../internal/config/testdata
      ../internal/bucket/testdata
      ../internal/seal/testdata
      ../internal/fastcdc/testdata
    ];
  };

  # After a go.mod change: set to lib.fakeHash and read the value from the build error.
  vendorHash = "sha256-vGi2sR4rSM7ayErWfPyMUFwdiK3ICl+Dnp/ghvHTy/g=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}" # azukari_build_info
  ];

  # `nix flake check` runs this phase. The default one tests only subPackages (the commands),
  # never internal/ nor gen/: vet and test the whole module instead.
  checkPhase = ''
    runHook preCheck
    go vet ./...
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "Store for the homelab's data, with typed buckets, starting with backups";
    homepage = "https://github.com/scorsi/azukari";
    mainProgram = "azukari";
  };
})
