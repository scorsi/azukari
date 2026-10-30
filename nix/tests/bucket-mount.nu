# Steps of the nixosTest `bucket-mount` (bucket-mount.nix): `bucket-mount.nu <step>`.
use std assert
use client.nu *
use common.nu *

def main [] {}

def wait-logged [event: string, bucket: string] {
  wait-until $"($event) of ($bucket)" { (logged $event $'"bucket":"($bucket)"') > 0 }
}

def wait-available [bucket: string, value: string] {
  let re = '^azukari_bucket_available\{([^}]*,)?bucket="' + $bucket + '"(,[^}]*)?\} ' + $value + '$'
  wait-until $"($bucket) available ($value)" { metrics | any {|l| $l =~ $re } }
}

# No restart: the store starts after srv-disk1.mount (After=), so its first boot creates backup.
def "main ready" [] {
  wait-logged bucket-created backup
}

# archive's disk is not mounted: 503, nothing on the root filesystem.
def "main unmounted" [] {
  assert ((^mountpoint -q /srv/disk2 | complete).exit_code != 0) "/srv/disk2 is mounted"
  wait-logged bucket-unavailable archive
  assert equal (call node-a PUT node-a/chunks/c1 one --bucket archive) "503 bucket-unavailable"
  assert equal (call node-a GET node-a/chunks/c1 --bucket archive) "503 bucket-unavailable"
  assert equal ("/srv/disk2" | path type) "dir"
  assert equal (ls -a /srv/disk2 | length) 0
}

def "main other-serves" [] {
  assert equal (call node-a PUT node-a/chunks/c1 one) "201"
  assert equal (call node-a GET node-a/chunks/c1) "200 one"
  wait-available backup "1"
  wait-available archive "0"
}

# Mounts archive's disk and restarts the store; `mounted` once its port is open again.
def "main mount" [] {
  ^systemctl start srv-disk2.mount
  ^mountpoint -q /srv/disk2
  ^systemctl restart azukari-server
}

def "main mounted" [] {
  wait-logged bucket-created archive
  assert equal (open --raw /srv/disk2/azukari/archive/FORMAT | str trim -r -c "\n") "2"
  assert equal (call node-a PUT node-a/chunks/c1 two --bucket archive) "201"
  assert equal (call node-a GET node-a/chunks/c1 --bucket archive) "200 two"
  assert equal (call node-a GET node-a/chunks/c1) "200 one"
  wait-available archive "1"
  wait-available backup "1"
}
