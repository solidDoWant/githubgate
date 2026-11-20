//go:build live

package live_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/auth"
)

func TestRecordingAuthenticatorSchemes(t *testing.T) {
	tests := map[string]struct {
		header  string
		wantErr error
	}{
		"bearer":      {header: "Bearer ns/sa"},
		"token":       {header: "token ns/sa"},
		"token cased": {header: "TOKEN ns/sa"},
		"basic":       {header: "Basic bnM6bnMvc2E="},
		"unknown":     {header: "Digest ns/sa", wantErr: auth.ErrNoCredential},
		"none":        {wantErr: auth.ErrNoCredential},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}

			identity, err := recordingAuthenticator{}.Authenticate(t.Context(), request)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, "ns", identity.Namespace)
			assert.Equal(t, "sa", identity.ServiceAccount)
		})
	}
}
