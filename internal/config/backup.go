package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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
	// Paths are the trees to back up: absolute, none inside another (ADR 0006).
	Paths []string  `toml:"paths"`
	TLS   BackupTLS `toml:"tls"`
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

// ParseBackup decodes TOML (unknown keys are errors) and validates; it reads no file.
func ParseBackup(data []byte) (*Backup, error) {
	c := &Backup{}
	if err := decode(data, c); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Backup) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !validServerURL(c.Server) {
		add("server: %q must be https://<host>:<port>, port 1..65535, no path", c.Server)
	}
	if !nameRE.MatchString(c.Bucket) {
		add("bucket: %q must match [a-z0-9-]+", c.Bucket)
	}
	if !cleanAbsolute(c.DataKeyFile) {
		add("data_key_file: %q must be a clean absolute path", c.DataKeyFile)
	}
	if len(c.Paths) == 0 {
		add("paths: at least one path required")
	}
	for i, p := range c.Paths {
		if !cleanAbsolute(p) {
			add("paths[%d]: %q must be a clean absolute path", i, p)
			continue
		}
		for j, o := range c.Paths[:i] {
			switch {
			case !cleanAbsolute(o):
			case p == o:
				add("paths[%d]: %q listed twice", i, p)
			case under(p, o) || under(o, p):
				add("paths[%d]: %q overlaps paths[%d] %q", i, p, j, o)
			}
		}
	}
	c.TLS.validate(add)
	if _, err := CheckID(c.TLS.ServerID, c.TLS.TrustDomain); err != nil {
		add("tls.server_id: %q %v", c.TLS.ServerID, err)
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
