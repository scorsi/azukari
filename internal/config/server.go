// Package config parses and validates azukari's TOML files: the store's (ADR 0004, 0005, 0007,
// 0008, 0009, 0011) and the backup component's (ADR 0005, 0006, 0011). Every check here needs no
// disk; mounts and bucket directories are checked at start.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Server is azukari-server's file; unknown keys are an error.
type Server struct {
	// Listen holds the mTLS listeners, host:port (ADR 0003, 0005).
	Listen []string `toml:"listen"`
	// ListenMetrics is the loopback /metrics listener, the only unauthenticated port (ADR 0005, 0009).
	ListenMetrics string           `toml:"listen_metrics"`
	TLS           TLS              `toml:"tls"`
	Buckets       []Bucket         `toml:"buckets"`
	Classes       map[string]Class `toml:"classes"`
	Replicas      []Replica        `toml:"replicas"`
}

// TLS is the [tls] table of both sides: the SVID files and the trust domain (ADR 0005, 0011).
type TLS struct {
	TrustDomain string `toml:"trust_domain"`
	SVIDFile    string `toml:"svid_file"`
	KeyFile     string `toml:"key_file"`
	BundleFile  string `toml:"bundle_file"`
}

// Bucket is one [[buckets]] table (ADR 0004, 0005, 0007, 0008).
type Bucket struct {
	Name           string    `toml:"name"`
	Type           string    `toml:"type"`
	Class          string    `toml:"class"`
	Path           string    `toml:"path"`
	MaxObjectBytes int64     `toml:"max_object_bytes"`
	DiskReserve    string    `toml:"disk_reserve"`
	MountPoint     string    `toml:"mount_point"`
	Nodes          []string  `toml:"nodes"`
	Retention      Retention `toml:"retention"`
}

// Retention is [buckets.retention] (ADR 0007). Grace defaults to DefaultGrace.
type Retention struct {
	Enabled bool   `toml:"enabled"`
	Keep    string `toml:"keep"`
	Grace   string `toml:"grace"`
	RunAt   string `toml:"run_at"`
}

// DefaultGrace is retention.grace when unset (ADR 0007).
const DefaultGrace = "48h"

// Class is one [classes.<name>] table (ADR 0008).
type Class struct {
	Replicas []string `toml:"replicas"`
}

// Replica is one [[replicas]] table (ADR 0008). Port defaults to 22 on kind ssh.
type Replica struct {
	Name           string `toml:"name"`
	Kind           string `toml:"kind"`
	Destination    string `toml:"destination"`
	Port           int    `toml:"port"`
	SSHKeyFile     string `toml:"ssh_key_file"`
	KnownHostsFile string `toml:"known_hosts_file"`
	MountPoint     string `toml:"mount_point"`
	Every          string `toml:"every"`
}

var (
	nameRE        = regexp.MustCompile(`^[a-z0-9-]+$`)
	trustDomainRE = regexp.MustCompile(`^[a-z0-9._-]+$`)
	// ADR 0003, 0015: a key segment is [A-Za-z0-9._-], neither "." nor "..".
	keySegmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// hanko's SPIFFE ID path segments (hanko ADR-0004).
	idSegmentRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	durationRE  = regexp.MustCompile(`^(?:([0-9]+)d)?(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?$`)
	sizeRE      = regexp.MustCompile(`^([0-9]+)(B|KiB|MiB|GiB|TiB)$`)
	runAtRE     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	// rsync's user@host:dir; IPv6 hosts in brackets, nothing that ssh could read as an option.
	sshDestRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*@([A-Za-z0-9][A-Za-z0-9.-]*|\[[0-9A-Fa-f:.]+\]):[^\s\x00-\x1f]+$`)
)

// LoadServer reads and validates a store file; the error lists every problem, one per line.
func LoadServer(path string) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseServer(data)
}

// ParseServer decodes TOML (unknown keys are errors), fills the defaults and validates.
func ParseServer(data []byte) (*Server, error) {
	c := &Server{}
	if err := decode(data, c); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func decode(data []byte, v any) error {
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		var errs []error
		for _, e := range strict.Errors {
			line, col := e.Position()
			errs = append(errs, fmt.Errorf("unknown key %q at %d:%d", strings.Join(e.Key(), "."), line, col))
		}
		return errors.Join(errs...)
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		line, col := de.Position()
		return fmt.Errorf("invalid TOML at %d:%d: %v", line, col, de)
	}
	return err
}

func (c *Server) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Listen) == 0 {
		add("listen: at least one address required")
	}
	seenListen := map[string]bool{}
	for i, l := range c.Listen {
		if !validListen(l, false) {
			add("listen[%d]: %q must be <ip>:<port> or :<port>, port 1..65535", i, l)
		} else if seenListen[l] {
			add("listen[%d]: %q listed twice", i, l)
		}
		seenListen[l] = true
	}
	if !validListen(c.ListenMetrics, true) {
		add("listen_metrics: %q must be a loopback <ip>:<port>, port 1..65535", c.ListenMetrics)
	}
	c.TLS.validate(add)

	replicas := map[string]bool{}
	for i := range c.Replicas {
		r := &c.Replicas[i]
		at := fmt.Sprintf("replicas[%d]", i)
		switch {
		case !nameRE.MatchString(r.Name):
			add("%s: name %q must match [a-z0-9-]+", at, r.Name)
		case replicas[r.Name]:
			add("%s: duplicate name %q", at, r.Name)
		default:
			at += " " + r.Name
		}
		replicas[r.Name] = true
		r.validate(at, add)
	}

	for name, cl := range c.Classes {
		at := "classes." + name
		if !nameRE.MatchString(name) {
			add("%s: class name must match [a-z0-9-]+", at)
		}
		seen := map[string]bool{}
		for _, r := range cl.Replicas {
			if !replicas[r] {
				add("%s: replicas: unknown replica %q", at, r)
			} else if seen[r] {
				add("%s: replicas: %q listed twice", at, r)
			}
			seen[r] = true
		}
	}

	if len(c.Buckets) == 0 {
		add("buckets: at least one bucket required")
	}
	buckets := map[string]bool{}
	for i := range c.Buckets {
		b := &c.Buckets[i]
		at := fmt.Sprintf("buckets[%d]", i)
		switch {
		case !nameRE.MatchString(b.Name):
			add("%s: name %q must match [a-z0-9-]+", at, b.Name)
		case buckets[b.Name]:
			add("%s: duplicate name %q", at, b.Name)
		default:
			at += " " + b.Name
		}
		buckets[b.Name] = true
		b.validate(at, c.TLS.TrustDomain, c.Classes, add)
		// A bucket's directory is its whole truth (ADR 0004): two buckets never share one.
		for _, o := range c.Buckets[:i] {
			if cleanAbsolute(b.Path) && cleanAbsolute(o.Path) && (under(b.Path, o.Path) || under(o.Path, b.Path) || b.Path == o.Path) {
				add("%s: path %q overlaps bucket %q's path %q", at, b.Path, o.Name, o.Path)
			}
		}
	}
	return errors.Join(errs...)
}

func (t *TLS) validate(add func(string, ...any)) {
	if !trustDomainRE.MatchString(t.TrustDomain) {
		add("tls.trust_domain: %q must be lowercase letters, digits, '.', '-' or '_'", t.TrustDomain)
	}
	for _, f := range []struct{ key, v string }{{"svid_file", t.SVIDFile}, {"key_file", t.KeyFile}, {"bundle_file", t.BundleFile}} {
		if !cleanAbsolute(f.v) {
			add("tls.%s: %q must be a clean absolute path", f.key, f.v)
		}
	}
}

func (b *Bucket) validate(at, trustDomain string, classes map[string]Class, add func(string, ...any)) {
	if b.Type != "backup" {
		add("%s: type %q unknown (only \"backup\")", at, b.Type)
	}
	if _, ok := classes[b.Class]; !ok {
		add("%s: class %q unknown", at, b.Class)
	}
	if !cleanAbsolute(b.MountPoint) {
		add("%s: mount_point %q must be a clean absolute path", at, b.MountPoint)
	}
	if !cleanAbsolute(b.Path) {
		add("%s: path %q must be a clean absolute path", at, b.Path)
	} else if cleanAbsolute(b.MountPoint) && !under(b.Path, b.MountPoint) {
		add("%s: path %q must be under mount_point %q", at, b.Path, b.MountPoint)
	}
	if b.MaxObjectBytes <= 0 {
		add("%s: max_object_bytes must be positive", at)
	}
	if _, err := ParseSize(b.DiskReserve); err != nil {
		add("%s: disk_reserve: %v", at, err)
	}
	seen := map[string]bool{}
	for j, id := range b.Nodes {
		if err := checkNodeID(id, trustDomain); err != nil {
			add("%s: nodes[%d]: %q %v", at, j, id, err)
		} else if seen[id] {
			add("%s: nodes[%d]: %q listed twice", at, j, id)
		}
		seen[id] = true
	}

	r := &b.Retention
	if r.Keep == "" {
		add("%s: retention.keep required", at)
	} else if _, err := parseDuration(r.Keep); err != nil {
		add("%s: retention.keep: %v", at, err)
	}
	if r.Grace == "" {
		r.Grace = DefaultGrace
	}
	if _, err := parseDuration(r.Grace); err != nil {
		add("%s: retention.grace: %v", at, err)
	}
	if !runAtRE.MatchString(r.RunAt) {
		add("%s: retention.run_at %q must be hh:mm, 00:00 to 23:59", at, r.RunAt)
	}
}

func (r *Replica) validate(at string, add func(string, ...any)) {
	if _, err := parseDuration(r.Every); err != nil {
		add("%s: every: %v", at, err)
	}
	switch r.Kind {
	case "ssh":
		if !sshDestRE.MatchString(r.Destination) {
			add("%s: destination %q must be <user>@<host>:<dir>", at, r.Destination)
		}
		if r.Port == 0 {
			r.Port = 22
		}
		if r.Port < 1 || r.Port > 65535 {
			add("%s: port %d must be 1..65535", at, r.Port)
		}
		if !cleanAbsolute(r.SSHKeyFile) {
			add("%s: ssh_key_file %q must be a clean absolute path", at, r.SSHKeyFile)
		}
		if !cleanAbsolute(r.KnownHostsFile) {
			add("%s: known_hosts_file %q must be a clean absolute path", at, r.KnownHostsFile)
		}
		if r.MountPoint != "" {
			add("%s: mount_point belongs to kind dir, not ssh", at)
		}
	case "dir":
		if !cleanAbsolute(r.MountPoint) {
			add("%s: mount_point %q must be a clean absolute path", at, r.MountPoint)
		}
		if !cleanAbsolute(r.Destination) {
			add("%s: destination %q must be a clean absolute path", at, r.Destination)
		} else if cleanAbsolute(r.MountPoint) && !under(r.Destination, r.MountPoint) {
			add("%s: destination %q must be under mount_point %q", at, r.Destination, r.MountPoint)
		}
		if r.Port != 0 || r.SSHKeyFile != "" || r.KnownHostsFile != "" {
			add("%s: port, ssh_key_file and known_hosts_file belong to kind ssh, not dir", at)
		}
	default:
		add("%s: kind %q unknown (want ssh or dir)", at, r.Kind)
	}
}

// checkNodeID: a SPIFFE ID whose host is a key segment, the node's namespace (ADR 0003, 0005).
func checkNodeID(id, trustDomain string) error {
	host, err := CheckID(id, trustDomain)
	if err != nil {
		return err
	}
	if !validSegment(host, keySegmentRE) {
		return errors.New("host segment must be a key segment: [A-Za-z0-9._-], not '.' or '..'")
	}
	return nil
}

// CheckID checks spiffe://<trust domain>/<host>/<service> (hanko ADR-0004); returns the host segment.
func CheckID(id, trustDomain string) (string, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "spiffe" || u.Opaque != "" || u.User != nil || u.Port() != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.String() != id {
		return "", errors.New("is not spiffe://<trust domain>/<host>/<service>")
	}
	if u.Host != trustDomain {
		return "", fmt.Errorf("is outside trust domain %q", trustDomain)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if !strings.HasPrefix(u.Path, "/") || len(segs) != 2 || !validSegment(segs[0], idSegmentRE) || !validSegment(segs[1], idSegmentRE) {
		return "", errors.New("is not spiffe://<trust domain>/<host>/<service>")
	}
	return segs[0], nil
}

func validSegment(s string, re *regexp.Regexp) bool {
	return s != "." && s != ".." && re.MatchString(s)
}

// parseDuration reads ADR 0007's durations: d, h, m, s in that order, e.g. "30d" or "1d12h"; positive.
func parseDuration(s string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(s)
	bad := fmt.Errorf("%q must be a positive duration of d, h, m, s such as \"30d\" or \"1d12h\"", s)
	if s == "" || m == nil {
		return 0, bad
	}
	var total time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64((1<<63-1)/unit) || total > 1<<63-1-time.Duration(n)*unit {
			return 0, bad
		}
		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, bad
	}
	return total, nil
}

// ParseSize reads "<n><unit>", unit B, KiB, MiB, GiB or TiB, e.g. "20GiB".
func ParseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(s)
	bad := fmt.Errorf("%q must be a size such as \"20GiB\" (B, KiB, MiB, GiB, TiB)", s)
	if m == nil {
		return 0, bad
	}
	shift := map[string]uint{"B": 0, "KiB": 10, "MiB": 20, "GiB": 30, "TiB": 40}[m[2]]
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > (1<<63-1)>>shift {
		return 0, bad
	}
	return n << shift, nil
}

func validListen(s string, loopback bool) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return false
	}
	if host == "" {
		return !loopback
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Zone() == "" && (!loopback || a.IsLoopback())
}

func cleanAbsolute(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, "\x00\n\r")
}

// under: p is strictly inside dir (both clean absolute paths).
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}
