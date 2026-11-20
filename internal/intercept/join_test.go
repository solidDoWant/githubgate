package intercept

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJoin(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  Join
		err   require.ErrorAssertionFunc
	}{
		"valid":              {value: "main/SSL_CERT_FILE=/a/b", want: Join{Container: "main", Variable: "SSL_CERT_FILE", Source: "/a/b"}, err: require.NoError},
		"no source":          {value: "main/SSL_CERT_FILE=", err: require.Error},
		"no variable":        {value: "main=/a", err: require.Error},
		"container climbs":   {value: "../SSL_CERT_FILE=/a", err: require.Error},
		"variable has slash": {value: "main/a/b=/a", err: require.Error},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseJoin(test.value)
			test.err(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestJoinedFileFollowsItsSource(t *testing.T) {
	t.Parallel()

	stub := newStubProxy(t, nil)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	source := filepath.Join(t.TempDir(), "own.pem")
	trustDir := filepath.Join(t.TempDir(), "trust")
	joined := filepath.Join(trustDir, "join", "main", "SSL_CERT_FILE")

	done := make(chan error, 1)
	runCtx, cancelRun := context.WithCancel(t.Context())

	stateDir := filepath.Join(t.TempDir(), "state")
	tokenFile := writeToken(t, "sa-token")

	// Registered after the temporary directories, so it runs before they are removed.
	t.Cleanup(func() {
		cancelRun()
		assert.NoError(t, <-done)
	})

	go func() {
		done <- Run(runCtx, Options{
			Listener: listener, ProxyAddress: stub.address(), ProxyCA: stub.caPEM, TokenFile: tokenFile,
			TrustDir: trustDir, StateDir: stateDir,
			Joins:        []Join{{Container: "main", Variable: "SSL_CERT_FILE", Source: source}},
			JoinInterval: 10 * time.Millisecond,
		})
	}()

	caPEM := func() string {
		data, err := os.ReadFile(filepath.Join(trustDir, "ca.crt"))
		require.NoError(t, err)

		return string(data)
	}

	read := func() string {
		data, _ := os.ReadFile(joined)

		return string(data)
	}

	require.Eventually(t, func() bool { return read() != "" && Probe(t.Context(), listener.Addr().String(), trustDir) == nil }, 10*time.Second, 5*time.Millisecond)
	assert.Equal(t, caPEM(), read(), "a missing source leaves only the pod CA")

	require.NoError(t, os.WriteFile(source, []byte("own-ca"), 0o600))
	require.Eventually(t, func() bool { return read() == "own-ca\n"+caPEM() }, 10*time.Second, 5*time.Millisecond)

	require.NoError(t, os.WriteFile(source, []byte("changed\n"), 0o600))
	require.Eventually(t, func() bool { return read() == "changed\n"+caPEM() }, 10*time.Second, 5*time.Millisecond)

	info, err := os.Stat(joined)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(trustFileMode), info.Mode().Perm())
}
