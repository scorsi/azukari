package server

import "syscall"

// sockUID reads the peer's uid with LOCAL_PEERCRED, macOS's SO_PEERCRED, -1 on error. The
// standard library has no getter for its struct xucred {u_int cr_version; uid_t cr_uid; ...}:
// the kernel truncates it to the 32 bytes of an ICMPv6Filter, whose second word is cr_uid.
// Development only; golang.org/x/sys is linked only into azukari-backup (ADR 0018).
func sockUID(fd int) int {
	const solLocal, localPeerCred = 0, 1
	x, err := syscall.GetsockoptICMPv6Filter(fd, solLocal, localPeerCred)
	if err != nil {
		return -1
	}
	return int(x.Filt[1])
}
