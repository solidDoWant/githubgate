package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markdownFiles returns the text of every Markdown page directly under dir.
func markdownFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	require.NoError(t, err)

	pages := map[string]string{}

	for _, path := range paths {
		content, err := os.ReadFile(path)
		require.NoError(t, err)

		pages[path] = string(content)
	}

	return pages
}

func hasHeading(page, pattern string) bool {
	return regexp.MustCompile(`(?mi)^#{1,6} .*` + pattern).MatchString(page)
}

// hasKindBlock reports whether the page has a fenced block with a manifest of kind.
func hasKindBlock(page, kind string) bool {
	blocks := regexp.MustCompile("(?s)```[a-z]*\n(.*?)```").FindAllStringSubmatch(page, -1)
	for _, block := range blocks {
		if regexp.MustCompile(`(?m)^kind:\s*` + kind + `\s*$`).MatchString(block[1]) {
			return true
		}
	}

	return false
}

func anyPage(pages map[string]string, check func(string) bool) bool {
	for _, page := range pages {
		if check(page) {
			return true
		}
	}

	return false
}

func TestUserPageCoversTektonTaskAndCoderWorkspace(t *testing.T) {
	pages := markdownFiles(t, "user")
	assert.True(t, anyPage(pages, func(page string) bool { return hasHeading(page, "tekton") }), "no Tekton heading on a user page")
	assert.True(t, anyPage(pages, func(page string) bool { return hasHeading(page, "coder") }), "no Coder heading on a user page")
	assert.True(t, anyPage(pages, func(page string) bool { return hasHeading(page, "refusal") }), "no refusal heading on a user page")
}

func TestAdminPageCoversAppInstallationCeilingAndPolicy(t *testing.T) {
	pages := markdownFiles(t, "admin")
	for _, kind := range []string{"GitHubApp", "GitHubInstallation", "GitHubAccessPolicy"} {
		assert.True(t, anyPage(pages, func(page string) bool { return hasKindBlock(page, kind) }), "no %s manifest on an admin page", kind)
	}

	assert.True(t, anyPage(pages, func(page string) bool { return hasHeading(page, "ceiling") }), "no ceiling heading on an admin page")
}

func TestUserPageShowsProxyPodSetupAndPush(t *testing.T) {
	pages := markdownFiles(t, "user")
	assert.True(t, anyPage(pages, func(page string) bool {
		return hasKindBlock(page, "Pod") && regexp.MustCompile(`(?m)^\s*hostAliases:`).MatchString(page)
	}), "no Pod manifest with hostAliases on a user page")
	assert.True(t, anyPage(pages, func(page string) bool {
		return regexp.MustCompile("githubgate-client push --proxy").MatchString(page)
	}), "no githubgate-client push example on a user page")
}

func TestAdminPageCoversProxyCAAndProxyRules(t *testing.T) {
	pages := markdownFiles(t, "admin")
	assert.True(t, anyPage(pages, func(page string) bool {
		return hasHeading(page, "name-constrained CA") && regexp.MustCompile(`permitted;DNS:github\.com`).MatchString(page)
	}), "no name-constrained proxy CA section on an admin page")

	for _, field := range []string{"denyOperations", "allowOperations", "denyMutations", "allowMutations", "pushRefs"} {
		assert.True(t, anyPage(pages, func(page string) bool {
			return hasKindBlock(page, "GitHubAccessPolicy") && regexp.MustCompile(field).MatchString(page)
		}), "proxy field %s missing on an admin page", field)
	}
}
