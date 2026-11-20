// Command gen-permissions writes the catalogues generated from GitHub's API
// descriptions at the commits pinned below:
//   - internal/permissions/catalogue_gen.go from the REST OpenAPI description
//     (schema components.schemas.app-permissions);
//   - internal/operations/rest_gen.go from the same description's paths;
//   - internal/operations/mutations_gen.go from the Mutation type of
//     octokit/graphql-schema.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// descriptionCommit pins github/rest-api-description; Renovate updates it.
// renovate: depName=github/rest-api-description
const descriptionCommit = "c9c95fe34ced05fe076001f5c55704742bc8e7f8"

// schemaCommit pins octokit/graphql-schema; Renovate updates it.
// renovate: depName=octokit/graphql-schema
const schemaCommit = "82ff2d4780080e6929ebb95608cefa22dfa05ac7"

const schemaURL = "https://raw.githubusercontent.com/octokit/graphql-schema/" + schemaCommit + "/schema.graphql"

const descriptionURL = "https://raw.githubusercontent.com/github/rest-api-description/" + descriptionCommit +
	"/descriptions/api.github.com/api.github.com.json"

// scopes places each permission. A name missing here fails generation, so a
// new GitHub permission is classified on purpose. Enterprise permissions
// apply beyond one repository, so they are grouped with organization ones.
var scopes = map[string][]string{
	"Repository": {
		"actions", "administration", "artifact_metadata", "attestations", "checks", "code_quality",
		"codespaces", "contents", "dependabot_secrets", "deployments", "discussions", "environments",
		"issues", "merge_queues", "metadata", "packages", "pages", "pull_requests",
		"repository_custom_properties", "repository_hooks", "repository_projects",
		"secret_scanning_alerts", "secrets", "security_events", "single_file", "statuses",
		"vulnerability_alerts", "workflows",
	},
	"Organization": {
		"custom_properties_for_organizations", "members", "organization_administration",
		"organization_custom_roles", "organization_custom_org_roles", "organization_custom_properties",
		"organization_copilot_seat_management", "organization_copilot_agent_settings",
		"organization_copilot_requests", "organization_external_properties_for_repos",
		"organization_announcement_banners", "organization_events", "organization_hooks",
		"organization_personal_access_tokens", "organization_personal_access_token_requests",
		"organization_plan", "organization_projects", "organization_packages", "organization_secrets",
		"organization_self_hosted_runners", "organization_user_blocking",
		"enterprise_custom_properties_for_organizations",
	},
	"Account": {
		"email_addresses", "followers", "git_ssh_keys", "gpg_keys", "interaction_limits", "profile", "starring",
	},
}

type description struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-permissions:", err)
		os.Exit(1)
	}
}

func run() error {
	client := &http.Client{Timeout: 2 * time.Minute}

	body, err := fetch(client, descriptionURL)
	if err != nil {
		return err
	}

	var desc description
	if err := json.Unmarshal(body, &desc); err != nil {
		return err
	}

	permissions, err := generate(desc.Components.Schemas["app-permissions"].Properties)
	if err != nil {
		return err
	}

	ids, err := operationIDs(desc.Paths)
	if err != nil {
		return err
	}

	rest, err := generateREST(ids)
	if err != nil {
		return err
	}

	schema, err := fetch(client, schemaURL)
	if err != nil {
		return err
	}

	mutations, err := generateMutations(string(schema))
	if err != nil {
		return err
	}

	for path, src := range map[string][]byte{
		"internal/permissions/catalogue_gen.go": permissions,
		"internal/operations/rest_gen.go":       rest,
		"internal/operations/mutations_gen.go":  mutations,
	} {
		if err := os.WriteFile(path, src, 0o644); err != nil {
			return err
		}
	}

	return nil
}

func fetch(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}

	return io.ReadAll(resp.Body)
}

// operationIDs reads the operation id of every method of every path. Path
// items also hold non-method keys (parameters, summary), which are skipped.
func operationIDs(paths map[string]map[string]json.RawMessage) (map[string]map[string]string, error) {
	ids := map[string]map[string]string{}

	for template, item := range paths {
		for key, raw := range item {
			if _, ok := httpMethods[key]; !ok {
				if key == "head" || key == "options" || key == "trace" {
					return nil, fmt.Errorf("%s %s: method not supported by the operation catalogue", key, template)
				}

				continue
			}

			var op struct {
				OperationID string `json:"operationId"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", key, template, err)
			}

			if ids[template] == nil {
				ids[template] = map[string]string{}
			}

			ids[template][key] = op.OperationID
		}
	}

	return ids, nil
}

func generate(props map[string]struct {
	Enum []string `json:"enum"`
}) ([]byte, error) {
	if len(props) == 0 {
		return nil, fmt.Errorf("schema app-permissions has no properties")
	}

	scopeOf := map[string]string{}

	for scope, names := range scopes {
		for _, name := range names {
			scopeOf[name] = scope
		}
	}

	names := make([]string, 0, len(props))
	for name := range props {
		if _, ok := scopeOf[name]; !ok {
			return nil, fmt.Errorf("permission %q has no scope: add it to scopes", name)
		}

		names = append(names, name)
	}

	sort.Strings(names)

	var buf bytes.Buffer
	buf.WriteString("// Code generated by scripts/gen-permissions from github/rest-api-description at commit " +
		descriptionCommit + ". DO NOT EDIT.\n\npackage permissions\n\n")
	buf.WriteString("var catalogue = map[string]Permission{\n")

	for _, name := range names {
		levels := make([]string, 0, 3)

		for _, level := range props[name].Enum {
			switch level {
			case "read", "write", "admin":
				levels = append(levels, "Level"+strings.ToUpper(level[:1])+level[1:])
			default:
				return nil, fmt.Errorf("permission %q has unknown level %q", name, level)
			}
		}

		if len(levels) == 0 {
			return nil, fmt.Errorf("permission %q has no levels", name)
		}

		fmt.Fprintf(&buf, "\t%q: {Name: %q, Scope: Scope%s, Levels: []Level{%s}},\n",
			name, name, scopeOf[name], strings.Join(levels, ", "))
	}

	buf.WriteString("}\n")

	return format.Source(buf.Bytes())
}
