//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// podReadyTimeout bounds every wait for a workload pod.
const podReadyTimeout = 3 * time.Minute

var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`)

// redactKeyMaterial replaces any private key block in diagnostic output.
func redactKeyMaterial(text string) string {
	return pemBlock.ReplaceAllString(text, "[redacted private key]")
}

// waitPodReady waits for a pod to become Ready. When the wait fails it logs
// what a person needs to see why: the pod description, the namespace events
// and the logs of every container, init containers and sidecars included.
func (c cluster) waitPodReady(t *testing.T, namespace, name string) {
	t.Helper()

	start := time.Now()
	out, err := c.tryKubectl(t.Context(), "", "wait", "--for=condition=Ready", "-n", namespace, "pod/"+name,
		fmt.Sprintf("--timeout=%s", podReadyTimeout))

	recordWait("kubectl wait pod/"+name, time.Since(start))

	if err != nil {
		t.Logf("pod %s/%s was not Ready:\n%s", namespace, name, c.podDiagnostics(namespace, name))
	}

	require.NoError(t, err, "kubectl wait pod/%s: %s", name, out)
}

// podDiagnostics collects the output of several kubectl commands, each
// best effort, with key material redacted. It uses its own context so it
// still runs when the test's context is done.
func (c cluster) podDiagnostics(namespace, name string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 90*time.Second)
	defer cancel()

	var report strings.Builder

	section := func(title string, args ...string) string {
		out, err := c.tryKubectl(ctx, "", args...)
		if err != nil {
			out += "\n(command failed: " + err.Error() + ")"
		}

		fmt.Fprintf(&report, "--- %s ---\n%s\n", title, out)

		return out
	}

	section("kubectl describe pod", "describe", "pod", "-n", namespace, name)
	section("events in namespace "+namespace, "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")

	names := section("container names", "get", "pod", "-n", namespace, name, "-o",
		`jsonpath={range .spec.initContainers[*]}{.name}{"\n"}{end}{range .spec.containers[*]}{.name}{"\n"}{end}`)
	for container := range strings.FieldsSeq(names) {
		if strings.HasPrefix(container, "(command") {
			break
		}

		section("logs of "+container, "logs", "-n", namespace, name, "-c", container, "--tail=200")
		section("previous logs of "+container, "logs", "-n", namespace, name, "-c", container, "--previous", "--tail=200")
	}

	return redactKeyMaterial(report.String())
}
