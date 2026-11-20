package intercept

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// freeBind sets IP_FREEBIND (or IPV6_FREEBIND) so the socket binds an
// address that isn't on an interface yet.
func freeBind(network, _ string, conn syscall.RawConn) error {
	var sockErr error

	err := conn.Control(func(fd uintptr) {
		if network == "tcp6" {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_FREEBIND, 1)

			return
		}

		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
	})
	if err != nil {
		return err
	}

	return sockErr
}
