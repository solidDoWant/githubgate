package patterns_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/githubgate/internal/patterns"
)

func TestMatchSegments(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{pattern: "repos/create-release", name: "repos/create-release", want: true},
		{pattern: "repos/create-release", name: "repos/create-releases"},
		{pattern: "repos/*", name: "repos/get", want: true},
		{pattern: "repos/*", name: "repos/"},
		{pattern: "repos/*", name: "repos/a/b"},
		{pattern: "repos/create-*", name: "repos/create-release", want: true},
		{pattern: "repos/create-release*", name: "repos/create-release"},
		{pattern: "*/get", name: "repos/get", want: true},
		{pattern: "create*", name: "createPullRequest", want: true},
		{pattern: "create*", name: "create"},
		{pattern: "mergePullRequest", name: "MergePullRequest"},
		{pattern: "refs/heads/agent/**", name: "refs/heads/agent/x", want: true},
		{pattern: "refs/heads/agent/**", name: "refs/heads/agent/x/y", want: true},
		{pattern: "refs/heads/agent/**", name: "refs/heads/agent"},
		{pattern: "refs/heads/agent/**", name: "refs/heads/agent/"},
		{pattern: "refs/heads/agent/**", name: "refs/heads/agents/x"},
		{pattern: "refs/**/x", name: "refs/heads/a/x", want: true},
		{pattern: "refs/**/x", name: "refs/x"},
		{pattern: "refs/heads/a.b", name: "refs/heads/aXb"},
		{pattern: "refs/tags/v*", name: "refs/tags/v1.0", want: true},
		{pattern: "refs/tags/v*", name: "refs/tags/v1/x"},
	}

	for _, test := range tests {
		t.Run(test.pattern+" "+test.name, func(t *testing.T) {
			assert.Equal(t, test.want, patterns.MatchSegments(test.pattern, test.name))
		})
	}
}

func TestValidRef(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{pattern: "refs/heads/agent/**", want: true},
		{pattern: "refs/heads/main", want: true},
		{pattern: "refs/tags/v*", want: true},
		{pattern: "heads/main"},
		{pattern: "refs/"},
		{pattern: "refs/heads/a..b"},
		{pattern: "refs/heads/a@{1}"},
		{pattern: "refs/heads/a\\b"},
		{pattern: "refs/heads/a b"},
		{pattern: "refs/heads/a\x01"},
		{pattern: "refs/heads/a~1"},
		{pattern: "refs/heads//a"},
		{pattern: "refs/heads/a/"},
		{pattern: "refs/heads/.a"},
		{pattern: "refs/heads/a.lock"},
		{pattern: "refs/heads/a**"},
		{pattern: "refs/heads/***"},
	}

	for _, test := range tests {
		t.Run(test.pattern, func(t *testing.T) {
			assert.Equal(t, test.want, patterns.ValidRef(test.pattern))
		})
	}
}

func TestValidOperationAndMutation(t *testing.T) {
	assert.True(t, patterns.ValidOperation("repos/create-release"))
	assert.True(t, patterns.ValidOperation("repos/*"))
	assert.False(t, patterns.ValidOperation("Repos/get"))
	assert.False(t, patterns.ValidOperation("repos//get"))
	assert.False(t, patterns.ValidOperation("repos/**"))
	assert.False(t, patterns.ValidOperation(""))

	assert.True(t, patterns.ValidMutation("mergePullRequest"))
	assert.True(t, patterns.ValidMutation("create*"))
	assert.False(t, patterns.ValidMutation("merge/PullRequest"))
	assert.False(t, patterns.ValidMutation("a**"))
	assert.False(t, patterns.ValidMutation(""))
}
