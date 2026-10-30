# Steps of the nixosTest `retention` (retention.nix), one VM: `retention.nu <step>`. node-a and
# node-b back up /srv/tree-<node> with the real azukari-backup as root; the operator calls run as
# `operator` (azukari-admin). The clock is moved forward twice (`jump`): at the start just past
# the daily 03:30, so no scheduled run lands mid-test, and at the end just before it.
use std assert
use client.nu *
use common.nu *

const cli_log = "/var/log/azukari-cli.log"
const objects = "/srv/disk1/azukari/backup/objects/node-a"
# retention.nix's keep and grace, in seconds.
const keep = 60
const grace = 150
# Every file backs up its node's tree as the export `tree` (ADR 0030).
const export = "tree"
# The interrupted run: its refs without a root naming two chunks, identifiers 32 × "a" and 32 × "b".
const orphan = "20260101T000000Z-0badc0de"

def main [] {}

def now []: nothing -> int { ^date -u +%s | into int }

# Runs `cmd`, its stderr (schema v1 lines) kept for the validator.
def cli [cmd: list<string>]: nothing -> record {
  let r = run-external ...$cmd | complete
  $r.stderr | save -a $cli_log
  print -e $r.stderr
  $r
}

def ok [cmd: list<string>]: nothing -> string {
  let r = cli $cmd
  if $r.exit_code != 0 { error make {msg: $"($cmd | str join ' ') exited ($r.exit_code)"} }
  $r.stdout
}

def backup [node: string, bucket: string = "backup"] {
  ok [$env.AZUKARI_BACKUP run $"/etc/azukari-test/($node)-($bucket).toml" $export] | ignore
}

# The operator command, as a member of azukari-admin.
def azukari [...args: string]: nothing -> string { ok [runuser -u operator -- azukari ...$args] }

# A namespace's snapshots, oldest first: name and receive time (Unix seconds).
def snapshots [ns: string, bucket: string = "backup"]: nothing -> list<record> {
  azukari snapshots $bucket $ns $export | lines | where $it != "" | parse "{name} {time} {size}"
  | each {|s| {name: $s.name, at: (^date -u -d $s.time +%s | into int)} }
}

def names [ns: string, bucket: string = "backup"]: nothing -> list<string> {
  snapshots $ns $bucket | each {|s| $s.name }
}

# `azukari retention`'s line without its bytes.
def retention [bucket: string, --dry-run]: nothing -> string {
  let flags = if $dry_run { [--dry-run] } else { [] }
  azukari retention $bucket ...$flags | str trim | str replace -r ' bytes=\d+' ''
}

def orphan-files []: nothing -> list<string> {
  [$"($objects)/refs/($export)/($orphan)" (chunk-file 61) (chunk-file 62)]
}

def chunk-file [hx: string]: nothing -> string { $"($objects)/chunks/($hx)/(1..32 | each { $hx } | str join)" }

# Moves the clock forward to the next `at` (UTC), then restarts the store: its daily timer
# counts from the clock it starts with.
def jump [at: string] {
  mut t = ^date -u -d $at +%s | into int
  if $t <= (now) { $t += 86400 }
  ^date -u -s $"@($t)" | ignore
  ^systemctl restart azukari-server.service
}

def "main start" [] { jump "03:31:00" }

def "main socket" [] {
  wait-until "admin.sock" { "/run/azukari/admin.sock" | path exists }
  assert equal (^stat -c '%a %G' /run/azukari/admin.sock | str trim) "660 azukari-admin"
  let r = cli [runuser -u stranger -- azukari snapshots backup node-a $export]
  assert ($r.exit_code == 1 and ($r.stderr | str contains "permission denied")) $r.stderr
  assert equal (names node-a) []
  assert equal (logged admin-call '"uid":1601') 0
}

# node-a's tree holds X (/srv/x, 8 MiB random), which the race step backs up again.
def "main first" [] {
  mkdir /srv/tree-node-a /srv/tree-node-b
  ^head -c 8M /dev/urandom | save /srv/x
  cp /srv/x /srv/tree-node-a/a-x
  "v1\n" | save /srv/tree-node-a/note
  "b\n" | save /srv/tree-node-b/note
  backup node-a
  backup node-b
  backup node-a trial
  assert equal ([(names node-a) (names node-b) (names node-a trial)] | each {|l| $l | length }) [1 1 1]
}

def "main aged" [] {
  let last = snapshots node-a trial | last
  wait-until "keep" { (now) - $last.at > $keep }
  rm /srv/tree-node-a/a-x
  "v2\n" | save -f /srv/tree-node-a/note
  backup node-a
  backup node-a
  backup node-a trial
  # An interrupted run: chunks, then refs, no root.
  for hx in [61 62] {
    let id = 1..32 | each { $hx } | str join
    assert equal (call node-a PUT $"node-a/chunks/($hx)/($id)" $"interrupted-($hx)") "201"
  }
  let refs = ("" | fill -c a -w 32) + ("" | fill -c b -w 32)
  assert equal (call node-a PUT $"node-a/refs/($export)/($orphan)" $refs) "201"
  now | save -f /tmp/orphan-at
}

# Counts: node-a's first snapshot (root and refs); no chunk, all younger than grace.
def "main dry-run" [] {
  let before = [(names node-a) (names node-b) (names node-a trial)]
  assert equal (retention backup --dry-run) "snapshots=1 refs=1 chunks=0 dry_run=true"
  assert equal (retention trial) "snapshots=1 refs=1 chunks=0 dry_run=true"
  assert equal [(names node-a) (names node-b) (names node-a trial)] $before
  assert equal (logged prune-completed '"bucket":"backup"' '"snapshots":1' '"dry_run":true') 1
  assert equal (logged prune-completed '"bucket":"trial"' '"snapshots":1' '"dry_run":true') 1
}

def "main prune" [] {
  let a = names node-a
  assert equal (retention backup) "snapshots=1 refs=1 chunks=0 dry_run=false"
  assert equal (names node-a) ($a | skip 1)
  # node-b's only snapshot, older than keep, is its newest.
  let b = snapshots node-b
  assert (($b | length) == 1 and (now) - $b.0.at > $keep) ($b | to nuon)
  # The interrupted run is younger than grace: its refs and chunks stay.
  assert (orphan-files | all {|f| $f | path exists })
}

def "main admin" [] {
  let a = names node-a
  assert equal (azukari delete-snapshot backup node-a $export $a.0 | str trim) $"deleted node-a ($export) ($a.0)"
  assert equal (names node-a) [$a.1]
  let r = cli [runuser -u operator -- azukari delete-snapshot backup node-a $export $a.0]
  assert ($r.exit_code == 1 and ($r.stderr | str contains "snapshot-unknown")) $r.stderr
  let deleted = ['"uid":1600' '"call":"delete-snapshot"' $'"export":"($export)","snapshot":"($a.0)"']
  assert equal (logged admin-call ...($deleted ++ ['"code":200'])) 1
  assert equal (logged admin-call ...($deleted ++ ['"code":404'])) 1
  assert ((logged admin-call '"uid":1600' '"call":"snapshots"' '"namespace":"node-a"') > 0)
  assert ((logged admin-call '"uid":1600' '"call":"retention"' '"dry_run":true') > 0)
}

def missing-calls []: nothing -> int { logged request '"call":"missing"' }

# node-a backs up X again, slowed down (CPUQuota) so it is still uploading after its first
# `missing` said X's chunks are present; frozen, a prune past grace deletes them (unreferenced
# since the first snapshot went) with the interrupted run; thawed, the run's refs come back
# refs-incomplete and it uploads them again (ADR 0007).
def "main race" [] {
  let at = open /tmp/orphan-at | into int
  wait-until "grace" { (now) - $at > $grace }
  cp /srv/x /srv/tree-node-a/a-x
  # Over the 64 MiB a run seals before asking missing, so X's chunks are asked in the first batch.
  ^head -c 60M /dev/urandom | save /srv/tree-node-a/b-filler
  ^head -c 32M /dev/urandom | save /srv/tree-node-a/c-rest
  let asked = missing-calls
  (^systemd-run --unit=race-backup -p CPUQuota=3% -p RemainAfterExit=yes
    $env.AZUKARI_BACKUP run /etc/azukari-test/node-a-backup.toml $export)
  wait-until "the run's first missing" { (missing-calls) > $asked }
  ^systemctl freeze race-backup.service
  let r = retention backup
  let p = $r | parse "snapshots={s} refs={r} chunks={c} dry_run={d}" | first
  assert ($p.s == "0" and $p.r == "1" and ($p.c | into int) >= 4 and $p.d == "false") $r
  assert (orphan-files | all {|f| not ($f | path exists) }) "the interrupted run was kept past grace"
  ^systemctl thaw race-backup.service
  wait-until "race-backup" { (^systemctl show -P SubState race-backup.service | str trim) in [exited failed] }
  assert equal (^systemctl show -P Result race-backup.service | str trim) "success"
  assert ((logged request-refused '"reason":"refs-incomplete"') > 0) "the prune deleted none of the run's chunks"
  ^journalctl --sync
  let name = logged-snapshot
  assert equal (names node-a | last) $name
  ok [$env.AZUKARI_BACKUP restore --verify /etc/azukari-test/node-a-backup.toml $export $name /tmp/verify-race] | ignore
}

def logged-snapshot []: nothing -> string {
  ^journalctl _SYSTEMD_UNIT=race-backup.service -o cat | lines
  | where ($it | str contains '"event":"backup-completed"') | first | from json | get snapshot
}

def scheduled-runs []: nothing -> int { logged prune-completed '"bucket":"backup"' '"dry_run":false' }

# node-a's snapshot before the race's is now past keep and goes; node-b's newest stays.
def "main schedule" [] {
  let a = names node-a
  let b = names node-b
  assert equal ($a | length) 2
  let runs = scheduled-runs
  let calls = logged admin-call
  jump "03:29:00"
  wait-until "the daily run" { (scheduled-runs) > $runs }
  assert equal (logged admin-call) $calls "the run was an operator call"
  assert equal (names node-a) ($a | skip 1)
  assert equal (names node-b) $b
}
