//go:build !linux

package backup

// hasXattrs: shortcut: not detected outside Linux (no listxattr in syscall), the only target.
func hasXattrs(string) bool { return false }
