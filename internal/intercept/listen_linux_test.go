package intercept

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// notLocalAddress is in TEST-NET-1, which no test host has on an interface.
const notLocalAddress = "192.0.2.1:0"

func TestListenBindsAnAddressThatIsNotOnAnInterfaceYet(t *testing.T) {
	var plain net.ListenConfig

	plainListener, err := plain.Listen(t.Context(), "tcp", notLocalAddress)
	if err == nil {
		_ = plainListener.Close()

		t.Skip("192.0.2.1 is a local address on this host")
	}

	require.ErrorIs(t, err, unix.EADDRNOTAVAIL)

	listener, err := Listen(t.Context(), notLocalAddress)
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	assert.Equal(t, "192.0.2.1", listener.Addr().(*net.TCPAddr).IP.String())
}

func TestListenServesOnTheOwnLoopbackAddress(t *testing.T) {
	listener, err := Listen(t.Context(), "127.43.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}
