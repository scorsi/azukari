package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
)

// ErrUnknownBucket is returned by the operator calls for a bucket not in the configuration.
var ErrUnknownBucket = errors.New("bucket unknown")

// Schedule runs each bucket's retention daily at its run_at, local time (ADR 0007), and its
// replication to each replica of its class every `every` (ADR 0008), until ctx is done. It
// returns at once; the runs happen in the background.
func (s *Server) Schedule(ctx context.Context) {
	for name, sb := range s.buckets {
		go s.schedule(ctx, name, sb.ret.RunAt)
		for _, r := range sb.replicas {
			go s.scheduleReplica(ctx, name, r)
		}
	}
}

func (s *Server) schedule(ctx context.Context, name, runAt string) {
	at, err := time.Parse("15:04", runAt)
	if err != nil {
		s.log.Error("", "event", "prune-failed", "bucket", name, "err", fmt.Sprintf("retention.run_at: %v", err))
		return
	}
	for {
		now := s.now()
		select {
		case <-ctx.Done():
			return
		case <-s.after(nextRun(now, at.Hour(), at.Minute()).Sub(now)):
		}
		s.RunRetention(name, false) // logged there
	}
}

// nextRun is the first hh:mm strictly after now, in now's location.
func nextRun(now time.Time, hour, minute int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, now.Location())
	}
	return t
}

// RunRetention prunes bucket name now (ADR 0007), one run per bucket at a time: dry when
// dryRun or when its retention is not enabled. It logs prune-completed or prune-failed and,
// for real deletions, updates the prune metrics (ADR 0009). An unavailable bucket is never
// pruned (ErrUnavailable). It waits for the bucket's replication runs to end (ADR 0008).
func (s *Server) RunRetention(name string, dryRun bool) (bucket.Pruned, error) {
	sb, ok := s.buckets[name]
	if !ok {
		return bucket.Pruned{}, fmt.Errorf("%w: %q", ErrUnknownBucket, name)
	}
	sb.run.Lock()
	defer sb.run.Unlock()
	dry := dryRun || !sb.ret.Enabled
	p, err := s.prune(sb, dry)
	if !dry {
		s.m.mu.Lock()
		s.m.prunedBytes[name] += uint64(p.Bytes)
		if err == nil {
			s.m.pruneSuccess[name] = s.now().Unix()
		}
		s.m.mu.Unlock()
	}
	if err != nil {
		if errors.Is(err, bucket.ErrBecameUnavailable) {
			s.log.Error("", "event", "bucket-unavailable", "bucket", name, "err", err.Error())
		}
		s.log.Error("", "event", "prune-failed", "bucket", name, "err", err.Error())
		return p, err
	}
	s.log.Info("", "event", "prune-completed", "bucket", name, "snapshots", p.Snapshots, "refs", p.Refs,
		"chunks", p.Chunks, "bytes", p.Bytes, "dry_run", dry)
	return p, nil
}

func (s *Server) prune(sb *served, dry bool) (bucket.Pruned, error) {
	if sb.b == nil {
		return bucket.Pruned{}, bucket.ErrUnavailable
	}
	keep, err := config.ParseDuration(sb.ret.Keep)
	if err != nil {
		return bucket.Pruned{}, fmt.Errorf("retention.keep: %w", err)
	}
	grace, err := config.ParseDuration(sb.ret.Grace)
	if err != nil {
		return bucket.Pruned{}, fmt.Errorf("retention.grace: %w", err)
	}
	return sb.b.Prune(s.now(), keep, grace, dry)
}

// DeleteSnapshot deletes one snapshot of an export for the operator (ADR 0007, 0030): root, then refs; its
// chunks go at the next run. It waits for the bucket's replication runs to end, as retention
// does (ADR 0008).
func (s *Server) DeleteSnapshot(name, namespace, export, snapshot string) error {
	b, err := s.bucket(name)
	if err != nil {
		return err
	}
	sb := s.buckets[name]
	sb.run.Lock()
	defer sb.run.Unlock()
	err = b.DeleteSnapshot(namespace, export, snapshot)
	if errors.Is(err, bucket.ErrBecameUnavailable) {
		s.log.Error("", "event", "bucket-unavailable", "bucket", name, "err", err.Error())
	}
	return err
}

// Snapshots lists the snapshots of a namespace's export for the operator, oldest first, with
// their receive times and the bytes of their root and refs.
func (s *Server) Snapshots(name, namespace, export string) ([]bucket.Snapshot, error) {
	b, err := s.bucket(name)
	if err != nil {
		return nil, err
	}
	return b.Snapshots(namespace, export)
}

func (s *Server) bucket(name string) (*bucket.Bucket, error) {
	sb, ok := s.buckets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBucket, name)
	}
	if sb.b == nil {
		return nil, bucket.ErrUnavailable
	}
	return sb.b, nil
}
