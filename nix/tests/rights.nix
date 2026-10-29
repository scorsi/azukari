# nixosTest `rights` (ADR 0010, milestone M1): the store refuses what a backup node must not do,
# with the reasons of ADR 0003 decoded from Error{reason}. One VM: the client is client.py with
# the test CA's identities, over loopback.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
in
pkgs.testers.runNixOSTest {
  name = "azukari-rights";

  nodes.machine = t.store;

  testScript = ''
    def call(identity, method, key, body=None):
        arg = "" if body is None else f" '{body}'"
        return machine.succeed(f"${t.client} {identity} {method} {key}{arg}").strip()

    journal = "journalctl _SYSTEMD_UNIT=azukari-server.service -o cat"

    # No restart: the store starts after srv-disk1.mount (After=), so its first boot creates the bucket.
    machine.wait_for_open_port(8443, "127.0.0.1")
    machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"bucket-created\"'")

    with subtest("an overwrite with other bytes is 409, the object keeps its bytes"):
        assert call("node-a", "PUT", "node-a/chunks/c1", "one") == "201", "created"
        assert call("node-a", "PUT", "node-a/chunks/c1", "one") == "200", "retry"
        assert call("node-a", "PUT", "node-a/chunks/c1", "two") == "409 object-exists"
        assert call("node-a", "GET", "node-a/chunks/c1") == "200 one"

    with subtest("another node's namespace is 403 namespace-forbidden"):
        assert call("node-b", "PUT", "node-b/chunks/c1", "b") == "201"
        assert call("node-a", "PUT", "node-b/chunks/c2", "x") == "403 namespace-forbidden"
        assert call("node-a", "GET", "node-b/chunks/c1") == "403 namespace-forbidden"
        assert call("node-b", "GET", "node-b/chunks/c2") == "404 object-unknown"

    with subtest("an ID not in nodes is 403 forbidden"):
        assert call("node-c", "PUT", "node-c/chunks/c1", "c") == "403 forbidden"
        assert call("node-c", "GET", "node-a/chunks/c1") == "403 forbidden"

    with subtest("a certificate from another CA fails the handshake"):
        assert call("other-ca/node-a", "GET", "node-a/chunks/c1") == "tls-refused"
        machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"tls-handshake-error\"'")

    with subtest("no method deletes"):
        for method in ["DELETE", "POST", "PATCH"]:
            assert call("node-a", method, "node-a/chunks/c1") == "400 request-invalid", method
        assert call("node-a", "GET", "node-a/chunks/c1") == "200 one"

    with subtest("a second config with other bytes (a wrong key) is 409 (ADR 0006)"):
        good = '{"format":1,"key_check":"' + "a" * 64 + '"}'
        wrong = '{"format":1,"key_check":"' + "b" * 64 + '"}'
        assert call("node-a", "PUT", "node-a/config", good) == "201"
        assert call("node-a", "PUT", "node-a/config", good) == "200"
        assert call("node-a", "PUT", "node-a/config", wrong) == "409 object-exists"
        assert call("node-a", "GET", "node-a/config") == "200 " + good

    with subtest("a held HTTP/2 connection after its preface is closed (ADR 0003)"):
        held = int(machine.succeed("${t.client} node-a h2-preface").strip())
        # Header timeout 10 s; anything well under the client's 60 s proves the store closed it.
        assert held <= 20, held

    ${t.validate "machine"}
  '';
}
