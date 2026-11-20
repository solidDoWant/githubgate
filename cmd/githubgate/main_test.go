package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunRejectsUnknownFlags(t *testing.T) {
	require.Error(t, run([]string{"--no-such-flag"}))
}
