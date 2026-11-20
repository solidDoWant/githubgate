//go:build live

package live_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestTokenWorksInCleanupAfterContextCancel(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}

		_, _ = writer.Write([]byte(`{"token":"t"}`))
	}))
	t.Cleanup(srv.Close)

	var result tokenResult

	t.Run("inner", func(t *testing.T) {
		live := &liveEnv{srv: srv, log: &transcript{tokens: map[string]bool{}}, tmp: t.TempDir()}
		t.Cleanup(func() {
			result = live.requestToken(t, "sa", map[string]any{})
		})
	})

	assert.Equal(t, http.StatusOK, result.status)
	assert.Equal(t, "t", result.token)
	assert.Len(t, received, 1)
}

func TestRunResolvesClientFromBinDirectory(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, clientBinary)
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho client-ran\n"), 0o700))

	// The test process's PATH does not contain bin.
	live := &liveEnv{bin: bin, home: t.TempDir(), srv: httptest.NewServer(nil), log: &transcript{tokens: map[string]bool{}}, tmp: t.TempDir()}
	t.Cleanup(live.srv.Close)

	out, err := live.run(t, "sa", live.tmp, clientBinary)
	require.NoError(t, err)
	assert.Contains(t, out, "client-ran")
}
