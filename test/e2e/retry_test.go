//go:build e2e

package e2e

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTransientError(t *testing.T) {
	tests := map[string]struct {
		output string
		want   bool
	}{
		"etcd timeout": {"server-side apply failed: etcdserver: request timed out", true},
		"leader moved": {"Error: etcdserver: leader changed", true},
		"unavailable":  {"Error from server (ServiceUnavailable): x", true},
		"validation":   {"The CustomResourceDefinition \"x\" is invalid: spec.names: Invalid value", false},
		"admission":    {"admission webhook \"v.example\" denied the request", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTransientError(tt.output))
		})
	}
}

func TestRetryTransientStopsOnRejection(t *testing.T) {
	calls := 0
	_, err := retryTransient(t.Context(), func() (string, error) {
		calls++
		return "is invalid", errors.New("exit 1")
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

func TestRetryTransientRecovers(t *testing.T) {
	calls := 0
	out, err := retryTransient(t.Context(), func() (string, error) {
		calls++
		if calls == 1 {
			return "etcdserver: request timed out", errors.New("exit 1")
		}

		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
	assert.Equal(t, 2, calls)
}
