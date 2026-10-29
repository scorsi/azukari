# Shared by the nixosTests (ADR 0010): the test CA, the store VM and the closing validators.
{
  self,
  pkgs,
  nisshi,
  masu,
}:
let
  system = pkgs.stdenv.hostPlatform.system;
  metrics = "127.0.0.1:9464";
  id = host: service: "spiffe://example.org/${host}/${service}";

  # The internal CA's profile (hanko ADR-0004): ECDSA P-256, one URI SAN and no DNS SAN,
  # digitalSignature, both EKUs, not a CA. Built once at build time; ca.key is kept for a test
  # that reissues. `<dir>/{svid,key}.pem` per identity, `bundle.pem` the store's trust bundle;
  # `other-ca/node-a` holds node-a's ID from an unrelated CA.
  ca = pkgs.runCommand "azukari-test-ca" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out/other-ca
    cat > ca.cnf <<EOF
    [req]
    distinguished_name = dn
    x509_extensions = v3
    [dn]
    [v3]
    basicConstraints = critical,CA:TRUE
    keyUsage = critical,keyCertSign
    subjectKeyIdentifier = hash
    EOF
    mkca() {
      openssl ecparam -name prime256v1 -genkey -noout -out $1.key
      openssl req -x509 -new -config ca.cnf -key $1.key -subj /CN=azukari-test-$2 -days 3650 -out $1.pem
    }
    issue() { # <ca> <dir> <spiffe id>
      mkdir -p $2
      openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out $2/key.pem
      openssl req -new -key $2/key.pem -subj /O=azukari-test -out leaf.csr
      printf '%s\n' basicConstraints=critical,CA:FALSE keyUsage=critical,digitalSignature \
        extendedKeyUsage=serverAuth,clientAuth "subjectAltName=URI:$3" > leaf.ext
      openssl x509 -req -in leaf.csr -CA $1.pem -CAkey $1.key -CAcreateserial -days 3650 \
        -extfile leaf.ext -out $2/svid.pem
    }
    mkca $out/ca ca
    mkca other other
    cp $out/ca.pem $out/bundle.pem
    issue $out/ca $out/store ${id "store" "azukari-server"}
    for n in node-a node-b node-c; do issue $out/ca $out/$n ${id "$n" "azukari-backup"}; done
    issue other $out/other-ca/node-a ${id "node-a" "azukari-backup"}
    openssl x509 -noout -ext subjectAltName -in $out/node-a/svid.pem | grep -q URI:spiffe
  '';
in
{
  inherit ca metrics id;

  # `${client} <identity> <method> <key> [body]`, see client.py.
  client = "${pkgs.python3.interpreter} ${./client.py} ${ca}";

  # The store: the module with one backup bucket granted to node-a and node-b, on its own extra
  # virtual disk mounted at its mount_point, `nofail` (ADR 0004, 0010). The SVID files are read
  # from the store's test CA path directly.
  store = {
    imports = [ self.nixosModules.azukari-server ];
    system.stateVersion = "26.05";
    virtualisation.emptyDiskImages = [
      {
        size = 256;
        driveConfig.deviceExtraOpts.serial = "disk1";
      }
    ];
    virtualisation.fileSystems."/srv/disk1" = {
      device = "/dev/disk/by-id/virtio-disk1";
      fsType = "ext4";
      autoFormat = true;
      options = [ "nofail" ];
    };
    services.azukari-server = {
      enable = true;
      settings = {
        listen = [ "127.0.0.1:8443" ];
        listen_metrics = metrics;
        tls = {
          trust_domain = "example.org";
          svid_file = "${ca}/store/svid.pem";
          key_file = "${ca}/store/key.pem";
          bundle_file = "${ca}/bundle.pem";
        };
        classes.standard.replicas = [ ];
        buckets = [
          {
            name = "backup";
            type = "backup";
            class = "standard";
            path = "/srv/disk1/azukari/backup";
            mount_point = "/srv/disk1";
            max_object_bytes = 1048576;
            disk_reserve = "1MiB";
            nodes = [
              (id "node-a" "azukari-backup")
              (id "node-b" "azukari-backup")
            ];
            retention = {
              keep = "30d";
              run_at = "03:30";
            };
          }
        ];
      };
    };
  };

  # Python closing every test on machine `m`: the store's own journal lines (not PID 1's) pass
  # the log schema, its /metrics the metric convention (ADR 0009).
  validate = m: ''
    ${m}.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=azukari-server.service -o cat"
                 " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    ${m}.succeed("${masu.lib.validateMetrics.${system}} validate --metrics http://${metrics}/metrics")
  '';
}
