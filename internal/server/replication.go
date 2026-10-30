package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
)

// Replication (ADR 0008): the store copies each bucket B to every replica of its class with
// rsync, run as a child process. D/ is the replica's destination (a local directory, or
// <user>@<host>:<dir> over SSH) and the copy is D/B/. One run is these calls, in this order:
//
//  0. rsync -r --include=/B/ --include=/B/FORMAT --exclude=* D/ <tmp>/
//     fetches D/B/FORMAT, if any: one other than the bucket's stops the run before copying.
//  1. rsync -r --include=/FORMAT --exclude=* <bucket>/ D/B/
//     FORMAT first (rsync creates D/B/).
//  2. rsync -r --exclude=/tmp/ --exclude=/objects/*/snapshots/** <bucket>/ D/B/
//     every new or recreated object except the snapshot roots; tmp/ never.
//  3. rsync --files-from=- <bucket>/ D/B/, the roots listed before step 1 on stdin.
//     Only roots stored before the run: their refs (ADR 0025) and the chunks those name
//     were there too, so step 2 copied them. A root stored since waits for the next run.
//  4. rsync -r --existing --ignore-existing --delete --include=/objects/ --include=/objects/*/
//     --include=/objects/*/<dir>/*** --exclude=* <bucket>/ D/B/
//     for <dir> = snapshots, then refs, then chunks (retention's order, ADR 0007): copies
//     nothing, deletes in D/B/<dir> what the bucket no longer holds; excluded files are never
//     deleted. Retention cannot run meanwhile (served.run), so the bucket's state is one that
//     retention left.
//
// Every call adds -t and keeps rsync's default quick check (ADR 0026): sizes and modification
// times are compared, never contents. The store writes each object when it receives it, so a
// key that retention removed and a later backup stored again (same size, new header) has a
// new time and is copied again. Every call also adds --stats --no-human-readable (counts
// parsed from its output). rsync
// writes each file under a temporary name and renames it when complete (its default, never
// --inplace): a cut run leaves only complete objects. Kind ssh adds -e with sshCommand.
//
// Durability, so a power cut on the target never keeps a later phase's work without an
// earlier one's: every call adds --fsync (the receiver fsyncs each file it writes). For kind
// dir, after each of steps 1 to 4 the store fsyncs D/ and every directory under D/B/, so the
// names that phase created or removed are durable before the next phase starts. For kind ssh
// the receiver's --fsync is all there is: the remote may be a restricted shell that runs
// nothing but rsync, so directory entries there are as durable as its filesystem makes them.
//
// Stalls: every call adds --timeout=rsyncTimeout (rsync exits when no data moves that long)
// and sshCommand bounds the connection and checks the peer is alive. rsync runs in its own
// process group: on cancellation the whole group (rsync and its ssh) is killed and reaped,
// and the run ends, releasing served.run.

// Results of a run: replication-completed or replication-failed (ADR 0009), a skipped run
// logs replication-skipped; azukari_replication_runs_total's result is the event's suffix.
const (
	replicationCompleted = "completed"
	replicationFailed    = "failed"
	replicationSkipped   = "skipped"
)

type replicaKey struct{ bucket, replica string }

type runKey struct{ bucket, replica, result string }

// copied sums rsync's --stats over a run's calls.
type copied struct{ files, bytes, deletions int64 }

// rsyncTimeout is rsync's --timeout in seconds: a transfer idle that long fails the run.
const rsyncTimeout = 600

var statsRE = regexp.MustCompile(`(?m)^(Number of regular files transferred|Total transferred file size|Number of deleted files): ([0-9]+)`)

// scheduleReplica runs replica r of bucket name at once, then every r.Every after each run
// ends, until ctx is done.
func (s *Server) scheduleReplica(ctx context.Context, name string, r config.Replica) {
	every, err := config.ParseDuration(r.Every)
	if err != nil {
		s.log.Error("", "event", "replication-failed", "bucket", name, "replica", r.Name, "err", fmt.Sprintf("every: %v", err))
		return
	}
	for {
		s.Replicate(ctx, name, r)
		select {
		case <-ctx.Done():
			return
		case <-s.after(every):
		}
	}
}

// Replicate runs one replication of bucket name to r (ADR 0008), unless a guard skips it, and
// logs and counts the outcome. The caller runs one at a time per bucket and replica.
func (s *Server) Replicate(ctx context.Context, name string, r config.Replica) {
	sb := s.buckets[name]
	if reason := s.replicaGuard(name, sb, r); reason != "" {
		s.log.Warn("", "event", "replication-skipped", "bucket", name, "replica", r.Name, "reason", reason)
		s.countReplication(name, r.Name, replicationSkipped)
		return
	}
	defer sb.run.RUnlock()
	start := time.Now()
	var c copied
	code, err := s.replicate(ctx, name, sb, r, &c)
	attrs := []any{"bucket", name, "replica", r.Name, "files", c.files, "bytes", c.bytes,
		"deletions", c.deletions, "dur_us", time.Since(start).Microseconds()}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
		if code != 0 {
			attrs = append(attrs, "exit_code", code)
		}
		s.log.Error("", append([]any{"event", "replication-failed"}, attrs...)...)
		s.countReplication(name, r.Name, replicationFailed)
		return
	}
	s.log.Info("", append([]any{"event", "replication-completed"}, attrs...)...)
	s.countReplication(name, r.Name, replicationCompleted)
}

// replicaGuard returns why a run must not start (ADR 0008), or "" with sb.run read-locked:
// retention takes it for writing, so neither starts while the other runs.
func (s *Server) replicaGuard(name string, sb *served, r config.Replica) string {
	if sb.b == nil || sb.b.Err() != nil {
		return "bucket unavailable"
	}
	if r.Kind == "dir" {
		if ok, err := s.isMount(r.MountPoint); err != nil {
			return fmt.Sprintf("mount_point %s: %v", r.MountPoint, err)
		} else if !ok {
			return fmt.Sprintf("mount_point %s is not a mount point", r.MountPoint)
		}
		if reason := s.replicaOverlap(name, r); reason != "" {
			return reason
		}
	}
	if !sb.run.TryRLock() {
		return "retention running"
	}
	return ""
}

// replicaOverlap is the configuration's check (config.Overlap) on resolved paths: a dir
// replica's destination, or its copy of bucket name, that a symlink leads into a bucket (or
// around one) is never written.
func (s *Server) replicaOverlap(name string, r config.Replica) string {
	for _, d := range []string{r.Destination, filepath.Join(r.Destination, name)} {
		dst, err := filepath.EvalSymlinks(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue // rsync creates D/B as a directory, or fails on a missing D
		} else if err != nil {
			return fmt.Sprintf("destination %s: %v", d, err)
		}
		for b, sb := range s.buckets {
			p, err := filepath.EvalSymlinks(sb.path)
			if err != nil {
				p = sb.path // absent (an unavailable bucket): as configured
			}
			if config.Overlap(dst, p) {
				return fmt.Sprintf("destination %s resolves to %s, overlapping bucket %s's path %s", d, dst, b, p)
			}
		}
	}
	return ""
}

// replicate makes the calls of the comment above; code is rsync's exit code when one failed.
func (s *Server) replicate(ctx context.Context, name string, sb *served, r config.Replica, c *copied) (int, error) {
	src, dst := sb.path+"/", r.Destination+"/"+name+"/"
	tmp, err := os.MkdirTemp("", "azukari-replica-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	if code, err := s.rsync(ctx, r, nil, nil, "check", "-r", "--include=/"+name+"/", "--include=/"+name+"/FORMAT",
		"--exclude=*", r.Destination+"/", tmp+"/"); err != nil {
		return code, err
	}
	want, err := os.ReadFile(filepath.Join(sb.path, "FORMAT"))
	if err != nil {
		return 0, err
	}
	switch got, err := os.ReadFile(filepath.Join(tmp, name, "FORMAT")); {
	case err == nil && !bytes.Equal(got, want):
		return 0, fmt.Errorf("destination FORMAT holds %q, want %q", got, want)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return 0, err
	}

	roots, err := listRoots(sb.path)
	if err != nil {
		return 0, err
	}
	type step struct {
		phase string
		stdin io.Reader
		args  []string
	}
	steps := []step{
		{"format", nil, []string{"-r", "--include=/FORMAT", "--exclude=*"}},
		{"objects", nil, []string{"-r", "--exclude=/tmp/", "--exclude=/objects/*/snapshots/**"}},
		{"roots", strings.NewReader(roots), []string{"--files-from=-"}},
	}
	for _, d := range []string{"snapshots", "refs", "chunks"} {
		steps = append(steps, step{"delete-" + d, nil, []string{"-r", "--existing", "--ignore-existing", "--delete",
			"--include=/objects/", "--include=/objects/*/", "--include=/objects/*/" + d + "/***", "--exclude=*"}})
	}
	for _, st := range steps {
		// A bucket whose disk failed meanwhile may look emptier than it is: never mirror it.
		if err := sb.b.Err(); err != nil {
			return 0, err
		}
		if code, err := s.rsync(ctx, r, st.stdin, c, st.phase, append(st.args, src, dst)...); err != nil {
			return code, err
		}
		if r.Kind == "dir" {
			if err := syncDirs(filepath.Join(r.Destination, name)); err != nil {
				return 0, fmt.Errorf("sync %s: %w", st.phase, err)
			}
		}
	}
	return 0, nil
}

// syncDirs fsyncs dir's parent and every directory under dir.
// shortcut: syncs every directory of the copy, not only those a phase touched; list them
// from rsync's --itemize-changes if a large copy makes this slow.
func syncDirs(dir string) error {
	if err := bucket.SyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return bucket.SyncDir(p)
	})
}

// listRoots lists every snapshot root, one path relative to the bucket per line.
func listRoots(path string) (string, error) {
	nss, err := os.ReadDir(filepath.Join(path, "objects"))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ns := range nss {
		if !ns.IsDir() {
			continue
		}
		dir := filepath.Join(path, "objects", ns.Name(), "snapshots")
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			switch {
			case p == dir && errors.Is(err, fs.ErrNotExist):
				return nil // no snapshot yet
			case err != nil:
				return err
			case d.Type().IsRegular():
				rel, _ := filepath.Rel(path, p)
				b.WriteString(rel + "\n")
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// rsync runs one call; c, when not nil, adds its counts. It returns rsync's exit code with
// the error when rsync itself failed.
func (s *Server) rsync(ctx context.Context, r config.Replica, stdin io.Reader, c *copied, phase string, args ...string) (int, error) {
	if s.onPhase != nil {
		s.onPhase(phase)
	}
	args = append([]string{"-t", "--fsync", "--timeout=" + strconv.Itoa(rsyncTimeout), "--stats", "--no-human-readable"}, args...)
	if r.Kind == "ssh" {
		args = append([]string{"-e", sshCommand(r)}, args...)
	}
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second // a straggler holding rsync's pipes never blocks Wait
	var out bytes.Buffer
	errs := &tail{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, errs
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("rsync %s: %w", phase, ctx.Err())
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return 0, fmt.Errorf("rsync %s: %w", phase, err)
		}
		lines := strings.Split(strings.TrimSpace(string(errs.b)), "\n")
		return ee.ExitCode(), fmt.Errorf("rsync %s: %v: %s", phase, err, lines[len(lines)-1])
	}
	if c != nil {
		for _, m := range statsRE.FindAllStringSubmatch(out.String(), -1) {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			switch m[1] {
			case "Number of regular files transferred":
				c.files += n
			case "Total transferred file size":
				c.bytes += n
			default:
				c.deletions += n
			}
		}
	}
	return 0, nil
}

// sshCommand is rsync's -e for kind ssh: the replica's key only, no agent, no user nor system
// configuration, the target's host key pinned by its known_hosts_file, never asking anything;
// connecting fails after 30 s, a peer silent for 2 minutes ends the connection.
// shortcut: rsync splits -e on spaces and honours quotes, so a key or known_hosts path holding
// a single quote breaks it; refuse such paths in the configuration if one ever needs it.
func sshCommand(r config.Replica) string {
	return strings.Join([]string{"ssh", "-F", "/dev/null", "-p", strconv.Itoa(r.Port), "-i", "'" + r.SSHKeyFile + "'",
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile='" + r.KnownHostsFile + "'",
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=30", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4"}, " ")
}

// tail keeps the last 4 KiB written: rsync may print one line per file it fails on.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = append([]byte(nil), t.b[len(t.b)-4096:]...)
	}
	return len(p), nil
}

func (s *Server) countReplication(name, replica, result string) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.replicationRuns[runKey{name, replica, result}]++
	if result == replicationCompleted {
		s.m.replicationSuccess[replicaKey{name, replica}] = s.now().Unix()
	}
}
