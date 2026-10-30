# Steps of the nixosTest `replication` (replication.nix): `replication.nu <step>`, on the store
# but for `listing`, which also runs on the SSH target. node-a and node-b back up
# /srv/tree-<node> with the real azukari-backup as root; replica `offsite` is the SSH target,
# `local` the `dir` target on /srv/disk2, both every 5 s.
use std assert
use common.nu *

const bucket = "/srv/disk1/azukari/backup"
const local = "/srv/disk2/azukari/backup"
# replication.nix's keep and grace, in seconds.
const keep = 60
const replicas = [offsite local]
# Every file backs up its node's tree as the export `tree` (ADR 0030).
const export = "tree"

def main [] {}

def now []: nothing -> int { ^date -u +%s | into int }

def backup [node: string] {
  run-external $env.AZUKARI_BACKUP run $"/etc/azukari-test/($node)-backup.toml" $export
}

# A namespace's snapshot names, oldest first (`azukari snapshots`, as root).
def names [ns: string]: nothing -> list<string> {
  ^azukari snapshots backup $ns $export | lines | where $it != "" | parse "{name} {time} {size}" | get name
}

# A copy of the bucket: "<sha256>  <path>" per file, sorted, tmp/ left out (never copied).
def listing-of [root: string]: nothing -> list<string> {
  ^find $root -type f -printf "%P\n" | lines | where {|p| not ($p | str starts-with "tmp/") } | sort
  | each {|p| $"(open --raw ($root | path join $p) | hash sha256)  ($p)" }
}

def events [event: string, replica: string]: nothing -> list<record> {
  ^journalctl _SYSTEMD_UNIT=azukari-server.service -o cat | lines
  | where {|l| ($l | str contains $'"event":"($event)"') and ($l | str contains $'"replica":"($replica)"') }
  | each {|l| $l | from json }
}

def completed [replica: string]: nothing -> int { events replication-completed $replica | length }

def skipped [replica: string, reason: string]: nothing -> int {
  events replication-skipped $replica | where {|e| $e.reason | str contains $reason } | length
}

# Waits for two more completed runs of each of `of`: the second started after this call, so it
# copied whatever the bucket held then.
def settle [of: list<string> = $replicas] {
  let before = $of | each {|r| {replica: $r, n: (completed $r)} }
  wait-until $"two runs of ($of | str join ', ')" { $before | all {|b| (completed $b.replica) >= $b.n + 2 } }
}

# The value of metric `name` whose labels are exactly `labels`.
def metric [name: string, labels: string]: nothing -> int {
  metrics | where {|l| $l | str starts-with $"($name){($labels)} " } | first | split row " " | last | into int
}

def runs [replica: string, result: string]: nothing -> int {
  metric azukari_replication_runs_total $'bucket="backup",replica="($replica)",result="($result)"'
}

def last-success [replica: string]: nothing -> int {
  metric azukari_replication_last_success_timestamp_seconds $'bucket="backup",replica="($replica)"'
}

# Writes the copy under `root` to `out` (the driver brings the SSH target's to the store).
def "main listing" [root: string, out: string] {
  listing-of $root | str join "\n" | save -f $out
}

# The bucket, the `dir` target and the SSH target's listing in `offsite` are the same files.
def "main same" [offsite: string] {
  let want = listing-of $bucket
  assert (($want | where {|l| $l | str contains "/snapshots/" } | length) > 0) "no snapshot in the bucket"
  assert equal (listing-of $local) $want "local differs from the bucket"
  assert equal (open $offsite | lines) $want "offsite differs from the bucket"
}

def "main unchanged" [before: string, after: string] {
  assert equal (open $after | lines) (open $before | lines) "offsite changed"
}

# node-a holds X (/srv/tree-node-a/x, 8 MiB random: several chunks), which the prune frees.
def "main first" [] {
  mkdir /srv/tree-node-a /srv/tree-node-b
  ^head -c 8M /dev/urandom | save /srv/tree-node-a/x
  "v1\n" | save /srv/tree-node-a/note
  "b1\n" | save /srv/tree-node-b/note
  backup node-a
  now | save -f /tmp/first-at
  backup node-b
  settle
  let snap = names node-a | last
  run-external ...[$env.AZUKARI_BACKUP restore --verify /etc/azukari-test/node-a-backup.toml $export $snap /tmp/verify-first]
  for r in $replicas {
    assert (events replication-completed $r | any {|e| $e.files > 0 and $e.bytes > 0 }) $"($r) copied nothing"
    assert ((runs $r completed) >= 2) $"($r) runs_total completed"
    assert ((last-success $r) > 0) $"($r) last success"
  }
}

# Past keep and grace, X removed and backed up again: azukari retention deletes node-a's first
# snapshot and X's chunks; both targets then mirror the deletions.
def "main prune" [] {
  let at = open /tmp/first-at | into int
  wait-until "keep and grace" { (now) - $at > $keep }
  rm /srv/tree-node-a/x
  "v2\n" | save -f /srv/tree-node-a/note
  backup node-a
  let old = names node-a | first
  let r = ^azukari retention backup | str trim | str replace -r ' bytes=\d+' ''
  let p = $r | parse "snapshots={s} refs={r} chunks={c} dry_run={d}" | first
  assert ($p.s == "1" and ($p.c | into int) >= 1 and $p.d == "false") $r
  assert (not ($old in (names node-a))) $"($old) was not pruned"
  settle
  for r in $replicas {
    assert (events replication-completed $r | any {|e| $e.deletions > 0 }) $"($r) mirrored no deletion"
  }
}

# The `dir` target's disk unmounted: its runs are skipped and nothing lands on the root
# filesystem; node-b backs up meanwhile.
def "main dir-unmounted" [] {
  listing-of $local | str join "\n" | save -f /tmp/local-before
  ^systemctl show -P MainPID azukari-server.service | str trim | save -f /tmp/store-pid
  let n = skipped local "is not a mount point"
  ^umount /srv/disk2
  wait-until "local skipped twice" { (skipped local "is not a mount point") >= $n + 2 }
  assert ((runs local skipped) >= 2) "local runs_total skipped"
  "b2\n" | save -f /srv/tree-node-b/note
  backup node-b
  assert equal (ls -a /srv/disk2 | length) 0 "a skipped run wrote on the root filesystem"
}

# Mounted again under the running store (no restart): every file is still there and the next
# runs copy node-b's new snapshot.
def "main dir-mounted" [] {
  ^systemctl start srv-disk2.mount
  ^mountpoint -q /srv/disk2
  let now = listing-of $local
  let lost = open /tmp/local-before | lines | where {|l| not ($l in $now) }
  assert equal $lost [] "files deleted on local"
  settle
  assert equal (^systemctl show -P MainPID azukari-server.service | str trim) (open /tmp/store-pid) "the store restarted"
}

# The bucket's disk unmounted and the store restarted (as bucket-mount): every run of both
# replicas is skipped, local keeps every file (offsite: `unchanged`).
def "main bucket-down" [] {
  let before = listing-of $local
  let n = $replicas | each {|r| {replica: $r, n: (skipped $r "bucket unavailable")} }
  ^systemctl stop azukari-server.service
  ^umount /srv/disk1
  ^systemctl start azukari-server.service
  wait-until "every replica skipped twice" { $n | all {|b| (skipped $b.replica "bucket unavailable") >= $b.n + 2 } }
  assert equal (listing-of $local) $before "local changed"
  for r in $replicas {
    assert ((runs $r skipped) >= 2) $"($r) runs_total skipped"
    assert equal (last-success $r) 0 $"($r) succeeded with its bucket unavailable"
  }
}
