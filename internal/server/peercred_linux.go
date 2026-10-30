package server

import "syscall"

// sockUID reads the peer's uid with SO_PEERCRED (ADR 0005, 0024), -1 on error.
func sockUID(fd int) int {
	u, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return -1
	}
	return int(u.Uid)
}
