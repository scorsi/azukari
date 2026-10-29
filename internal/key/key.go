// Package key checks object keys (ADR 0003, alphabet ADR 0015). The store's layout and its HTTP layer share it.
package key

import (
	"errors"
	"fmt"
	"strings"
)

// MaxLen is the longest key, in bytes (ADR 0003).
const MaxLen = 256

// ErrInvalid is wrapped by every refusal of Check; the HTTP layer answers it with key-invalid.
var ErrInvalid = errors.New("key invalid")

// Check accepts 1 to 256 bytes of [A-Za-z0-9._-] in "/"-separated segments, with no empty,
// "." or ".." segment (hence no leading, trailing or doubled "/").
func Check(k string) error {
	if len(k) == 0 || len(k) > MaxLen {
		return fmt.Errorf("%w: length %d not in 1..%d", ErrInvalid, len(k), MaxLen)
	}
	for i := 0; i < len(k); i++ {
		if c := k[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' || c == '/') {
			return fmt.Errorf("%w: byte %q at %d", ErrInvalid, c, i)
		}
	}
	for _, s := range strings.Split(k, "/") {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("%w: segment %q", ErrInvalid, s)
		}
	}
	return nil
}

// Namespace is the key's first segment: a node's name in a backup bucket (ADR 0003).
func Namespace(k string) string {
	ns, _, _ := strings.Cut(k, "/")
	return ns
}
