package backup

import "syscall"

// hasXattrs reports extended attributes, POSIX ACLs included (system.posix_acl_*), on path.
func hasXattrs(path string) bool {
	n, err := syscall.Listxattr(path, nil)
	return err == nil && n > 0
}
