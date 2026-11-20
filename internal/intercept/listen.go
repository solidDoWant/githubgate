package intercept

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// DefaultListen is the interceptor's own loopback address, so it can't
// collide with a container's 127.0.0.1:443 or catch localhost traffic.
const DefaultListen = "127.43.0.1:443"

// ListenAddress parses address as a specific IP and port. An unspecified
// address (0.0.0.0, ::, or an IPv4-mapped ::ffff:0.0.0.0, which the kernel
// binds as the wildcard too) is refused: anything that reaches the pod IP
// could then make requests as the pod's ServiceAccount.
func ListenAddress(address string) (netip.AddrPort, error) {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("listen address %q must be a specific IP and port", address)
	}

	if addrPort.Addr().Unmap().IsUnspecified() || addrPort.Addr().Zone() != "" {
		return netip.AddrPort{}, fmt.Errorf("listen address %q must be a specific IP, not an unspecified one", address)
	}

	return addrPort, nil
}

// Listen opens the interceptor's listener on address. A non-loopback
// address is bound with IP_FREEBIND, because a KubeVirt launcher pod's
// guest gateway only appears after the interceptor starts.
func Listen(ctx context.Context, address string) (net.Listener, error) {
	addrPort, err := ListenAddress(address)
	if err != nil {
		return nil, err
	}

	var config net.ListenConfig

	if !addrPort.Addr().IsLoopback() {
		config.Control = freeBind
	}

	return config.Listen(ctx, "tcp", addrPort.String())
}
