package proxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

const (
	oldID = "1111111111111111111111111111111111111111"
	newID = "2222222222222222222222222222222222222222"
	// packData stands in for the pack that follows the command section.
	packData = "PACK\x00\x00\x00\x02pack-bytes"
)

// pkt encodes payloads as pkt-lines.
func pkt(payloads ...string) string {
	var buffer bytes.Buffer

	encoder := pktline.NewEncoder(&buffer)
	for _, payload := range payloads {
		if err := encoder.EncodeString(payload); err != nil {
			panic(err)
		}
	}

	return buffer.String()
}

const flush = "0000"

// pushBody is a receive-pack request as git sends it: one command per ref,
// the first with capabilities, a flush packet and the pack.
func pushBody(capabilities string, refs ...string) string {
	lines := []string{}

	for index, ref := range refs {
		line := oldID + " " + newID + " " + ref
		if index == 0 {
			line += "\x00" + capabilities
		}

		lines = append(lines, line)
	}

	return pkt(lines...) + flush + packData
}

func pushRequest(body string) *http.Request {
	request := authenticated(http.MethodPost, proxy.HostGit, "/acme/foo.git/git-receive-pack")
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/x-git-receive-pack-request")

	return request
}

func agentRule() *model.Rule {
	return proxyRule("a", app1Installation, []string{"foo"}, write("contents"), pushRefs("refs/heads/agent/**"))
}

func TestProxyForwardsAPushOfAllowedRefsUnchanged(t *testing.T) {
	forwarding := newForwardingProxy(t, agentRule())

	body := pushBody("report-status side-band-64k agent=git/2.47.0", "refs/heads/agent/x", "refs/heads/agent/y/z")
	response := serve(forwarding.handler, pushRequest(body))

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, "/git/acme/foo.git/git-receive-pack", received[0].path)
	assert.Equal(t, body, received[0].body, "the push reached GitHub changed")

	lines := forwarding.auditLines(t)
	require.Len(t, lines, 1)
	assert.Equal(t, []any{"refs/heads/agent/x", "refs/heads/agent/y/z"}, lines[0]["refs"])
}

func TestProxyForwardsAPushWithShallowLines(t *testing.T) {
	forwarding := newForwardingProxy(t, agentRule())

	body := pkt("shallow "+oldID+"\n", "shallow "+newID+"\n", oldID+" "+newID+" refs/heads/agent/x\x00report-status") + flush + packData
	response := serve(forwarding.handler, pushRequest(body))

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, body, received[0].body)
}

// reportOf decodes a receive-pack result, through the side-band when
// asked, and returns the report and the side-band progress text.
func reportOf(t *testing.T, body []byte, band sideband.Type) (*packp.ReportStatus, string) {
	t.Helper()

	var progress bytes.Buffer

	reader := io.Reader(bytes.NewReader(body))

	if band != 0 {
		demuxer := sideband.NewDemuxer(band, reader)
		demuxer.Progress = &progress

		data, err := io.ReadAll(demuxer)
		require.NoError(t, err)

		reader = bytes.NewReader(data)
	}

	report := packp.NewReportStatus()
	require.NoError(t, report.Decode(reader))

	return report, progress.String()
}

func TestProxyRefusesAPushOutsidePushRefsInGitsProtocol(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		band         sideband.Type
	}{
		{name: "report-status", capabilities: "report-status agent=git/2.47.0"},
		{name: "report-status-v2", capabilities: "report-status-v2 object-format=sha1"},
		{name: "side-band-64k", capabilities: "report-status side-band-64k", band: sideband.Sideband64k},
		{name: "side-band", capabilities: "report-status-v2 side-band", band: sideband.Sideband},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, agentRule())

			body := pushBody(test.capabilities, "refs/heads/agent/x", "refs/heads/master", "refs/tags/v1")
			request := pushRequest(body)
			remaining := &remainingReader{reader: strings.NewReader(body)}
			request.Body = io.NopCloser(remaining)

			response := serve(forwarding.handler, request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, "application/x-git-receive-pack-result", response.Header().Get("Content-Type"))
			assert.Empty(t, forwarding.upstream.requests(), "a refused push reached GitHub")
			assert.True(t, remaining.drained, "the rest of the refused push was not read")

			report, progress := reportOf(t, response.Body.Bytes(), test.band)
			assert.Equal(t, "ok", report.UnpackStatus)

			statuses := map[string]string{}
			for _, status := range report.CommandStatuses {
				statuses[status.ReferenceName.String()] = status.Status
			}

			require.Len(t, statuses, 3)
			assert.Equal(t, "githubgate: push refused (another ref)", statuses["refs/heads/agent/x"])
			assert.Contains(t, statuses["refs/heads/master"], `githubgate: `)
			assert.Contains(t, statuses["refs/heads/master"], `ref "refs/heads/master" is outside pushRefs`)
			assert.Contains(t, statuses["refs/tags/v1"], `ref "refs/tags/v1" is outside pushRefs`)

			if test.band != 0 {
				assert.Contains(t, progress, "githubgate: ")
				assert.Contains(t, progress, "refs/heads/master")
			}

			lines := forwarding.auditLines(t)
			require.Len(t, lines, 1)
			assert.Equal(t, "denied", lines[0]["reason"])
		})
	}
}

// remainingReader records whether it was read to its end.
type remainingReader struct {
	reader  io.Reader
	drained bool
}

func (r *remainingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if err == io.EOF {
		r.drained = true
	}

	return read, err
}

func TestProxyRefusesAPushWithoutReportWithForbidden(t *testing.T) {
	forwarding := newForwardingProxy(t, agentRule())

	response := serve(forwarding.handler, pushRequest(pushBody("agent=git/2.47.0", "refs/tags/v1")))

	require.Equal(t, http.StatusForbidden, response.Code)
	assert.Contains(t, response.Body.String(), `githubgate: `)
	assert.Contains(t, response.Body.String(), `ref "refs/tags/v1" is outside pushRefs`)
	assert.Empty(t, forwarding.upstream.requests())
}

func TestProxyRefusesAPushTheRulesDontAllowForEveryRef(t *testing.T) {
	// Each ref alone would be allowed, but no rule gives contents: write.
	forwarding := newForwardingProxy(t, proxyRule("a", app1Installation, []string{"foo"}, read("contents"), pushRefs("refs/heads/**")))

	response := serve(forwarding.handler, pushRequest(pushBody("report-status", "refs/heads/a", "refs/heads/b")))

	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, forwarding.upstream.requests())

	report, _ := reportOf(t, response.Body.Bytes(), 0)
	require.Len(t, report.CommandStatuses, 2)

	for _, status := range report.CommandStatuses {
		assert.Contains(t, status.Status, "git needs contents: write")
	}
}

func TestProxyRefusesMalformedOrOversizedPushes(t *testing.T) {
	commands := func(count int) string {
		lines := []string{}

		for index := range count {
			line := fmt.Sprintf("%s %s refs/heads/agent/%d", oldID, newID, index)
			if index == 0 {
				line += "\x00report-status"
			}

			lines = append(lines, line)
		}

		return pkt(lines...) + flush + packData
	}

	tests := []struct {
		name           string
		body           string
		expectedStatus int
	}{
		{name: "an empty body", body: "", expectedStatus: http.StatusBadRequest},
		{name: "no flush packet", body: pkt(oldID + " " + newID + " refs/heads/agent/x\x00report-status"), expectedStatus: http.StatusBadRequest},
		{name: "a bad pkt-line length", body: "zzzz" + flush, expectedStatus: http.StatusBadRequest},
		{name: "a truncated pkt-line", body: "00ff" + oldID, expectedStatus: http.StatusBadRequest},
		{name: "only a flush packet", body: flush + packData, expectedStatus: http.StatusBadRequest},
		{name: "a push certificate", body: pkt("push-cert\x00report-status", "certificate version 0.1\n", "push-cert-end\n") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a first command without capabilities", body: pkt(oldID+" "+newID+" refs/heads/agent/x") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a short object id", body: pkt(oldID[1:]+" "+newID+" refs/heads/agent/x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "an upper-case object id", body: pkt(strings.ToUpper("abcdef"+oldID[6:])+" "+newID+" refs/heads/agent/x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a non-hex object id", body: pkt("g"+oldID[1:]+" "+newID+" refs/heads/agent/x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "two spaces between fields", body: pkt(oldID+"  "+newID+" refs/heads/agent/x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a ref with a space", body: pkt(oldID+" "+newID+" refs/heads/agent/x refs/heads/master\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a ref with a double dot", body: pkt(oldID+" "+newID+" refs/heads/agent/../master\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a ref outside refs/", body: pkt(oldID+" "+newID+" HEAD\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a ref with a control character", body: pkt(oldID+" "+newID+" refs/heads/agent/\x01x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a malformed shallow line", body: pkt("shallow 123", oldID+" "+newID+" refs/heads/agent/x\x00report-status") + flush, expectedStatus: http.StatusBadRequest},
		{name: "a shallow line after a command", body: pkt(oldID+" "+newID+" refs/heads/agent/x\x00report-status", "shallow "+oldID) + flush, expectedStatus: http.StatusBadRequest},
		{name: "an error line", body: pkt("ERR boom") + flush, expectedStatus: http.StatusBadRequest},
		{name: "1001 commands", body: commands(1001), expectedStatus: http.StatusRequestEntityTooLarge},
		{name: "a command section of 64 KiB and one byte", body: sizedPush(64<<10 + 1), expectedStatus: http.StatusRequestEntityTooLarge},
		{name: "a command section of 64 KiB and one byte without a flush", body: strings.Repeat(pkt(oldID+" "+newID+" refs/heads/agent/x\x00report-status"), 1000), expectedStatus: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forwarding := newForwardingProxy(t, agentRule())

			response := serve(forwarding.handler, pushRequest(test.body))

			assert.Equal(t, test.expectedStatus, response.Code, response.Body.String())
			assert.True(t, strings.HasPrefix(response.Body.String(), "githubgate: "), response.Body.String())
			assert.Empty(t, forwarding.upstream.requests(), "a refused push reached GitHub")
		})
	}
}

func TestProxyAcceptsAPushWithACommandSectionOf64KiB(t *testing.T) {
	forwarding := newForwardingProxy(t, agentRule())

	body := sizedPush(64 << 10)
	response := serve(forwarding.handler, pushRequest(body))

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	received := forwarding.upstream.requests()
	require.Len(t, received, 1)
	assert.Equal(t, body, received[0].body)
}

// sizedPush returns a push whose command section, up to and including the
// flush packet, is exactly size bytes: a first command with capabilities and
// a second whose ref fills the room.
func sizedPush(size int) string {
	first := pkt(oldID + " " + newID + " refs/heads/agent/x\x00report-status")
	prefix := oldID + " " + newID + " refs/heads/agent/"
	room := size - len(first) - len(flush) - 4 - len(prefix)
	section := first + pkt(prefix+strings.Repeat("a", room)) + flush

	if len(section) != size {
		panic(fmt.Sprintf("section is %d bytes, not %d", len(section), size))
	}

	return section + packData
}
