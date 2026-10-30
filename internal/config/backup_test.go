package config

import (
	"os"
	"slices"
	"testing"
)

func TestBackupValid(t *testing.T) {
	c, err := LoadBackup("testdata/backup/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := BackupTLS{TLS{"example.org", "/run/azukari-backup/svid.pem", "/run/azukari-backup/key.pem", "/run/azukari-backup/bundle.pem"}, "spiffe://example.org/store-host/azukari-server"}
	if c.Server != "https://store.example.org:8443" || c.Bucket != "backup" || c.DataKeyFile != "/var/lib/azukari-backup/data.key" || c.TLS != want {
		t.Fatalf("unexpected decode: %+v", c)
	}
	if len(c.Exports) != 2 || len(c.Exports["data"].Paths) != 3 || !slices.Equal(c.Exports["web.1"].Paths, []string{"/srv/www"}) {
		t.Fatalf("unexpected exports: %+v", c.Exports)
	}
}

func TestBackupRefused(t *testing.T) {
	testRefused(t, "testdata/backup/refused", func(b []byte) error { _, err := ParseBackup(b); return err })
}

func TestValidServerURL(t *testing.T) {
	for _, s := range []string{"https://store.example.org:8443", "https://[::1]:8443", "https://192.0.2.1:443"} {
		if !validServerURL(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", "https://:8443", "https://store.example.org:65536", "https://store.example.org:8443?x", "https://store.example.org:8443#f", "HTTPS://store.example.org:8443", "https:store.example.org:8443"} {
		if validServerURL(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

// A restore from a copy needs neither server nor [tls]; everything else is still checked.
func TestBackupOffline(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"bucket = \"backup\"\ndata_key_file = \"/k\"\n[exports.a]\npaths = [\"/a\"]\n", true},
		{"bucket = \"backup\"\ndata_key_file = \"/k\"\n[exports.a]\npaths = [\"/a\"]\n[tls]\nsvid_file = \"relative\"\n", true},
		{"bucket = \"Backup\"\ndata_key_file = \"/k\"\n[exports.a]\npaths = [\"/a\"]\n", false},
		{"bucket = \"backup\"\ndata_key_file = \"/k\"\n[exports.a]\npaths = [\"/a\"]\nnode = \"x\"\n", false},
	} {
		p := dir + "/c.toml"
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBackupOffline(p); (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.body, err)
		}
		if _, err := LoadBackup(p); err == nil {
			t.Errorf("%q accepted by LoadBackup", tc.body)
		}
	}
}
