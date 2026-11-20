package intercept

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultJoinInterval is how often a joined file's source is read again.
	DefaultJoinInterval = 30 * time.Second

	joinDir = "join"
	// maxJoinSource bounds the source file, so a joined file always fits the
	// trust volume.
	maxJoinSource = 2 << 20
)

var (
	joinContainerPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	joinVariablePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// Join asks for the file trust/join/<Container>/<Variable>: the file at
// Source followed by the pod CA.
type Join struct {
	Container string
	Variable  string
	Source    string
}

// ParseJoin parses "<container>/<VARIABLE>=<source path>".
func ParseJoin(value string) (Join, error) {
	name, source, found := strings.Cut(value, "=")
	if !found || source == "" {
		return Join{}, errors.New("a join is <container>/<VARIABLE>=<path>")
	}

	container, variable, found := strings.Cut(name, "/")
	if !found || !joinContainerPattern.MatchString(container) || !joinVariablePattern.MatchString(variable) {
		return Join{}, fmt.Errorf("%q is not <container>/<VARIABLE>", name)
	}

	return Join{Container: container, Variable: variable, Source: source}, nil
}

func (j Join) path(trustDir string) string {
	return filepath.Join(trustDir, joinDir, j.Container, j.Variable)
}

// content is the source file followed by the pod CA. A source that is
// missing or unreadable counts as empty, so the pod CA is always there.
func (j Join) content(ca *CA) []byte {
	source, err := readLimited(j.Source)
	if err != nil {
		source = nil
	}

	if len(source) > 0 && !bytes.HasSuffix(source, []byte("\n")) {
		source = append(source, '\n')
	}

	return append(source, ca.PEM...)
}

func readLimited(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) > maxJoinSource {
		return nil, errors.New("the source is too large")
	}

	return data, nil
}

// joiner keeps the joined files current.
type joiner struct {
	trustDir string
	ca       *CA
	joins    []Join
	last     [][]byte
}

func newJoiner(trustDir string, ca *CA, joins []Join) *joiner {
	return &joiner{trustDir: trustDir, ca: ca, joins: joins, last: make([][]byte, len(joins))}
}

// sync writes every joined file whose content changed since the last call.
func (j *joiner) sync() error {
	var errs []error

	for index, join := range j.joins {
		content := join.content(j.ca)
		if bytes.Equal(content, j.last[index]) {
			continue
		}

		path := join.path(j.trustDir)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the workload reads this directory
			errs = append(errs, err)

			continue
		}

		if err := writeFileAtomic(path, content, trustFileMode); err != nil {
			errs = append(errs, err)

			continue
		}

		j.last[index] = content
	}

	return errors.Join(errs...)
}

// run calls sync every interval until ctx ends.
func (j *joiner) run(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.sync(); err != nil {
				logger.Error("joined file not written", "error", err)
			}
		}
	}
}
