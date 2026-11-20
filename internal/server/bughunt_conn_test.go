package server_test

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An unauthenticated client that sends one request and then stays idle, or
// announces a body it never sends, keeps its connection (and the goroutine
// serving it) forever: the token API sets no IdleTimeout or ReadTimeout.
func TestBughuntIdleAndStalledConnectionsAreNeverClosed(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t, t.TempDir(), 1)
	r := startRun(t, f, certFile, keyFile, 0)

	for _, name := range []string{"idle after a full request", "body announced but never sent"} {
		t.Run(name, func(t *testing.T) {
			connection, err := tls.Dial("tcp", r.api, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
			require.NoError(t, err)
			defer connection.Close()

			body := "{}"
			length := len(body)
			if name == "body announced but never sent" {
				body, length = "", 1000
			}
			_, err = fmt.Fprintf(connection, "POST /v1/token HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", length, body)
			require.NoError(t, err)

			require.NoError(t, connection.SetReadDeadline(time.Now().Add(25*time.Second)))
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				t.Logf("no response after 25 s: %v", err)
				var netErr interface{ Timeout() bool }
				if errors.As(err, &netErr) && netErr.Timeout() {
					t.Errorf("BUG: the server neither answers nor closes the connection after 25 s")
				}
				return
			}
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			response.Body.Close()

			// Well past the 10 s header timeout: the server should have closed the connection.
			require.NoError(t, connection.SetReadDeadline(time.Now().Add(25*time.Second)))
			buffer := make([]byte, 1)
			_, err = connection.Read(buffer)
			t.Logf("read after 25 s: %v", err)
			var netErr interface{ Timeout() bool }
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Errorf("BUG: the server still holds the connection open after 25 s")
			}
		})
	}
}
