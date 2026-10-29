package key

import (
	"errors"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, k := range []string{
		"a", "node-1/chunks/ab/abcdef", "n/refs/2026-10-29t12.00", "x_y/.hidden/..x/x..", "a/" + strings.Repeat("b", MaxLen-2),
	} {
		if err := Check(k); err != nil {
			t.Errorf("Check(%q) = %v, want nil", k, err)
		}
	}
	for _, k := range []string{
		"", strings.Repeat("a", MaxLen+1), "/a", "a/", "a//b", ".", "..", "a/./b", "a/../b", "../a",
		"A", "a b", "a\\b", "a\x00", "é", "a/b?c", "a%2fb",
	} {
		if err := Check(k); !errors.Is(err, ErrInvalid) {
			t.Errorf("Check(%q) = %v, want ErrInvalid", k, err)
		}
	}
}

func TestNamespace(t *testing.T) {
	for k, want := range map[string]string{"node/chunks/ab": "node", "node": "node", "a.b/c": "a.b"} {
		if got := Namespace(k); got != want {
			t.Errorf("Namespace(%q) = %q, want %q", k, got, want)
		}
	}
}
