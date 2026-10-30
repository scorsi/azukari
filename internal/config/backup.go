package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
)

// Backup is azukari-backup's file (ADR 0011); unknown keys are an error. The node's namespace
// is not configured: it is the <host> segment of the node's own SVID (ADR 0003, 0005).
type Backup struct {
	// Server is the store, https://<host>:<port> with no path.
	Server string `toml:"server"`
	Bucket string `toml:"bucket"`
	// DataKeyFile is the node's data key (ADR 0006), not the TLS key.
	DataKeyFile string `toml:"data_key_file"`
	// Exports are the node's units of backup, each its own snapshots (ADR 0030), by name.
	Exports map[string]Export `toml:"exports"`
	TLS     BackupTLS         `toml:"tls"`
}

// Export is one [exports.<name>] table (ADR 0030).
type Export struct {
	// Paths are the trees to back up: absolute, none inside another, across every export
	// of the file (ADR 0006, 0030).
	Paths []string `toml:"paths"`
}

// BackupTLS is the backup side's [tls]: the shared files plus the store's expected ID (ADR 0005).
type BackupTLS struct {
	TLS
	ServerID string `toml:"server_id"`
}

// LoadBackup reads and validates a backup file; the error lists every problem, one per line.
func LoadBackup(path string) (*Backup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBackup(data)
}

// LoadBackupOffline reads a backup file for a restore from a bucket copy (ADR 0006): the
// store is never reached, so server and [tls] are neither required nor checked (on a fresh
// host the table may be absent or name missing files). Unknown keys are still errors.
func LoadBackupOffline(path string) (*Backup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Backup{}
	if err := decode(data, c); err != nil {
		return nil, err
	}
	return c, c.validate(false)
}

// ParseBackup decodes TOML (unknown keys are errors) and validates; it reads no file.
func ParseBackup(data []byte) (*Backup, error) {
	c := &Backup{}
	if err := decode(data, c); err != nil {
		return nil, err
	}
	return c, c.validate(true)
}

// validate checks the file; network adds server and [tls], which only the store needs.
func (c *Backup) validate(network bool) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if network && !validServerURL(c.Server) {
		add("server: %q must be https://<host>:<port>, port 1..65535, no path", c.Server)
	}
	if !nameRE.MatchString(c.Bucket) {
		add("bucket: %q must match [a-z0-9-]+", c.Bucket)
	}
	if !cleanAbsolute(c.DataKeyFile) {
		add("data_key_file: %q must be a clean absolute path", c.DataKeyFile)
	}
	if len(c.Exports) == 0 {
		add("exports: at least one export required")
	}
	type seen struct{ path, at string }
	var all []seen // every valid path so far, of every export
	for _, name := range slices.Sorted(maps.Keys(c.Exports)) {
		if !validSegment(name, keySegmentRE) {
			add("exports: %q must be a key segment: [A-Za-z0-9._-], not '.' or '..'", name)
		}
		paths := c.Exports[name].Paths
		if len(paths) == 0 {
			add("exports.%s.paths: at least one path required", name)
		}
		for i, p := range paths {
			at := fmt.Sprintf("exports.%s.paths[%d]", name, i)
			if !cleanAbsolute(p) {
				add("%s: %q must be a clean absolute path", at, p)
				continue
			}
			if p == "/" { // its entry path would be "", which restore refuses
				add("%s: \"/\" is not allowed, list the directories to back up", at)
				continue
			}
			for _, o := range all {
				switch {
				case p == o.path:
					add("%s: %q listed twice, in %s", at, p, o.at)
				case under(p, o.path) || under(o.path, p):
					add("%s: %q overlaps %s %q", at, p, o.at, o.path)
				}
			}
			all = append(all, seen{p, at})
		}
	}
	if network {
		c.TLS.validate(add)
		if _, err := CheckID(c.TLS.ServerID, c.TLS.TrustDomain); err != nil {
			add("tls.server_id: %q %v", c.TLS.ServerID, err)
		}
	}
	return errors.Join(errs...)
}

func validServerURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != s {
		return false
	}
	p, err := strconv.ParseUint(u.Port(), 10, 16)
	return err == nil && p != 0
}
