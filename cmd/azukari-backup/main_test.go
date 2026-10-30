package main

import (
	"slices"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	ok := []struct {
		in, call, fromDir, rest string
		verify                  bool
	}{
		{"check c", "check", "", "c", false},
		{"run c", "run", "", "c", false},
		{"snapshots c", "snapshots", "", "c", false},
		{"snapshots --from-dir d c", "snapshots", "d", "c", false},
		{"restore c s t", "restore", "", "c s t", false},
		{"restore --verify c s t", "restore", "", "c s t", true},
		{"restore --from-dir d c s t", "restore", "d", "c s t", false},
		{"restore --verify --from-dir d c s t", "restore", "d", "c s t", true},
		{"restore --from-dir d --verify c s t", "restore", "d", "c s t", true},
	}
	for _, c := range ok {
		in := strings.Fields(c.in)
		orig := slices.Clone(in)
		call, verify, fromDir, rest, err := parseArgs(in)
		if err != nil || call != c.call || verify != c.verify || fromDir != c.fromDir || strings.Join(rest, " ") != c.rest {
			t.Errorf("%q: got %q %t %q %q %v", c.in, call, verify, fromDir, rest, err)
		}
		if !slices.Equal(in, orig) {
			t.Errorf("%q: input changed to %q", c.in, in)
		}
	}
	bad := [][]string{
		{},
		{"backup", "c"},
		{"check"},
		{"check", "c", "x"},
		{"check", "--verify", "c"},
		{"run", "--from-dir", "d", "c"},
		{"snapshots"},
		{"snapshots", "--verify", "c"},
		{"snapshots", "--from-dir", "d", "--from-dir", "e", "c"},
		{"snapshots", "--from-dir"},
		{"snapshots", "--from-dir", "", "c"},
		{"snapshots", "--other", "c"},
		{"restore", "c", "s"},
		{"restore", "c", "s", "t", "x"},
		{"restore", "--verify", "--verify", "c", "s", "t"},
		{"restore", "--from-dir", "d", "--from-dir", "e", "c", "s", "t"},
		{"restore", "--from-dir", "c", "s", "t"},
	}
	for _, in := range bad {
		if _, _, _, _, err := parseArgs(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}
