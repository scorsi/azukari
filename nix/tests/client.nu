# Test client of the nixosTests (ADR 0010): one call to the store as one of the test CA's identities.
#
# <identity> is a directory of $env.CA holding svid.pem and key.pem; the store is verified against
# $env.CA/bundle.pem (its SVID has no DNS SAN: no hostname check, as azukari-backup). curl, as
# Nushell's `http` presents no client certificate; openssl for the raw HTTP/2 connection.

const store = "127.0.0.1:8443"

# One HTTP/1.1 call to `key` of `bucket`: "<status> <reason>" for an error (Error{reason}
# decoded), "<status> <body>" otherwise, "tls-refused" when the store refuses the handshake.
export def call [
  identity: string
  method: string
  key: string
  body?: string
  --bucket: string = "backup"
]: nothing -> string {
  let id = $"($env.CA)/($identity)"
  let out = mktemp -t
  # curl cannot verify a chain without a hostname: -k, then the served chain is verified below.
  let args = [
    -s --http1.1 -k -X $method --cert $"($id)/svid.pem" --key $"($id)/key.pem"
    -o $out -w "%{http_code} %{content_type}\n%{certs}"
  ]
  let args = if $body == null { $args } else {
    $args ++ [-H $"Azukari-Sha256: ($body | hash sha256)" --data-binary @-]
  }
  let r = $body | default "" | ^curl ...$args $"https://($store)/v1/b/($bucket)/o/($key)" | complete
  # 35 handshake, 52 empty reply, 56 receive failure: the store refused the handshake.
  if $r.exit_code in [35 52 56] { return "tls-refused" }
  if $r.exit_code != 0 { error make {msg: $"curl exited ($r.exit_code): ($r.stderr)"} }
  let certs = mktemp -t
  $r.stdout | save -f $certs
  ^openssl verify -CAfile $"($env.CA)/bundle.pem" -purpose sslserver $certs | ignore
  let head = $r.stdout | lines | first | split row " "
  let status = $head.0 | into int
  let data = open --raw $out | into binary
  rm $out $certs
  if $status >= 400 {
    if $head.1? != "application/x-protobuf" { error make {msg: $"content type: ($r.stdout | lines | first)"} }
    $"($status) (reason $data)"
  } else {
    $"($status) ($data | decode utf-8)" | str trim
  }
}

# Decodes Error{reason}: exactly one field 1 (string), length under 128.
def reason [body: binary]: nothing -> string {
  let n = $body | bytes length
  if $n < 2 or ($body | bytes at 0..0) != 0x[0a] or ($body | bytes at 1..1 | into int) != $n - 2 {
    error make {msg: $"not an Error: ($body)"}
  }
  $body | bytes at 2.. | decode utf-8
}

# Holds an HTTP/2 connection after its preface: the seconds until the store closes it.
export def h2-preface [identity: string]: nothing -> int {
  let id = $"($env.CA)/($identity)"
  # The client preface and an empty SETTINGS frame, then nothing: no HEADERS ever comes.
  let preface = ("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" | into binary) ++ 0x[00 00 00 04 00 00 00 00 00]
  let start = date now
  let r = (
    $preface | ^timeout 60 openssl s_client -connect $store -alpn h2 -ign_eof -nocommands
      -CAfile $"($env.CA)/bundle.pem" -verify_return_error
      -cert $"($id)/svid.pem" -key $"($id)/key.pem"
    | complete
  )
  let held = (date now) - $start
  if $r.exit_code == 124 { error make {msg: "the store kept the connection 60 s"} }
  if ($r.stdout | into binary | bytes index-of ("ALPN protocol: h2" | into binary)) < 0 {
    error make {msg: $"no h2 connection: ($r.stderr)"}
  }
  $held / 1sec | math round
}
