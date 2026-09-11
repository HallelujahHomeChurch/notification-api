package governance

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

var excludedColumns = map[string]map[string]string{
	"schema_migrations": {
		"version":    "Migration filename, not an Account subject.",
		"checksum":   "Migration content checksum, not an Account subject.",
		"applied_at": "Schema deployment time, not an Account event.",
	},
}

const dsrUserIDBoundary = "DSR userId is validation/correlation only and is not queried; current DSR lookup uses retained key IDs and email-derived hashes."

func notificationManifest(t *testing.T) map[string]any {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(root, "docs/data-governance.yaml"))
	require.NoError(t, err)
	document, err := validateManifest(root, raw, manifestService)
	require.NoError(t, err)
	return document
}

func TestDataGovernanceManifest(t *testing.T) {
	document := notificationManifest(t)
	datasets := map[string]map[string]any{}
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		datasets[dataset["id"].(string)] = dataset
	}
	require.ElementsMatch(t, []string{
		"notification.message-sensitive",
		"notification.message-metadata",
		"notification.delivery-receipts",
		"notification.outbox",
		"notification.rate-limit-buckets",
	}, mapKeys(datasets))

	exactPhysicalFields := map[string][]string{
		"notification.message-sensitive": {"target_ciphertext", "payload_ciphertext"},
		"notification.message-metadata": {
			"id", "caller_app_id", "idempotency_key", "request_hash", "template_id", "template_version",
			"channel", "target_type", "target_hash", "resource_type", "resource_id", "eligibility_campaign_id", "eligibility_recipient_id", "status", "created_at",
			"updated_at", "terminal_at", "payload_purged_at", "encryption_key_id", "hash_key_id", "expires_at",
		},
		"notification.delivery-receipts": {
			"id", "message_id", "channel", "endpoint_ref", "provider", "status", "attempt_count",
			"next_attempt_at", "lease_expires_at", "sent_at", "provider_message_id", "last_error_code",
			"created_at", "updated_at",
		},
		"notification.outbox": {
			"id", "delivery_id", "status", "attempt_count", "next_attempt_at", "lease_expires_at",
			"published_at", "created_at", "updated_at",
		},
		"notification.rate-limit-buckets": {"bucket_key", "count", "expires_at"},
	}
	for id, want := range exactPhysicalFields {
		physical, nested := partitionFields(datasets[id])
		require.ElementsMatch(t, want, physical, id)
		if id != "notification.message-sensitive" {
			require.Empty(t, nested, "%s must not contain nested encrypted fields", id)
		}
	}

	_, sensitiveNested := partitionFields(datasets["notification.message-sensitive"])
	require.ElementsMatch(t, []string{
		"target_ciphertext.email.address",
		"target_ciphertext.web_push.endpoint",
		"target_ciphertext.web_push.keys.p256dh",
		"target_ciphertext.web_push.keys.auth",
		"payload_ciphertext.locale",
		"payload_ciphertext.fields.verifyUrl",
		"payload_ciphertext.fields.resetUrl",
		"payload_ciphertext.fields.confirmUrl",
		"payload_ciphertext.fields.provider",
		"payload_ciphertext.fields.code",
		"payload_ciphertext.fields.subject",
		"payload_ciphertext.fields.body",
		"payload_ciphertext.fields.actionUrl",
		"payload_ciphertext.fields.unsubscribeUrl",
		"payload_ciphertext.fields.oneClickUnsubscribeUrl",
		"payload_ciphertext.fields.title",
		"payload_ciphertext.fields.clickBehavior",
	}, sensitiveNested)

	sensitiveRetention := datasets["notification.message-sensitive"]["retention"].(map[string]any)
	require.Equal(t, "delete", sensitiveRetention["action"])
	description := sensitiveRetention["rule"].(map[string]any)["description"].(string)
	for _, qualification := range []string{
		"target_ciphertext", "payload_ciphertext", "not complete de-identification",
		"HMAC", "resource", "receipt", "external/provider copies",
	} {
		require.Contains(t, description, qualification)
	}

	refs := sourceReferences(document)
	for _, ref := range []string{
		"internal/service/service.go#Service.Send",
		"internal/store/store.go#Store.Create",
		"internal/retention/worker.go#postgresRepository.retain",
		"internal/dsr/service.go#Service.Apply",
		"internal/httpapi/handler.go#handler.authorizeCaller",
		"internal/worker/worker.go#postgresStore.markSent",
		"internal/providers/provider.go#ProviderReceipt",
		"internal/outbox/dispatcher.go#Dispatcher.DispatchOne",
		"internal/queue/servicebus.go#ServiceBus.Publish",
	} {
		require.Contains(t, refs, ref)
	}

	scope, err := os.ReadFile("../../docs/data-governance-scope.md")
	require.NoError(t, err)
	var wantExclusions []string
	for table, columns := range excludedColumns {
		for column, reason := range columns {
			wantExclusions = append(wantExclusions, fmt.Sprintf("- `%s.%s` — %s", table, column, reason))
		}
	}
	require.ElementsMatch(t, wantExclusions, scopeSectionLines(t, string(scope), "Operational schema exclusions"))
	require.Contains(t, string(scope), dsrUserIDBoundary)
	require.Contains(t, datasets["notification.message-metadata"]["attribution"].(map[string]any)["explanation"], dsrUserIDBoundary)

	metadataFields := datasets["notification.message-metadata"]["fields"].([]any)
	datasets["notification.message-metadata"]["fields"] = append(metadataFields, map[string]any{
		"name": "target_ciphertext", "purpose": "Invalid duplicate.", "necessity": "required", "data_classes": []any{"security"},
	})
	_, err = manifestFields(document)
	require.ErrorContains(t, err, "duplicate classified column notification_messages.target_ciphertext")
}

func TestDataGovernanceMigratedColumnCoverage(t *testing.T) {
	document := notificationManifest(t)
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(strings.TrimPrefix(parsed.Path, "/")), "test")

	admin, err := sql.Open("pgx", rawURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	schema := "governance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrations.Run(context.Background(), db))

	rows, err := db.Query(`
		SELECT table_name,column_name
		FROM information_schema.columns
		WHERE table_schema=$1
		ORDER BY table_name,ordinal_position`, schema)
	require.NoError(t, err)
	defer rows.Close()
	actual := map[string][]string{}
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		actual[table] = append(actual[table], column)
	}
	require.NoError(t, rows.Err())

	fields, err := manifestFields(document)
	require.NoError(t, err)
	var tables []string
	for table := range actual {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			require.NoError(t, classifiedColumns(actual[table], fields[table], excludedColumns[table]))
		})
	}
	for table := range fields {
		require.Contains(t, actual, table, "manifest references unknown table")
	}
	for table := range excludedColumns {
		require.Contains(t, actual, table, "exclusion references unknown table")
	}

	require.ErrorContains(t, classifiedColumns(append(actual["notification_messages"], "unclassified_fixture"), fields["notification_messages"], nil), "unclassified column unclassified_fixture")
}

func manifestFields(document map[string]any) (map[string]map[string]bool, error) {
	resources := map[string]map[string]bool{}
	owners := map[string]map[string]string{}
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		storage := dataset["storage"].(map[string]any)
		if storage["kind"] != "postgres" {
			continue
		}
		resource := storage["resource"].(string)
		if resources[resource] == nil {
			resources[resource] = map[string]bool{}
			owners[resource] = map[string]string{}
		}
		for _, field := range datasetFieldNames(dataset) {
			if !strings.Contains(field, ".") {
				if owner := owners[resource][field]; owner != "" {
					return nil, fmt.Errorf("duplicate classified column %s.%s in %s and %s", resource, field, owner, dataset["id"])
				}
				resources[resource][field] = true
				owners[resource][field] = dataset["id"].(string)
			}
		}
	}
	return resources, nil
}

func classifiedColumns(actual []string, fields map[string]bool, excluded map[string]string) error {
	columns := map[string]bool{}
	for _, column := range actual {
		columns[column] = true
		if !fields[column] && strings.TrimSpace(excluded[column]) == "" {
			return fmt.Errorf("unclassified column %s", column)
		}
		if fields[column] && excluded[column] != "" {
			return fmt.Errorf("ambiguous column %s", column)
		}
	}
	for field := range fields {
		if !columns[field] {
			return fmt.Errorf("unknown classified column %s", field)
		}
	}
	for field, reason := range excluded {
		if !columns[field] || strings.TrimSpace(reason) == "" {
			return fmt.Errorf("invalid exclusion %s", field)
		}
	}
	return nil
}

func datasetFieldNames(dataset map[string]any) []string {
	fields := make([]string, 0, len(dataset["fields"].([]any)))
	for _, value := range dataset["fields"].([]any) {
		fields = append(fields, value.(map[string]any)["name"].(string))
	}
	return fields
}

func partitionFields(dataset map[string]any) (physical, nested []string) {
	for _, field := range datasetFieldNames(dataset) {
		if strings.Contains(field, ".") {
			nested = append(nested, field)
		} else {
			physical = append(physical, field)
		}
	}
	return physical, nested
}

func scopeSectionLines(t *testing.T, document, heading string) []string {
	t.Helper()
	_, section, found := strings.Cut(document, "## "+heading+"\n")
	require.True(t, found, "missing scope section %q", heading)
	section, _, _ = strings.Cut(section, "\n## ")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(section), "\n") {
		if strings.HasPrefix(line, "- ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func sourceReferences(document any) map[string]bool {
	refs := map[string]bool{}
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			path, pathOK := node["path"].(string)
			symbol, symbolOK := node["symbol"].(string)
			if pathOK && symbolOK {
				refs[path+"#"+symbol] = true
			}
			for _, child := range node {
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(document)
	return refs
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
