# Steps of the nixosTest `rights` (rights.nix), one per subtest: `rights.nu <step>`.
use std assert
use client.nu *
use common.nu *

def main [] {}

# No restart: the store starts after srv-disk1.mount (After=), so its first boot creates the bucket.
def "main ready" [] {
  wait-until "bucket-created" { (logged bucket-created) > 0 }
}

def "main overwrite" [] {
  assert equal (call node-a PUT node-a/chunks/c1 one) "201" "created"
  assert equal (call node-a PUT node-a/chunks/c1 one) "200" "retry"
  assert equal (call node-a PUT node-a/chunks/c1 two) "409 object-exists"
  assert equal (call node-a GET node-a/chunks/c1) "200 one"
}

def "main namespace" [] {
  assert equal (call node-b PUT node-b/chunks/c1 b) "201"
  assert equal (call node-a PUT node-b/chunks/c2 x) "403 namespace-forbidden"
  assert equal (call node-a GET node-b/chunks/c1) "403 namespace-forbidden"
  assert equal (call node-b GET node-b/chunks/c2) "404 object-unknown"
}

def "main unknown-id" [] {
  assert equal (call node-c PUT node-c/chunks/c1 c) "403 forbidden"
  assert equal (call node-c GET node-a/chunks/c1) "403 forbidden"
}

def "main other-ca" [] {
  assert equal (call other-ca/node-a GET node-a/chunks/c1) "tls-refused"
  wait-until "tls-handshake-error" { (logged tls-handshake-error) > 0 }
}

def "main no-delete" [] {
  for method in [DELETE POST PATCH] {
    assert equal (call node-a $method node-a/chunks/c1) "400 request-invalid" $method
  }
  assert equal (call node-a GET node-a/chunks/c1) "200 one"
}

# A second config with other bytes (a wrong key) is 409 (ADR 0006).
def "main config" [] {
  let good = $'{"format":1,"key_check":"("" | fill -c a -w 64)"}'
  let wrong = $'{"format":1,"key_check":"("" | fill -c b -w 64)"}'
  assert equal (call node-a PUT node-a/config $good) "201"
  assert equal (call node-a PUT node-a/config $good) "200"
  assert equal (call node-a PUT node-a/config $wrong) "409 object-exists"
  assert equal (call node-a GET node-a/config) $"200 ($good)"
}

def "main h2" [] {
  let held = h2-preface node-a
  # Header timeout 10 s; anything well under the client's 60 s proves the store closed it.
  assert ($held <= 20) $"held ($held) s"
}
