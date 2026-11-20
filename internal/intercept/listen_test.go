package intercept

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListenAddressAcceptsOnlyASpecificIP(t *testing.T) {
	tests := []struct {
		address     string
		assertError require.ErrorAssertionFunc
	}{
		{address: DefaultListen},
		{address: "10.0.2.1:443"},
		{address: "[fd00::1]:443"},
		{address: "0.0.0.0:443", assertError: require.Error},
		{address: "[::]:443", assertError: require.Error},
		{address: "[::ffff:0.0.0.0]:443", assertError: require.Error},
		{address: "[::ffff:0:0]:443", assertError: require.Error},
		{address: ":443", assertError: require.Error},
		{address: "localhost:443", assertError: require.Error},
		{address: "10.0.2.1", assertError: require.Error},
		{address: "[fe80::1%eth0]:443", assertError: require.Error},
	}

	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			if test.assertError == nil {
				test.assertError = require.NoError
			}

			_, err := ListenAddress(test.address)
			test.assertError(t, err)
		})
	}
}

func TestListenRefusesAnUnspecifiedAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "[::ffff:0.0.0.0]:0", "[::ffff:0:0]:0"} {
		listener, err := Listen(t.Context(), address)
		if listener != nil {
			_ = listener.Close()
		}

		require.Error(t, err, address)
	}
}
