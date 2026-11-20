package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
)

const (
	// maxReceiveCommands and maxReceiveCommandBytes bound the command
	// section of a push read before it is decided.
	maxReceiveCommands     = 1000
	maxReceiveCommandBytes = 64 << 10
	// maxDiscardedPushBytes bounds how much of a refused push is read and
	// thrown away so git sees the refusal instead of a broken pipe.
	maxDiscardedPushBytes = 100 << 20
	// receivePackResultType is the content type of receive-pack's answer.
	receivePackResultType = "application/x-git-receive-pack-result"
	// reportStatusV2 is the capability asking for the second report format;
	// its ng lines are the same as report-status's.
	reportStatusV2 capability.Capability = "report-status-v2"
)

var errCommandSectionTooLarge = errors.New("the push command section is larger than 64 KiB")

// receiveCommands is what a push's command section says.
type receiveCommands struct {
	// Raw is the command section exactly as read, up to and including the
	// flush packet.
	Raw []byte
	// Refs are the refs the commands write, in order.
	Refs []string
	// Capabilities are the capabilities the client asked for.
	Capabilities *capability.List
}

// readReceiveCommands reads a receive-pack request's command section up to
// its flush packet: optional shallow lines, then commands with the first one
// carrying the capabilities. A body that is only a flush packet is git's
// probe and names no ref. Anything else, a malformed object id or a ref
// that is not a valid ref name is refused, as is a section over 1000
// commands or 64 KiB. It reads nothing past the flush packet, except to
// check that a probe ends there.
func readReceiveCommands(body io.Reader) (*receiveCommands, *refusal) {
	section := &boundedRecorder{reader: body, limit: maxReceiveCommandBytes}
	scanner := pktline.NewScanner(section)
	lines := [][]byte{}
	flushed := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			flushed = true

			break
		}

		lines = append(lines, bytes.Clone(line))
	}

	if errors.Is(scanner.Err(), errCommandSectionTooLarge) || (!flushed && section.overflowed) {
		return nil, refuse(http.StatusRequestEntityTooLarge, codeTooLarge, "a push command section larger than 64 KiB is not accepted")
	}

	if !flushed {
		return nil, malformedPush("the push command list can't be read")
	}

	if len(lines) == 0 {
		return readProbe(body, section.recorded.Bytes())
	}

	commands := lines

	for len(commands) > 0 && bytes.HasPrefix(commands[0], []byte("shallow ")) {
		if !isObjectID(strings.TrimSuffix(string(commands[0][len("shallow "):]), "\n")) {
			return nil, malformedPush("a shallow line of the push is malformed")
		}

		commands = commands[1:]
	}

	if len(commands) > maxReceiveCommands {
		return nil, refuse(http.StatusRequestEntityTooLarge, codeTooLarge, fmt.Sprintf("a push of more than %d refs is not accepted", maxReceiveCommands))
	}

	if len(commands) == 0 {
		return nil, malformedPush("the push names no ref")
	}

	capabilities := capability.NewList()
	refs := make([]string, 0, len(commands))

	for index, line := range commands {
		if index == 0 {
			command, rawCapabilities, found := bytes.Cut(line, []byte{0})
			if !found {
				return nil, malformedPush("the first push command has no capabilities")
			}

			if err := capabilities.Decode(rawCapabilities); err != nil {
				return nil, malformedPush("the push capabilities can't be read")
			}

			line = command
		}

		ref, ok := commandRef(string(line))
		if !ok {
			return nil, malformedPush("a push command is malformed or names an invalid ref")
		}

		refs = append(refs, ref)
	}

	return &receiveCommands{Raw: section.recorded.Bytes(), Refs: refs, Capabilities: capabilities}, nil
}

// readProbe accepts a body that is only a flush packet: git sends it before
// a push larger than its post buffer to check that the server accepts a
// push, and GitHub answers it. It names no ref and carries no pack, so it
// writes nothing. Anything after the flush packet is refused.
func readProbe(body io.Reader, raw []byte) (*receiveCommands, *refusal) {
	if _, err := io.ReadFull(body, make([]byte, 1)); !errors.Is(err, io.EOF) {
		return nil, malformedPush("the push names no ref")
	}

	return &receiveCommands{Raw: raw, Refs: []string{}, Capabilities: capability.NewList()}, nil
}

// commandRef returns the ref of a command line `<old> <new> <ref>`, and
// false when the line has any other form or the ref is not a valid ref.
func commandRef(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\n")

	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 || !isObjectID(fields[0]) || !isObjectID(fields[1]) {
		return "", false
	}

	ref := fields[2]
	if !strings.HasPrefix(ref, "refs/") || plumbing.ReferenceName(ref).Validate() != nil {
		return "", false
	}

	return ref, true
}

// isObjectID reports whether value is a SHA-1 object id in git's lowercase
// hex form.
func isObjectID(value string) bool {
	if len(value) != len(plumbing.ZeroHash.String()) {
		return false
	}

	return strings.Trim(value, "0123456789abcdef") == ""
}

func malformedPush(reason string) *refusal {
	return refuse(http.StatusBadRequest, codeInvalidRequest, reason)
}

// boundedRecorder records what is read through it and fails a read that
// would go past limit.
type boundedRecorder struct {
	reader     io.Reader
	limit      int
	recorded   bytes.Buffer
	overflowed bool
}

func (r *boundedRecorder) Read(buffer []byte) (int, error) {
	room := r.limit - r.recorded.Len()
	if room <= 0 {
		r.overflowed = true

		return 0, errCommandSectionTooLarge
	}

	if len(buffer) > room {
		buffer = buffer[:room]
	}

	read, err := r.reader.Read(buffer)
	r.recorded.Write(buffer[:read])

	return read, err
}

// writePushRefusal answers a refused push so git shows the reason: the rest
// of the body is read and discarded, and the report the client asked for
// marks every ref as refused. A client that asked for no report gets a 403
// with the reason as text. reasons maps each refused ref to its reason.
func writePushRefusal(writer http.ResponseWriter, body io.Reader, commands *receiveCommands, reason string, reasons map[string]string) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDiscardedPushBytes))

	capabilities := commands.Capabilities
	if !capabilities.Supports(capability.ReportStatus) && !capabilities.Supports(reportStatusV2) {
		writeRefusal(writer, HostGit, false, refuse(http.StatusForbidden, codeDenied, reason))

		return
	}

	report := packp.NewReportStatus()
	report.UnpackStatus = "ok"

	for _, ref := range commands.Refs {
		why, refused := reasons[ref]
		if !refused {
			why = "push refused (another ref)"
		}

		report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{
			ReferenceName: plumbing.ReferenceName(ref),
			Status:        "githubgate: " + why,
		})
	}

	writer.Header().Set("Content-Type", receivePackResultType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)

	band := sideband.Type(0)

	switch {
	case capabilities.Supports(capability.Sideband64k):
		band = sideband.Sideband64k
	case capabilities.Supports(capability.Sideband):
		band = sideband.Sideband
	}

	if band == 0 {
		_ = report.Encode(writer)

		return
	}

	muxer := sideband.NewMuxer(band, writer)
	_, _ = muxer.WriteChannel(sideband.ProgressMessage, []byte("githubgate: "+reason+"\n"))

	var encoded bytes.Buffer

	_ = report.Encode(&encoded)
	_, _ = muxer.WriteChannel(sideband.PackData, encoded.Bytes())
	_ = pktline.NewEncoder(writer).Flush()
}
