//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitRecord is one timed setup step or wait.
type waitRecord struct {
	what    string
	seconds float64
}

var (
	waitsMu sync.Mutex
	waits   []waitRecord
)

// recordWait prints an `e2e-wait:` line for a step and keeps it for the
// summary at the end of the run.
func recordWait(what string, elapsed time.Duration) {
	seconds := elapsed.Seconds()

	waitsMu.Lock()

	waits = append(waits, waitRecord{what: what, seconds: seconds})
	waitsMu.Unlock()

	fmt.Fprintf(os.Stderr, "e2e-wait: %s %.1f\n", what, seconds)
}

// timed runs step and records how long it took.
func timed(what string, step func()) {
	start := time.Now()
	defer func() { recordWait(what, time.Since(start)) }()

	step()
}

// longestWaits returns the n longest records, longest first.
func longestWaits(records []waitRecord, n int) []waitRecord {
	sorted := append([]waitRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].seconds > sorted[j].seconds })

	if len(sorted) > n {
		sorted = sorted[:n]
	}

	return sorted
}

// commandLabel names a command by its leading words, up to the first flag and
// at most three, plus the first `kind/name` resource argument, so that no
// other value passed to a tool lands in the log.
func commandLabel(name string, args []string) string {
	words := []string{name}
	leading := true

	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			leading = false
		}

		switch {
		case leading && len(words) <= 3:
			words = append(words, arg)
		case !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") && strings.Contains(arg, "/") && !strings.Contains(arg, "="):
			return strings.Join(append(words, arg), " ")
		}
	}

	return strings.Join(words, " ")
}

// helmLabel names a helm call by its verb and release, for the form
// `helm upgrade --install release ...`.
func helmLabel(args []string) string {
	if len(args) >= 3 && args[1] == "--install" {
		return commandLabel("helm", []string{args[0], args[2]})
	}

	return commandLabel("helm", args)
}

// eventually is require.Eventually that records how long the poll took.
func eventually(t *testing.T, what string, condition func() bool, waitFor, tick time.Duration) {
	t.Helper()

	timed("eventually "+what, func() {
		t.Helper()
		require.Eventually(t, condition, waitFor, tick)
	})
}

func TestMain(m *testing.M) {
	code := m.Run()

	waitsMu.Lock()
	defer waitsMu.Unlock()

	if len(waits) > 0 {
		fmt.Fprintln(os.Stderr, "e2e-wait: the ten longest:")

		for _, record := range longestWaits(waits, 10) {
			fmt.Fprintf(os.Stderr, "e2e-wait: %s %.1f\n", record.what, record.seconds)
		}
	}

	os.Exit(code)
}
