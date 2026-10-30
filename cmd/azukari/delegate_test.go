package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary also plays the copies it runs as: `azukari` runs main, `azukari-backup` echoes
// what it received.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "azukari":
		main()
	case "azukari-backup":
		fmt.Printf("%q %s\n", os.Args[1:], os.Getenv("AZUKARI_TEST"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBackupDelegation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, d := range []string{"pkg", "link", "lone"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"pkg/azukari", "pkg/azukari-backup", "lone/azukari"} {
		if err := os.WriteFile(filepath.Join(dir, p), bin, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// As a profile's bin/: azukari-backup is looked up next to the link's target.
	if err := os.Symlink(filepath.Join(dir, "pkg/azukari"), filepath.Join(dir, "link/azukari")); err != nil {
		t.Fatal(err)
	}
	cmd := func(path string, args ...string) (int, string, string) {
		c := exec.Command(path, args...)
		c.Env = append(os.Environ(), "AZUKARI_TEST=env-kept", "PATH=") // never through PATH
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		err := c.Run()
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode(), out.String(), errOut.String()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, out.String(), errOut.String()
	}
	link := filepath.Join(dir, "link/azukari")
	args := []string{"restore", "--verify", "--from-dir", "/mnt/copy", "--socket", "c.toml", "s1", "/tmp/t"}
	if code, out, errOut := cmd(link, append([]string{"backup"}, args...)...); code != 0 || out != fmt.Sprintf("%q env-kept\n", args) {
		t.Fatalf("restore = %d %q %q", code, out, errOut)
	}
	if code, out, _ := cmd(link, "backup", "snapshots", "c.toml"); code != 0 || out != `["snapshots" "c.toml"] env-kept`+"\n" {
		t.Fatalf("snapshots = %d %q", code, out)
	}
	for _, bad := range [][]string{{"backup"}, {"backup", "run", "c.toml"}, {"backup", "check", "c.toml"}, {"--socket", "s", "backup", "snapshots", "c.toml"}} {
		if code, out, errOut := cmd(link, bad...); code != 2 || out != "" || !strings.HasPrefix(errOut, "usage:") {
			t.Errorf("%q = %d %q %q", bad, code, out, errOut)
		}
	}
	if code, _, errOut := cmd(filepath.Join(dir, "lone/azukari"), "backup", "snapshots", "c.toml"); code != 1 ||
		!strings.Contains(errOut, `"event":"admin-failed"`) || !strings.Contains(errOut, "azukari-backup") {
		t.Fatalf("missing azukari-backup = %d %q", code, errOut)
	}
}
