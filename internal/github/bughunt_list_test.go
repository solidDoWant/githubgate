package github_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The design limits installation listings to one every 30 s per App. A
// listing that fails records no time, so while GitHub fails every request
// lists again.
func TestBughuntFailedListingsAreNotLimited(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFixture(t)
			f.setOverride(func(writer http.ResponseWriter, request *http.Request) bool {
				if request.URL.Path == "/app/installations" {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(status)
					_, _ = writer.Write([]byte(`{"message":"boom"}`))

					return true
				}

				return false
			})
			client := f.client(t, nil)

			for range 5 {
				_, err := client.InstallationID(t.Context(), owner)
				require.Error(t, err)
			}

			// Same clock instant: at most one listing is allowed.
			t.Logf("listings at one instant: %d", f.count(listInstallations))
			require.Equal(t, 1, f.count(listInstallations))
		})
	}
}
