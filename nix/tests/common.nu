# Shared by the nixosTests' Nushell scripts (ADR 0010), as common.nix by their Nix files.

# How many lines of `unit`'s journal log `event` and hold every one of `needles` (grep -F).
export def logged [
  event: string
  ...needles: string
  --unit: string = "azukari-server.service"
]: nothing -> int {
  let needles = [$'"event":"($event)"' ...$needles]
  ^journalctl $"_SYSTEMD_UNIT=($unit)" -o cat
  | lines
  | where {|l| $needles | all {|n| $l | str contains $n } }
  | length
}

# Retries `check` once a second until it returns true, as the driver's wait_until_succeeds
# (an error is a false); fails after 900 tries.
export def wait-until [what: string, check: closure] {
  for _ in 1..900 {
    if (try { do $check } catch { false }) { return }
    sleep 1sec
  }
  error make {msg: $"timed out waiting for ($what)"}
}

# The store's /metrics lines.
export def metrics []: nothing -> list<string> {
  http get --raw $"http://($env.METRICS)/metrics" | lines
}
