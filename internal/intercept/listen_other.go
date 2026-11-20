//go:build !linux

package intercept

import (
	"errors"
	"syscall"
)

// freeBind fails where IP_FREEBIND doesn't exist, so a non-loopback
// listen address is refused rather than bound some other way.
func freeBind(_, _ string, _ syscall.RawConn) error {
	return errors.New("binding a non-loopback listen address needs IP_FREEBIND, which only Linux has")
}
