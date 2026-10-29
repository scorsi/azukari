# nixosTest `svid-reload` (ADR 0010, milestone M1): the test CA reissues the store's SVID in the
# VM, SIGHUP (the unit's reload), and new handshakes see the new certificate; a reload with a key
# that does not match keeps the previous one (ADR 0005). One VM, as `rights`.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  dir = "/var/lib/azukari-svid";
  openssl = "${pkgs.openssl}/bin/openssl";
in
pkgs.testers.runNixOSTest {
  name = "azukari-svid-reload";

  nodes.machine = {
    imports = [ t.store ];
    # A writable copy of the store's SVID (common.nix reads it from the CA's store path).
    systemd.tmpfiles.rules = [
      "d ${dir} 0755 root root -"
      "C ${dir}/svid.pem - - - - ${t.ca}/store/svid.pem"
      "C ${dir}/key.pem - - - - ${t.ca}/store/key.pem"
    ];
    services.azukari-server.settings.tls = {
      svid_file = pkgs.lib.mkForce "${dir}/svid.pem";
      key_file = pkgs.lib.mkForce "${dir}/key.pem";
    };
  };

  testScript = ''
    import datetime

    journal = "journalctl _SYSTEMD_UNIT=azukari-server.service -o cat"

    def served():
        """SHA-256 fingerprint of the certificate a new handshake gets from the store."""
        return machine.succeed(
            "${openssl} s_client -connect 127.0.0.1:8443 -tls1_3"
            " -cert ${t.ca}/node-a/svid.pem -key ${t.ca}/node-a/key.pem </dev/null 2>/dev/null"
            " | ${openssl} x509 -noout -fingerprint -sha256"
        ).strip()

    def fingerprint(path):
        return machine.succeed(f"${openssl} x509 -noout -fingerprint -sha256 -in {path}").strip()

    def not_after(path):
        end = machine.succeed(f"${openssl} x509 -noout -enddate -in {path}").strip().split("=", 1)[1]
        return datetime.datetime.strptime(end, "%b %d %H:%M:%S %Y %Z").replace(
            tzinfo=datetime.timezone.utc).timestamp()

    def expiry():
        out = machine.succeed("${pkgs.curl}/bin/curl -fsS http://${t.metrics}/metrics")
        lines = [l for l in out.splitlines() if l.startswith("azukari_svid_expiry_timestamp_seconds ")]
        assert len(lines) == 1, out
        return float(lines[0].split()[1])

    def events(name):
        return int(machine.succeed(f"{journal} | grep -cF '\"event\":\"{name}\"' || true").strip())

    machine.wait_for_unit("srv-disk1.mount")
    machine.succeed("systemctl restart azukari-server")
    machine.wait_for_open_port(8443, "127.0.0.1")

    old = fingerprint("${dir}/svid.pem")
    assert served() == old, "the store serves the SVID it started with"
    assert expiry() == not_after("${dir}/svid.pem")

    with subtest("the test CA reissues the SVID, SIGHUP, a new handshake sees it"):
        # The internal CA's profile (common.nix), a new key, another serial and NotAfter.
        machine.succeed(
            "cd $(mktemp -d)"
            " && ${openssl} genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out key.pem"
            " && ${openssl} req -new -key key.pem -subj /O=azukari-test -out leaf.csr"
            " && printf '%s\\n' basicConstraints=critical,CA:FALSE keyUsage=critical,digitalSignature"
            " extendedKeyUsage=serverAuth,clientAuth"
            " 'subjectAltName=URI:${t.id "store" "azukari-server"}' > leaf.ext"
            " && ${openssl} x509 -req -in leaf.csr -CA ${t.ca}/ca.pem -CAkey ${t.ca}/ca.key"
            " -set_serial 4242 -days 30 -extfile leaf.ext -out svid.pem"
            " && install -m 0644 svid.pem key.pem ${dir}/"
        )
        new = fingerprint("${dir}/svid.pem")
        assert new != old
        assert "serial=1092" in machine.succeed("${openssl} x509 -noout -serial -in ${dir}/svid.pem")
        before = expiry()
        machine.succeed("systemctl reload azukari-server")
        machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"svid-reloaded\"'")
        assert served() == new, "new handshakes get the reissued SVID"
        assert expiry() == not_after("${dir}/svid.pem") != before, "the expiry gauge follows"

    with subtest("a key that does not match: svid-reload-failed, the previous SVID still serves"):
        machine.succeed(
            "${openssl} genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out /tmp/other.pem"
            " && install -m 0644 /tmp/other.pem ${dir}/key.pem"
        )
        assert events("svid-reload-failed") == 0
        machine.succeed("systemctl reload azukari-server")
        machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"svid-reload-failed\"'")
        assert events("svid-reloaded") == 1
        assert served() == new, "handshakes keep the previous SVID"
        assert expiry() == not_after("${dir}/svid.pem")

    ${t.validate "machine"}
  '';
}
