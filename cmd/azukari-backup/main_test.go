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
		{"run c e", "run", "", "c e", false},
		{"snapshots c e", "snapshots", "", "c e", false},
		{"snapshots --from-dir d c e", "snapshots", "d", "c e", false},
		{"restore c e s t", "restore", "", "c e s t", false},
		{"restore --verify c e s t", "restore", "", "c e s t", true},
		{"restore --from-dir d c e s t", "restore", "d", "c e s t", false},
		{"restore --verify --from-dir d c e s t", "restore", "d", "c e s t", true},
		{"restore --from-dir d --verify c e s t", "restore", "d", "c e s t", true},
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
		{"run", "c"},
		{"run", "c", "e", "x"},
		{"run", "--from-dir", "d", "c", "e"},
		{"snapshots"},
		{"snapshots", "c"},
		{"snapshots", "c", "e", "x"},
		{"snapshots", "--verify", "c", "e"},
		{"snapshots", "--from-dir", "d", "--from-dir", "e", "c", "e"},
		{"snapshots", "--from-dir"},
		{"snapshots", "--from-dir", "", "c", "e"},
		{"snapshots", "--other", "c", "e"},
		{"restore", "c", "s", "t"},
		{"restore", "c", "e", "s", "t", "x"},
		{"restore", "--verify", "--verify", "c", "e", "s", "t"},
		{"restore", "--from-dir", "d", "--from-dir", "e", "c", "e", "s", "t"},
		{"restore", "--from-dir", "c", "e", "s", "t"},
	}
	for _, in := range bad {
		if _, _, _, _, err := parseArgs(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}
