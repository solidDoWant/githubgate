//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLongestWaitsSortsAndCaps(t *testing.T) {
	records := []waitRecord{{"a", 1}, {"b", 5}, {"c", 3}, {"d", 4}}
	got := longestWaits(records, 3)
	assert.Equal(t, []waitRecord{{"b", 5}, {"d", 4}, {"c", 3}}, got)
	assert.Equal(t, "a", records[0].what, "input is not reordered")
}

func TestCommandLabelDropsFlagsAndValues(t *testing.T) {
	assert.Equal(t, "helm upgrade", commandLabel("helm", []string{"upgrade", "--install", "cert-manager", "--set", "k=secret"}))
	assert.Equal(t, "kubectl wait pod/a", commandLabel("kubectl", []string{"wait", "--for=condition=Ready", "-n", "ns", "pod/a"}))
	assert.Equal(t, "kubectl get pods x", commandLabel("kubectl", []string{"get", "pods", "x", "y", "z"}))
}

func TestHelmLabelNamesTheRelease(t *testing.T) {
	assert.Equal(t, "helm upgrade cert-manager", helmLabel([]string{"upgrade", "--install", "cert-manager", "oci://x", "--set", "k=v"}))
}
