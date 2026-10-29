package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerValid(t *testing.T) {
	c, err := LoadServer("testdata/server/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Listen) != 2 || c.TLS != (TLS{"example.org", "/run/azukari-server/svid.pem", "/run/azukari-server/key.pem", "/run/azukari-server/bundle.pem"}) || len(c.Buckets) != 2 || len(c.Classes) != 2 || len(c.Replicas) != 2 {
		t.Fatalf("unexpected decode: %+v", c)
	}
	b := c.Buckets[0]
	if b.Retention.Enabled || b.Retention.Grace != DefaultGrace || len(b.Nodes) != 2 || b.MaxObjectBytes != 16<<20 {
		t.Fatalf("bucket defaults: %+v", b)
	}
	if c.Buckets[1].Retention.Grace != "1d12h" || !c.Buckets[1].Retention.Enabled {
		t.Fatalf("explicit retention lost: %+v", c.Buckets[1].Retention)
	}
}

func TestServerPortDefault(t *testing.T) {
	data, err := os.ReadFile("testdata/server/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseServer([]byte(strings.Replace(string(data), "port = 2222\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Replicas[0].Port != 22 || c.Replicas[1].Port != 0 {
		t.Fatalf("ports: %d %d", c.Replicas[0].Port, c.Replicas[1].Port)
	}
}

func TestServerRefused(t *testing.T) {
	testRefused(t, "testdata/server/refused", func(b []byte) error { _, err := ParseServer(b); return err })
}

// testRefused: each fixture's first line is `# want: <part of the error>`.
func testRefused(t *testing.T, dir string, parse func([]byte) error) {
	files, _ := filepath.Glob(dir + "/*.toml")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			first, _, _ := strings.Cut(string(data), "\n")
			want, ok := strings.CutPrefix(first, "# want: ")
			if !ok {
				t.Fatal("first line must be # want: <error>")
			}
			err = parse(data)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want an error containing %q", err, want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	for s, want := range map[string]time.Duration{
		"48h": 48 * time.Hour, "30d": 30 * 24 * time.Hour, "1d12h": 36 * time.Hour,
		"90s": 90 * time.Second, "1d2h3m4s": 26*time.Hour + 3*time.Minute + 4*time.Second,
	} {
		if got, err := parseDuration(s); err != nil || got != want {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
	for _, s := range []string{"", "0s", "0d0h", "1w", "1h1d", "1.5h", "-1h", "1 h", "1H", "106752d"} {
		if _, err := parseDuration(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestParseSize(t *testing.T) {
	for s, want := range map[string]int64{"0B": 0, "1KiB": 1024, "20GiB": 20 << 30, "2TiB": 2 << 40} {
		if got, err := ParseSize(s); err != nil || got != want {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
	for _, s := range []string{"", "20", "20GB", "20gib", "-1GiB", "8388608TiB"} {
		if _, err := ParseSize(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
