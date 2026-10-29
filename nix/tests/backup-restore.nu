# Steps of the nixosTest `backup-restore` (backup-restore.nix): `backup-restore.nu <step> [args]`,
# each on the VM its name says. Values one VM hands to another are files under /tmp that the
# driver copies: `<tag>.tar` (node-a's exported tree), `<tag>.listing` (its stat listing) and
# `<tag>.snapshot` (the snapshot that backup made).
use std assert
use common.nu *

const cfg = "/etc/azukari/azukari-backup.toml"
const cli_log = "/var/log/azukari-cli.log"
const data = "/persist/backups/data"
const chunks_dir = "/srv/disk1/azukari/backup/objects/node-a/chunks"

def main [] {}

# azukari-backup, as root or (--user) as azukari-backup; its stderr (schema v1 lines) is kept in
# /tmp/err and for the validator.
def cli [args: list<string>, --user]: nothing -> string {
  let run = if $user { [runuser -u azukari-backup -- $env.AZUKARI_BACKUP] } else { [$env.AZUKARI_BACKUP] }
  let r = run-external ...$run ...$args | complete
  $r.stderr | save -f /tmp/err
  $r.stderr | save -a $cli_log
  print -e $r.stderr
  if $r.exit_code != 0 { error make {msg: $"azukari-backup ($args | str join ' ') exited ($r.exit_code)"} }
  $r.stdout
}

def snapshots []: nothing -> list<string> {
  cli --user [snapshots $cfg] | lines | where ($it | str trim) != "" | each { str trim | split row -r '\s+' | first } | sort
}

# Path, type, mode, numeric owner and group, mtime, link target of every entry.
def listing [root: string]: nothing -> string {
  cd $root
  ^find . -printf '%p %y %m %U %G %T@ %l\n' | lines | sort | str join "\n"
}

# Oneshot: start returns once the export and the run ended, and fails if either failed.
def backup [] { ^systemctl start azukari-backup.service }

def unit-events [event: string]: nothing -> int { logged $event --unit azukari-backup.service }

# The last /tmp/err holds a restore-completed line with `fields`.
def restore-completed [fields: string] {
  let hits = open /tmp/err | lines | where {|l| ($l | str contains '"event":"restore-completed"') and ($l | str contains $fields) }
  assert (($hits | length) > 0) $"no restore-completed with ($fields)"
}

def stat [format: string, path: string]: nothing -> string { ^stat -c $format $path | str trim }

# node-a's exported tree now: its tar and stat listing, for the restorer.
def capture [tag: string] {
  ^tar -C /persist/backups -cf $"/tmp/($tag).tar" data
  listing $data | save -f $"/tmp/($tag).listing"
}

# node-a: the run is azukari-backup with no capability.
def "main unit" [] {
  assert equal (^systemctl show -P User azukari-backup.service | str trim) "azukari-backup"
  assert equal (^systemctl show -P CapabilityBoundingSet azukari-backup.service | str trim) ""
}

# node-a: empty files, large random, text, nesting, links, modes, owners, mtimes.
def "main tree" [] {
  # Group azukari-backup or world-readable everywhere, as an export writes it; the root
  # directory as the module makes the export's (2750 root:azukari-backup).
  mkdir /srv/fixture/a/b/c /srv/fixture/empty-dir
  cd /srv/fixture
  "" | save empty
  "" | save a/b/c/empty
  ^head -c 24M /dev/urandom | save big
  ^yes 'azukari compressible text line' | ^head -c 3M | save text
  "hello\n" | save a/b/hello
  "#!/bin/sh\necho run\n" | save a/run
  ^ln -s a/b/hello rel-link
  ^ln -s /persist/backups/data/text abs-link
  ^ln -s missing dangling
  ^ln -s a dir-link
  ^chgrp -hR azukari-backup .
  ^chmod 0640 big empty
  ^chmod 4755 a/run
  ^chmod 0751 a
  ^chmod 1777 empty-dir
  ^chmod 2750 .
  ^chown -R alice:alice a/b
  ^chown 4242:4243 text
  ^chown -h 4242:4243 rel-link
  ^touch -d '2001-02-03 04:05:06.123456789' big text empty a/b/hello
  ^touch -h -d '2002-03-04 05:06:07.5' rel-link abs-link dangling dir-link
  ^touch -d '2003-04-05 06:07:08.25' a/b/c a/b a empty-dir .
  # The service's own data, outside the export and root-only: the export links from it.
  ^mkdir -m 0700 /persist/data
  ^head -c 1M /dev/urandom | save /persist/data/day-1
  ^chmod 0600 /persist/data/day-1
}

# node-a: the first backup, its export and snapshot.
def "main first-backup" [] {
  backup
  capture one
  let orig = open /tmp/one.listing
  assert (($orig | str contains " 4755 ") and ($orig | str contains " 1777 ")) $orig
  # The hard link from /persist/data, group azukari-export-data and no world read, was
  # backed up and restored.
  assert equal (stat '%G %a %h %i' $"($data)/day-1") (stat '%G %a %h %i' /persist/data/day-1)
  assert equal (stat '%G %a %h' $"($data)/day-1") "azukari-export-data 640 2"
  assert ($orig | str contains "./day-1 f 640 ") $orig
  let first = snapshots
  assert equal ($first | length) 1 ($first | str join " ")
  $first.0 | save -f /tmp/one.snapshot
  # The non-root export wrote through the base.
  assert equal (stat '%U %G %a' /persist/backups/alice/hello) "alice azukari-export-alice 640"
}

# store: the chunks a backup left, kept in /tmp/chunks for `chunks-added`.
def chunks []: nothing -> int { ^find $chunks_dir -type f | lines | length }

def "main chunks-first" [] {
  let before = chunks
  assert ($before >= 6) $"($before) chunks for a 27 MiB tree \(chunks are 4 MiB at most\)"
  $before | into string | save -f /tmp/chunks
}

def "main chunks-added" [] {
  let added = (chunks) - (open /tmp/chunks | into int)
  assert ($added >= 1 and $added <= 4) $"($added) chunks added for one changed file"
}

# restorer: extracts node-a's tree `tag` for diff -r.
def "main extract" [tag: string] {
  mkdir $"/orig-($tag)"
  ^tar -C $"/orig-($tag)" -xf $"/tmp/($tag).tar"
}

# restorer: restores `orig`'s snapshot as root into an empty /restore-<tag>, compares it with
# node-a's tree `orig`, then --verify as azukari-backup.
def "main restore" [tag: string, orig: string] {
  let snapshot = open $"/tmp/($orig).snapshot"
  let target = $"/restore-($tag)"
  ^mkdir $target $"/verify-($tag)"
  cli [restore $cfg $snapshot $target]
  restore-completed '"verify":false,"owners":true'
  ^diff -r --no-dereference $"/orig-($orig)/data" $"($target)($data)"
  let want = open $"/tmp/($orig).listing"
  let got = listing $"($target)($data)"
  assert ($got == $want) $"stat listings differ:\n($want)\n---\n($got)"
  cli --user [restore --verify $cfg $snapshot $"/verify-($tag)"]
  restore-completed '"verify":true,"owners":false'
  assert equal (ls -a $"/verify-($tag)" | length) 0
}

# restorer: the non-root export's file came back.
def "main alice" [] {
  assert equal (open --raw /restore-one/persist/backups/alice/hello | str trim -r -c "\n") "hello-from-alice"
}

# node-a: one file changes, the second backup.
def "main second-backup" [] {
  "changed" | ^dd of=/srv/fixture/big bs=1 seek=12582912 conv=notrunc
  ^touch -d '2004-05-06 07:08:09' /srv/fixture/big
  backup
  capture two
  let first = open /tmp/one.snapshot
  let both = snapshots
  assert (($both | length) == 2 and $first in $both) ($both | str join " ")
  $both | where $it != $first | first | save -f /tmp/two.snapshot
  $both | to json | save -f /tmp/both.json
  assert equal (unit-events backup-completed) 2
}

# node-a: an unreadable file fails the run.
def "main unreadable" [] {
  # Group azukari-backup from the setgid directory, but no group read: no capability reads it.
  "secret\n" | save /srv/fixture/secret
  ^chmod 0600 /srv/fixture/secret
  assert ((^systemctl start azukari-backup.service | complete).exit_code != 0) "the run succeeded"
  assert ((logged backup-failed $"($data)/secret" --unit azukari-backup.service) > 0)
  assert equal (snapshots) (open /tmp/both.json) "a failed run published a snapshot"
  ^chmod 0640 /srv/fixture/secret
  backup
  assert equal (snapshots | length) 3
  assert equal (unit-events backup-completed) 3
}

# node-a: no syscall the run made was denied. SystemCallFilter= kills with SIGSYS (status 31),
# logged by systemd and the kernel's audit.
def "main syscalls" [] {
  let unit = ^journalctl -u azukari-backup.service | lines | where $it =~ 'status=31/SYS|SIGSYS'
  assert equal $unit []
  let audit = ^journalctl -k | lines | where $it =~ 'type=1326.*azukari'
  assert equal $audit []
}
