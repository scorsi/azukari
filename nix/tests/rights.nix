# nixosTest `rights` (ADR 0010, milestone M1): the store refuses what a backup node must not do,
# with the reasons of ADR 0003 decoded from Error{reason}. One VM: the steps are rights.nu, its
# client client.nu with the test CA's identities, over loopback.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  nu = t.nu "rights.nu";
in
pkgs.testers.runNixOSTest {
  name = "azukari-rights";

  nodes.machine = t.store;

  testScript = ''
    machine.wait_for_open_port(8443, "127.0.0.1")
    machine.succeed("${nu} ready")

    with subtest("an overwrite with other bytes is 409, the object keeps its bytes"):
        machine.succeed("${nu} overwrite")

    with subtest("another node's namespace is 403 namespace-forbidden"):
        machine.succeed("${nu} namespace")

    with subtest("an ID not in nodes is 403 forbidden"):
        machine.succeed("${nu} unknown-id")

    with subtest("a certificate from another CA fails the handshake"):
        machine.succeed("${nu} other-ca")

    with subtest("no method deletes"):
        machine.succeed("${nu} no-delete")

    with subtest("a second config with other bytes (a wrong key) is 409 (ADR 0006)"):
        machine.succeed("${nu} config")

    with subtest("a held HTTP/2 connection after its preface is closed (ADR 0003)"):
        machine.succeed("${nu} h2")

    ${t.validate "machine"}
  '';
}
