# Steps of the nixosTest `restore-from-dir` (restore-from-dir.nix): `restore-from-dir.nu <step>`,
# `backup` and `stop` on the store, the others on the fresh restorer. The driver brings the store's
# /tmp/replica.tar (the `dir` replica's bucket directory), /tmp/tree.listing and /tmp/snapshot to
# the restorer's /tmp.
use std assert
use common.nu *

const cfg = "/etc/azukari-test/node-a.toml"
const tree = "/srv/tree-node-a"
const copy = "/copy/backup"
const cli_log = "/var/log/azukari-cli.log"

def main [] {}

# Path, type, mode, numeric owner and group, mtime and link target of every entry, then the
# SHA-256 of every file.
def listing [root: string]: nothing -> string {
  cd $root
  let stat = ^find . -printf '%p %y %m %U %G %T@ %l\n' | lines
  let sums = ^find . -type f -exec sha256sum '{}' + | lines
  $stat | sort | append ($sums | sort) | str join "\n"
}

def replicated []: nothing -> int { logged replication-completed '"replica":"local"' }

# `azukari backup <args>` (ADR 0028), as root; its stderr (schema v1 lines) kept for the validator.
def cli [args: list<string>]: nothing -> record {
  let r = ^azukari backup ...$args | complete
  $r.stderr | save -a $cli_log
  print -e $r.stderr
  $r
}

def ok [args: list<string>]: nothing -> record {
  let r = cli $args
  if $r.exit_code != 0 { error make {msg: $"azukari backup ($args | str join ' ') exited ($r.exit_code)"} }
  $r
}

def has-event [r: record, event: string, fields: string = ""] {
  let hits = $r.stderr | lines | where {|l| ($l | str contains $'"event":"($event)"') and ($l | str contains $fields) }
  assert (($hits | length) > 0) $"no ($event) with ($fields)"
}

def snapshot []: nothing -> string { open /tmp/snapshot | str trim }

# store: node-a's tree (a large random file over several chunks, nesting, symlinks, modes, numeric
# owners, mtimes), its listing, one backup, then two more runs of the `dir` replica, the second
# started after the backup.
def "main backup" [] {
  mkdir $"($tree)/a/b"
  cd $tree
  ^head -c 12M /dev/urandom | save big
  "hello\n" | save hello
  "" | save empty
  "note\n" | save a/b/note
  "#!/bin/sh\necho run\n" | save a/run
  ^ln -s a/b/note link
  ^ln -s missing dangling
  ^chmod 0640 big
  ^chmod 4755 a/run
  ^chmod 0751 a
  ^chmod 0700 a/b
  ^chown 4242:4243 big a/b a/b/note
  ^chown -h 4242:4243 link
  ^touch -d '2001-02-03 04:05:06.123456789' big hello empty a/b/note a/run
  ^touch -h -d '2002-03-04 05:06:07.5' link dangling
  ^touch -d '2003-04-05 06:07:08.25' a/b a .
  listing $tree | save -f /tmp/tree.listing
  let n = replicated
  run-external $env.AZUKARI_BACKUP run $cfg
  wait-until "two runs of local" { (replicated) >= $n + 2 }
  let names = ^azukari snapshots backup node-a | lines | where $it != "" | parse "{name} {time} {size}" | get name
  assert equal ($names | length) 1 ($names | str join " ")
  $names.0 | save -f /tmp/snapshot
}

# store: stopped for good, then the replica's bucket directory as it lies on its disk.
def "main stop" [] {
  ^systemctl stop azukari-server.service
  assert equal ((^systemctl is-active azukari-server.service | complete).stdout | str trim) "inactive"
  ^tar --numeric-owner -C /srv/disk2/azukari -cf /tmp/replica.tar backup
}

# restorer: the copy at /copy/backup (FORMAT, objects/), owners, modes and mtimes as on the disk.
def "main extract" [] {
  assert (not ("/etc/azukari" | path exists)) "the restorer has a store or a backup unit"
  mkdir /copy
  ^tar --numeric-owner -C /copy -xpf /tmp/replica.tar
  assert equal (open --raw $"($copy)/FORMAT") "1\n"
  let chunks = ^find $"($copy)/objects/node-a/chunks" -type f | lines | length
  assert ($chunks >= 3) $"($chunks) chunks for a 12 MiB file \(chunks are 4 MiB at most\)"
}

# restorer: `snapshots --from-dir` lists exactly the store's snapshot.
def "main snapshots" [] {
  let r = ok [snapshots --from-dir $copy $cfg]
  let names = $r.stdout | lines | where ($it | str trim) != "" | each { str trim | split row -r '\s+' | first }
  assert equal $names [(snapshot)]
}

# restorer: `restore --verify --from-dir` passes and writes nothing.
def "main verify" [] {
  mkdir /verify
  let r = ok [restore --verify --from-dir $copy $cfg (snapshot) /verify]
  has-event $r restore-completed '"verify":true,"owners":false'
  assert equal (ls -a /verify | length) 0
}

# restorer: `restore --from-dir` as root into an empty directory equals the store's tree.
def "main restore" [] {
  mkdir /restore
  let r = ok [restore --from-dir $copy $cfg (snapshot) /restore]
  has-event $r restore-completed '"verify":false,"owners":true'
  let want = open /tmp/tree.listing
  let got = listing $"/restore($tree)"
  assert ($got == $want) $"listings differ:\n($want)\n---\n($got)"
}

# restorer: one byte flipped in the copy's largest chunk; the restore fails, restore-failed.
def "main tampered" [] {
  ^cp -a /copy /tampered
  let chunk = ^find /tampered/backup/objects/node-a/chunks -type f -printf '%s %p\n' | lines
  | parse '{size} {path}' | update size { into int } | sort-by size | last | get path
  let at = 100
  let byte = ^od -An -tu1 -j $at -N 1 $chunk | str trim | into int
  (($byte + 1) mod 256) | into binary | bytes at 0..0 | ^dd $"of=($chunk)" bs=1 $"seek=($at)" conv=notrunc status=none
  assert not equal (^od -An -tu1 -j $at -N 1 $chunk | str trim | into int) $byte "no byte flipped"
  mkdir /tampered-target
  let r = cli [restore --from-dir /tampered/backup $cfg (snapshot) /tampered-target]
  assert ($r.exit_code != 0) "the restore of a tampered copy succeeded"
  has-event $r restore-failed
}
