package docs

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

var requiredOperations = map[string]string{
	"GET /health":                         "operations",
	"GET /ready":                          "operations",
	"POST /priv/notifications/send":       "private",
	"GET /priv/notifications/{messageId}": "private",
}

type operationMetadata struct {
	tags       []string
	visibility []string
	callers    int
}

func TestOpenAPIContract(t *testing.T) {
	document, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if errors := validateOpenAPI(string(document)); len(errors) != 0 {
		t.Fatalf("invalid OpenAPI contract:\n- %s", strings.Join(errors, "\n- "))
	}
}

func TestOpenAPIRejectsInvalidCatalogContracts(t *testing.T) {
	const valid = `openapi: 3.1.0
x-hhc-service: notification-api
x-hhc-owner: HHC Platform
x-hhc-repository: HallelujahHomeChurch/notification-api
paths:
  /health:
    get:
      tags: [Operations]
      x-hhc-visibility: operations
      x-hhc-callers: []
  /ready:
    get:
      tags: [Operations]
      x-hhc-visibility: operations
      x-hhc-callers: []
  /priv/notifications/send:
    post:
      tags: [Private]
      x-hhc-visibility: private
      x-hhc-callers: [account-api, engagement-api]
  /priv/notifications/{messageId}:
    get:
      tags: [Private]
      x-hhc-visibility: private
      x-hhc-callers: [account-api, engagement-api]
`
	tests := []struct {
		name      string
		document  string
		wantError string
	}{
		{"non-3.1", strings.Replace(valid, "3.1.0", "3.0.3", 1), "OpenAPI 3.1"},
		{"missing visibility", strings.Replace(valid, "      x-hhc-visibility: private\n", "", 1), "exactly one x-hhc-visibility"},
		{"unknown visibility", strings.Replace(valid, "x-hhc-visibility: private", "x-hhc-visibility: internal", 1), "unknown visibility"},
		{"mismatched visibility", strings.Replace(valid, "x-hhc-visibility: private", "x-hhc-visibility: public", 1), "does not match tag"},
		{"missing callers", strings.Replace(valid, "      x-hhc-callers: [account-api, engagement-api]\n", "", 1), "exactly one x-hhc-callers"},
		{"missing service", strings.Replace(valid, "x-hhc-service: notification-api\n", "", 1), "x-hhc-service"},
		{"missing owner", strings.Replace(valid, "x-hhc-owner: HHC Platform\n", "", 1), "x-hhc-owner"},
		{"missing repository", strings.Replace(valid, "x-hhc-repository: HallelujahHomeChurch/notification-api\n", "", 1), "x-hhc-repository"},
		{"missing critical route", strings.Replace(valid, "/priv/notifications/send", "/priv/notifications/dispatch", 1), "missing POST /priv/notifications/send"},
		{"unexpected documented route", valid + "  /metrics:\n    get:\n      tags: [Operations]\n      x-hhc-visibility: operations\n      x-hhc-callers: []\n", "unexpected GET /metrics"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errors := validateOpenAPI(test.document)
			if !slices.ContainsFunc(errors, func(err string) bool { return strings.Contains(err, test.wantError) }) {
				t.Fatalf("errors = %q, want one containing %q", errors, test.wantError)
			}
		})
	}
}

func validateOpenAPI(document string) []string {
	var errors []string
	for key, want := range map[string]string{
		"openapi":          "3.1.",
		"x-hhc-service":    "notification-api",
		"x-hhc-owner":      "HHC Platform",
		"x-hhc-repository": "HallelujahHomeChurch/notification-api",
	} {
		got := rootValue(document, key)
		if key == "openapi" {
			if !strings.HasPrefix(got, want) {
				errors = append(errors, "openapi must use OpenAPI 3.1")
			}
		} else if got != want {
			errors = append(errors, fmt.Sprintf("%s = %q, want %q", key, got, want))
		}
	}

	operations := parseOperations(document)
	for operation, visibility := range requiredOperations {
		metadata, ok := operations[operation]
		if !ok {
			errors = append(errors, "missing "+operation)
			continue
		}
		if len(metadata.tags) != 1 {
			errors = append(errors, operation+" must have exactly one visibility tag")
		}
		if len(metadata.visibility) != 1 {
			errors = append(errors, operation+" must have exactly one x-hhc-visibility")
		} else if !slices.Contains([]string{"public", "admin", "private", "operations"}, metadata.visibility[0]) {
			errors = append(errors, operation+" has unknown visibility "+metadata.visibility[0])
		} else if metadata.visibility[0] != visibility || len(metadata.tags) != 1 || strings.ToLower(metadata.tags[0]) != visibility {
			errors = append(errors, operation+" visibility does not match tag")
		}
		if metadata.callers != 1 {
			errors = append(errors, operation+" must have exactly one x-hhc-callers")
		}
	}
	for operation := range operations {
		if _, ok := requiredOperations[operation]; !ok {
			errors = append(errors, "unexpected "+operation)
		}
	}
	return errors
}

func rootValue(document, key string) string {
	prefix := key + ":"
	for line := range strings.SplitSeq(document, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func parseOperations(document string) map[string]operationMetadata {
	operations := make(map[string]operationMetadata)
	var path, operation string
	scanner := bufio.NewScanner(strings.NewReader(document))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
			operation = ""
		case path != "" && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") && strings.HasSuffix(line, ":"):
			method := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if slices.Contains([]string{"get", "post", "put", "patch", "delete", "head", "options"}, method) {
				operation = strings.ToUpper(method) + " " + path
				operations[operation] = operationMetadata{}
			}
		case operation != "" && strings.HasPrefix(line, "      tags:"):
			metadata := operations[operation]
			metadata.tags = append(metadata.tags, inlineList(line)...)
			operations[operation] = metadata
		case operation != "" && strings.HasPrefix(line, "      x-hhc-visibility:"):
			metadata := operations[operation]
			metadata.visibility = append(metadata.visibility, strings.TrimSpace(strings.TrimPrefix(line, "      x-hhc-visibility:")))
			operations[operation] = metadata
		case operation != "" && strings.HasPrefix(line, "      x-hhc-callers:"):
			metadata := operations[operation]
			metadata.callers++
			operations[operation] = metadata
		}
	}
	return operations
}

func inlineList(line string) []string {
	start, end := strings.IndexByte(line, '['), strings.LastIndexByte(line, ']')
	if start < 0 || end <= start+1 {
		return nil
	}
	items := strings.Split(line[start+1:end], ",")
	for index := range items {
		items[index] = strings.TrimSpace(items[index])
	}
	return items
}
