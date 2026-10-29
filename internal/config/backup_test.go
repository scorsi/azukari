package config

import "testing"

func TestBackupValid(t *testing.T) {
	c, err := LoadBackup("testdata/backup/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := BackupTLS{TLS{"example.org", "/run/azukari-backup/svid.pem", "/run/azukari-backup/key.pem", "/run/azukari-backup/bundle.pem"}, "spiffe://example.org/store-host/azukari-server"}
	if c.Server != "https://store.example.org:8443" || c.Bucket != "backup" || c.DataKeyFile != "/var/lib/azukari-backup/data.key" || len(c.Paths) != 3 || c.TLS != want {
		t.Fatalf("unexpected decode: %+v", c)
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
