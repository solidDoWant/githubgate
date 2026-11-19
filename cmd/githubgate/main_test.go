package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunPrintsTheBuildVersion(t *testing.T) {
	original := version
	version = "1.2.3"

	t.Cleanup(func() { version = original })

	var out bytes.Buffer
	require.NoError(t, run(&out))
	require.Equal(t, "githubgate 1.2.3\n", out.String())
}
