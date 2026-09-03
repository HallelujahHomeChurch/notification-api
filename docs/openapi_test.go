package docs

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

var requiredOperations = map[string]struct {
	visibility string
	callers    []string
}{
	"GET /health":                         {visibility: "operations"},
	"GET /ready":                          {visibility: "operations"},
	"POST /priv/notifications/send":       {visibility: "private", callers: []string{"account-api", "engagement-api"}},
	"GET /priv/notifications/{messageId}": {visibility: "private", callers: []string{"account-api", "engagement-api"}},
	"POST /priv/dsr/exports":              {visibility: "private", callers: []string{"account-api"}},
	"POST /priv/dsr/actions":              {visibility: "private", callers: []string{"account-api"}},
}

type operationMetadata struct {
	tags         []string
	visibility   []string
	callerFields int
	callers      []string
	callersArray bool
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

func TestOpenAPITagDefinitionsMatchOperations(t *testing.T) {
	document, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Operations", "Private"}
	if got := topLevelTagNames(string(document)); !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level tags = %v, want %v", got, want)
	}
	if got := operationTagNames(string(document)); !reflect.DeepEqual(got, want) {
		t.Fatalf("operation tags = %v, want %v", got, want)
	}
}

func topLevelTagNames(document string) []string {
	head, _, _ := strings.Cut(document, "paths:\n")
	var tags []string
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "  - name: ") {
			tags = append(tags, strings.TrimPrefix(line, "  - name: "))
		}
	}
	sort.Strings(tags)
	return tags
}

func operationTagNames(document string) []string {
	tags := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^      tags: \[([^]]+)\]$`).FindAllStringSubmatch(document, -1) {
		tags[match[1]] = true
	}
	result := make([]string, 0, len(tags))
	for tag := range tags {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
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
		{"scalar callers", strings.Replace(valid, "x-hhc-callers: [account-api, engagement-api]", "x-hhc-callers: account-api", 1), "must be an array"},
		{"wrong callers", strings.Replace(valid, "[account-api, engagement-api]", "[hhc-web-api]", 1), "callers ="},
		{"wrong block callers", strings.Replace(valid, "      x-hhc-callers: [account-api, engagement-api]\n", "      x-hhc-callers:\n        - hhc-web-api\n", 1), "callers ="},
		{"missing service", strings.Replace(valid, "x-hhc-service: notification-api\n", "", 1), "x-hhc-service"},
		{"missing owner", strings.Replace(valid, "x-hhc-owner: HHC Platform\n", "", 1), "x-hhc-owner"},
		{"missing repository", strings.Replace(valid, "x-hhc-repository: HallelujahHomeChurch/notification-api\n", "", 1), "x-hhc-repository"},
		{"missing critical route", strings.Replace(valid, "/priv/notifications/send", "/priv/notifications/dispatch", 1), "missing POST /priv/notifications/send"},
		{"unexpected documented route", valid + "  /metrics:\n    get:\n      tags: [Operations]\n      x-hhc-visibility: operations\n      x-hhc-callers: []\n", "unexpected GET /metrics"},
		{"unexpected trace operation", valid + "  /metrics:\n    trace:\n      tags: [Operations]\n      x-hhc-visibility: operations\n      x-hhc-callers: []\n", "unexpected TRACE /metrics"},
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

func TestOpenAPIMatchesImplementedRuntimeSemantics(t *testing.T) {
	contents, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(contents)

	if strings.Contains(yamlBlock(document, "info:"), "\n  license:") {
		t.Error("info must not assert an unsupported repository license")
	}
	requireContains(t, yamlBlock(document, "    IdempotencyKey:"), "retained until 730 days after the notification reaches a terminal state; non-terminal notifications retain it longer")
	if strings.Contains(document, "'405':") {
		t.Error("documented operations must not advertise unreachable 405 responses")
	}

	sendRequest := yamlBlock(document, "    SendRequest:")
	for _, want := range []string{
		"required: [templateId, channel, target, payload, resource]",
		"Unsupported or omitted locales fall back to en",
		"channel: { const: email }",
		"target: { $ref: '#/components/schemas/EmailTarget' }",
		"channel: { const: web_push }",
		"target: { $ref: '#/components/schemas/WebPushTarget' }",
	} {
		requireContains(t, sendRequest, want)
	}
	if strings.Contains(sendRequest, "enum: [zh-Hant, zh-Hans, en, ja, ko]") {
		t.Error("current SendRequest must not advertise dormant v3 locales")
	}

	for _, check := range []struct {
		schema string
		wants  []string
	}{
		{"    EmailTarget:", []string{"type: { const: email }", "format: email", "minLength: 1"}},
		{"    WebPushTarget:", []string{"type: { const: web_push }", "minLength: 1", "strict JSON object", "HTTPS endpoint", "p256dh", "auth", "unknown fields"}},
	} {
		block := yamlBlock(document, check.schema)
		for _, want := range check.wants {
			requireContains(t, block, want)
		}
	}
	resource := yamlBlock(document, "    Resource:")
	if count := strings.Count(resource, "pattern: '.*\\S.*'"); count != 2 {
		t.Errorf("Resource non-blank patterns = %d, want 2", count)
	}

	for _, check := range []struct {
		path, status, schema string
	}{
		{"/ready", "503", "NotReadyErrorResponse"},
		{"/priv/notifications/send", "400", "InvalidRequestErrorResponse"},
		{"/priv/notifications/send", "401", "UnauthorizedErrorResponse"},
		{"/priv/notifications/send", "403", "ForbiddenErrorResponse"},
		{"/priv/notifications/send", "409", "IdempotencyConflictErrorResponse"},
		{"/priv/notifications/send", "429", "RateLimitedErrorResponse"},
		{"/priv/notifications/send", "500", "InternalErrorResponse"},
		{"/priv/notifications/send", "503", "DisabledErrorResponse"},
		{"/priv/notifications/{messageId}", "401", "UnauthorizedErrorResponse"},
		{"/priv/notifications/{messageId}", "403", "ForbiddenErrorResponse"},
		{"/priv/notifications/{messageId}", "404", "NotFoundErrorResponse"},
		{"/priv/notifications/{messageId}", "500", "InternalErrorResponse"},
	} {
		got := responseSchema(document, check.path, check.status)
		want := "#/components/schemas/" + check.schema
		if got != want {
			t.Errorf("%s %s schema = %q, want %q", check.path, check.status, got, want)
		}
	}
	for schema, code := range map[string]string{
		"InvalidRequestErrorResponse":      "NTF_INVALID_REQUEST",
		"UnauthorizedErrorResponse":        "NTF_UNAUTHORIZED",
		"ForbiddenErrorResponse":           "NTF_FORBIDDEN",
		"IdempotencyConflictErrorResponse": "NTF_IDEMPOTENCY_CONFLICT",
		"RateLimitedErrorResponse":         "NTF_RATE_LIMITED",
		"NotFoundErrorResponse":            "NTF_NOT_FOUND",
		"NotReadyErrorResponse":            "NTF_NOT_READY",
		"DisabledErrorResponse":            "NTF_DISABLED",
		"InternalErrorResponse":            "NTF_INTERNAL",
	} {
		requireContains(t, yamlBlock(document, "    "+schema+":"), "const: "+code)
	}
}

func TestOpenAPIDSRContractsExposeOnlyRedactedNotificationMetadata(t *testing.T) {
	contents, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(contents)
	for _, route := range []string{"/priv/dsr/exports", "/priv/dsr/actions"} {
		block := yamlBlock(document, "  "+route+":")
		requireContains(t, block, "x-hhc-callers: [account-api]")
	}
	exportRequest := yamlBlock(document, "    DSRExportRequest:")
	requireContains(t, exportRequest, "required: [requestId, userId, canonicalEmail]")
	actionRequest := yamlBlock(document, "    DSRActionRequest:")
	requireContains(t, actionRequest, "required: [requestId, userId, canonicalEmail, action, idempotencyKey]")
	actionResult := yamlBlock(document, "    DSRActionResult:")
	requireContains(t, actionResult, "action: { const: restrict_processing }")
	requireContains(t, actionResult, "status: { const: not_applicable }")
	requireContains(t, actionResult, "action: { const: erase }")
	requireContains(t, actionResult, "status: { const: completed }")
	record := yamlBlock(document, "    DSRExportRecord:")
	for _, forbidden := range []string{"ciphertext", "provider", "endpoint", "payload"} {
		if strings.Contains(strings.ToLower(record), forbidden) {
			t.Fatalf("DSR export record exposes %q: %s", forbidden, record)
		}
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
	for operation, requirement := range requiredOperations {
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
		} else if metadata.visibility[0] != requirement.visibility || len(metadata.tags) != 1 || strings.ToLower(metadata.tags[0]) != requirement.visibility {
			errors = append(errors, operation+" visibility does not match tag")
		}
		if metadata.callerFields != 1 {
			errors = append(errors, operation+" must have exactly one x-hhc-callers")
		} else if !metadata.callersArray {
			errors = append(errors, operation+" x-hhc-callers must be an array")
		} else if !slices.Equal(metadata.callers, requirement.callers) {
			errors = append(errors, fmt.Sprintf("%s callers = %q, want %q", operation, metadata.callers, requirement.callers))
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
	var path, operation, callerOperation string
	scanner := bufio.NewScanner(strings.NewReader(document))
	for scanner.Scan() {
		line := scanner.Text()
		if callerOperation != "" {
			if strings.HasPrefix(line, "        - ") {
				metadata := operations[callerOperation]
				metadata.callers = append(metadata.callers, strings.TrimSpace(strings.TrimPrefix(line, "        - ")))
				operations[callerOperation] = metadata
				continue
			}
			if strings.TrimSpace(line) != "" {
				callerOperation = ""
			}
		}
		switch {
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
			operation = ""
		case path != "" && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") && strings.HasSuffix(line, ":"):
			method := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if slices.Contains([]string{"get", "post", "put", "patch", "delete", "head", "options", "trace"}, method) {
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
			metadata.callerFields++
			value := strings.TrimSpace(strings.TrimPrefix(line, "      x-hhc-callers:"))
			if value == "" {
				metadata.callersArray = true
				callerOperation = operation
			} else {
				metadata.callers, metadata.callersArray = inlineListValue(line)
			}
			operations[operation] = metadata
		}
	}
	return operations
}

func inlineList(line string) []string {
	items, _ := inlineListValue(line)
	return items
}

func inlineListValue(line string) ([]string, bool) {
	start, end := strings.IndexByte(line, '['), strings.LastIndexByte(line, ']')
	if start < 0 || end < start {
		return nil, false
	}
	if end == start+1 {
		return nil, true
	}
	items := strings.Split(line[start+1:end], ",")
	for index := range items {
		items[index] = strings.TrimSpace(items[index])
	}
	return items, true
}

func yamlBlock(document, marker string) string {
	start := strings.Index(document, marker)
	if start < 0 {
		return ""
	}
	indent := len(marker) - len(strings.TrimLeft(marker, " "))
	end := len(document)
	offset := start + len(marker)
	for line := range strings.SplitSeq(document[offset:], "\n") {
		offset += len(line) + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " "))
		if lineIndent <= indent {
			end = offset - len(line) - 1
			break
		}
	}
	return document[start:end]
}

func responseSchema(document, path, status string) string {
	response := yamlBlock(yamlBlock(document, "  "+path+":"), "        '"+status+"':")
	const prefix = "$ref: '"
	start := strings.Index(response, prefix)
	if start < 0 {
		return ""
	}
	value := response[start+len(prefix):]
	end := strings.IndexByte(value, '\'')
	if end < 0 {
		return ""
	}
	return value[:end]
}

func requireContains(t *testing.T, document, want string) {
	t.Helper()
	if !strings.Contains(document, want) {
		t.Errorf("OpenAPI block missing %q", want)
	}
}
