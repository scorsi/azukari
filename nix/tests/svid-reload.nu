# Steps of the nixosTest `svid-reload` (svid-reload.nix): `svid-reload.nu <step>`.
use std assert
use common.nu *

# The store's writable SVID copy (svid-reload.nix's `dir`).
const dir = "/var/lib/azukari-svid"
const store_id = "spiffe://example.org/store/azukari-server"

def main [] {}

# SHA-256 fingerprint of the certificate a new handshake gets from the store.
def served []: nothing -> string {
  (
    "" | ^openssl s_client -connect 127.0.0.1:8443 -tls1_3
      -cert $"($env.CA)/node-a/svid.pem" -key $"($env.CA)/node-a/key.pem" err> /dev/null
    | ^openssl x509 -noout -fingerprint -sha256
    | str trim
  )
}

def fingerprint [path: string]: nothing -> string {
  ^openssl x509 -noout -fingerprint -sha256 -in $path | str trim
}

# The certificate's notAfter, in seconds since the epoch.
def not-after [path: string]: nothing -> float {
  let end = ^openssl x509 -noout -dateopt iso_8601 -enddate -in $path | str trim | split row "=" | get 1
  ($end | into datetime | into int) / 1_000_000_000 | into float
}

def expiry []: nothing -> float {
  let lines = metrics | where ($it | str starts-with "azukari_svid_expiry_timestamp_seconds ")
  assert equal ($lines | length) 1 ($lines | str join "\n")
  $lines.0 | split row " " | get 1 | into float
}

def events [name: string]: nothing -> int { logged $name }

# Issues a new SVID (the internal CA's profile, common.nix) into the current directory.
def reissue [serial: int] {
  ^openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out key.pem
  ^openssl req -new -key key.pem -subj /O=azukari-test -out leaf.csr
  let ext = [
    "basicConstraints=critical,CA:FALSE"
    "keyUsage=critical,digitalSignature"
    "extendedKeyUsage=serverAuth,clientAuth"
    $"subjectAltName=URI:($store_id)"
  ]
  $ext | each { $in + "\n" } | str join | save -f leaf.ext
  (
    ^openssl x509 -req -in leaf.csr -CA $"($env.CA)/ca.pem" -CAkey $"($env.CA)/ca.key"
      -set_serial $serial -days 30 -extfile leaf.ext -out svid.pem
  )
}

# Writes as the internal CA does: staged next to the file, then renamed over it.
def replace [name: string, src: string] {
  ^install -m 0644 $src $"($dir)/.($name).tmp"
  ^mv -f $"($dir)/.($name).tmp" $"($dir)/($name)"
}

# Reissues in a fresh directory and replaces svid.pem, then key.pem.
def renew [serial: int] {
  cd (mktemp -d)
  reissue $serial
  replace svid.pem svid.pem
  replace key.pem key.pem
}

def "main start" [] {
  assert equal (served) (fingerprint $"($dir)/svid.pem") "the store serves the SVID it started with"
  assert equal (expiry) (not-after $"($dir)/svid.pem")
}

# Renewed files, no reload: the next handshake sees the new SVID.
def "main renew" [] {
  let old = fingerprint $"($dir)/svid.pem"
  renew 4242
  let new = fingerprint $"($dir)/svid.pem"
  assert not equal $new $old
  assert ((^openssl x509 -noout -serial -in $"($dir)/svid.pem") | str contains "serial=1092")
  let before = expiry
  assert equal (served) $new "the next handshake gets the renewed SVID"
  wait-until "svid-reloaded" { (events svid-reloaded) > 0 }
  let after = not-after $"($dir)/svid.pem"
  assert equal (expiry) $after "the expiry gauge follows"
  assert not equal $after $before "the expiry gauge follows"
  served | ignore
  assert equal (events svid-reloaded) 1 "unchanged files are not reloaded again"
}

# A key that does not match: svid-reload-failed once, the previous SVID still serves.
def "main mismatch" [] {
  let new = fingerprint $"($dir)/svid.pem"
  ^openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out /tmp/other.pem
  replace key.pem /tmp/other.pem
  assert equal (events svid-reload-failed) 0
  assert equal (served) $new "handshakes keep the previous SVID"
  wait-until "svid-reload-failed" { (events svid-reload-failed) > 0 }
  assert equal (served) $new
  assert equal (events svid-reload-failed) 1 "one failure per change of the files"
  assert equal (events svid-reloaded) 1
  assert equal (expiry) (not-after $"($dir)/svid.pem")
}

# SIGHUP still loads the files, before any handshake.
def "main sighup" [] {
  renew 4343
  let third = fingerprint $"($dir)/svid.pem"
  ^systemctl reload azukari-server
  wait-until "a second svid-reloaded" { (events svid-reloaded) == 2 }
  assert equal (expiry) (not-after $"($dir)/svid.pem")
  assert equal (served) $third
}
