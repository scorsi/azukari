# Steps of the nixosTest `exports` (exports.nix), one VM: `exports.nu <step> [args]`. node-a's
# exports `fast` (timer every two minutes) and `slow` (daily) back up a copy of $FIXTURE/<export>;
# the operator calls run as root. The clock is moved forward once, to noon, so slow's daily timer
# never fires during the test.
use std assert
use common.nu *

const cfg = "/etc/azukari/azukari-backup.toml"
const cli_log = "/var/log/azukari-cli.log"
const timers = [azukari-backup-fast.timer azukari-backup-slow.timer]
const peer = '"peer_id":"spiffe://example.org/node-a/azukari-backup"'
# exports.nix's keep, in seconds.
const keep = 60
# The quiet window, in seconds.
const window = 60

def main [] {}

def now []: nothing -> int { ^date -u +%s | into int }

def unit [export: string]: nothing -> string { $"azukari-backup-($export).service" }

# Runs `cmd`, its stderr (schema v1 lines) kept for the validator; fails when it fails.
def ok [cmd: list<string>]: nothing -> string {
  let r = run-external ...$cmd | complete
  $r.stderr | save -a $cli_log
  print -e $r.stderr
  if $r.exit_code != 0 { error make {msg: $"($cmd | str join ' ') exited ($r.exit_code)"} }
  $r.stdout
}

# The snapshots the export's own unit logged as completed, sorted.
def completed [export: string]: nothing -> list<string> {
  ^journalctl $"_SYSTEMD_UNIT=(unit $export)" -o cat | lines
  | where ($it | str contains '"event":"backup-completed"')
  | each {|l| $l | from json | get snapshot } | sort
}

# The export's snapshots on the store, oldest first: name and receive time (Unix seconds).
def snapshots [export: string]: nothing -> list<record> {
  ok [azukari snapshots backup node-a $export] | lines | where $it != "" | parse "{name} {time} {size}"
  | each {|s| {name: $s.name, at: (^date -u -d $s.time +%s | into int)} }
}

def names [export: string]: nothing -> list<string> { snapshots $export | each {|s| $s.name } }

# When `timer` fires next (Unix seconds); systemd prints a weekday first, left out.
def next-elapse [timer: string]: nothing -> int {
  let at = ^systemctl show -P NextElapseUSecRealtime $timer | str trim | split row " " | skip 1 | str join " "
  ^date -u -d $at +%s | into int
}

# Waits for the export's oneshot to be neither starting nor running.
def idle [export: string] {
  wait-until $"($export) idle" { (^systemctl show -P ActiveState (unit $export) | str trim) in [inactive failed] }
}

# The store's log lines (any event) about a call from node-a.
def from-node []: nothing -> int {
  ^journalctl --sync
  ^journalctl _SYSTEMD_UNIT=azukari-server.service -o cat | lines | where ($it | str contains $peer) | length
}

# Timers stopped, the clock moved forward to noon (no midnight, slow's daily run, within the test),
# the store restarted (its daily retention counts from the clock it starts with), timers started.
def "main start" [] {
  ^systemctl stop ...$timers
  mut t = ^date -u -d "12:00:00" +%s | into int
  if $t <= (now) { $t += 86400 }
  ^date -u -s $"@($t)" | ignore
  ^systemctl restart azukari-server.service
  wait-until "the store" { (^curl -sf -o /dev/null $"http://($env.METRICS)/metrics" | complete).exit_code == 0 }
  ^systemctl start ...$timers
}

# slow's unit started by hand: its daily timer would not fire in the test.
def "main slow" [] {
  ^systemctl start (unit slow)
  assert equal (completed slow | length) 1
  assert equal (names slow | length) 1
}

# fast driven by its own timer (the cadence under test): two more completed runs, slow untouched.
def "main fast" [] {
  let before = completed fast | length
  wait-until "two runs of fast" { (completed fast | length) >= $before + 2 }
  assert equal (completed slow | length) 1 "slow ran again"
}

# Right after fast's run, before either timer fires again: no request from node-a for `window`
# seconds, while both timers are armed.
def "main quiet" [] {
  idle fast
  sleep 2sec
  for timer in $timers {
    assert equal (^systemctl show -P ActiveState $timer | str trim) "active" $"($timer) not armed"
    let left = (next-elapse $timer) - (now)
    assert ($left > $window + 5) $"($timer) fires in ($left) s, inside the window"
  }
  let runs = [fast slow] | each {|e| completed $e | length }
  let n = from-node
  assert ($n > 0) "no request from node-a logged at all"
  sleep ($window * 1sec)
  assert equal (from-node) $n "node-a reached the store while no timer fired"
  assert equal ([fast slow] | each {|e| completed $e | length }) $runs
}

# fast's timer stopped from here on, so the counts below hold: each export lists exactly what
# its own unit completed, on the store and from the node; the gauge has a series per export.
def "main lists" [] {
  ^systemctl stop azukari-backup-fast.timer
  idle fast
  let fast = names fast
  let slow = names slow
  assert (($fast | length) >= 2) ($fast | str join " ")
  assert equal ($slow | length) 1
  assert equal ($fast | sort) (completed fast)
  assert equal ($slow | sort) (completed slow)
  for e in [fast slow] {
    let local = ok [azukari backup snapshots $cfg $e] | lines | where ($it | str trim) != ""
    | each { str trim | split row -r '\s+' | first } | sort
    assert equal $local (names $e | sort) $"azukari backup snapshots ($e)"
  }
  let gauge = {|e|
    metrics | where {|l| $l | str starts-with $'azukari_last_snapshot_timestamp_seconds{bucket="backup",namespace="node-a",export="($e)"} ' }
    | first | split row " " | last | into int
  }
  assert ((do $gauge slow) < (do $gauge fast)) "slow's last snapshot is not older than fast's"
}

# slow's only snapshot is past keep, fast's older ones too: those go, the newest of each stays.
def "main retention" [] {
  let fast = snapshots fast
  let slow = snapshots slow
  assert ((now) - $slow.0.at > $keep) "slow's snapshot is not past keep"
  let r = ok [azukari retention backup] | str trim | str replace -r ' bytes=\d+' ''
  let p = $r | parse "snapshots={s} refs={r} chunks={c} dry_run={d}" | first
  assert ($p.s == (($fast | length) - 1 | into string) and $p.d == "false") $r
  assert equal (names fast) [($fast | last | get name)]
  assert equal (names slow) [$slow.0.name]
}

# The export's newest snapshot restores alone: its own directory only, equal to its source.
def "main restore" [export: string] {
  let target = $"/restore-($export)"
  ok [azukari backup restore $cfg $export (names $export | last) $target] | ignore
  assert equal (ls -a $"($target)/persist/backups" | each {|f| $f.name | path basename }) [$export]
  ^diff -r $"($env.FIXTURE)/($export)" $"($target)/persist/backups/($export)"
}
