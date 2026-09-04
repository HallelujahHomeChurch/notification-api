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

	sensitiveFields := datasetFieldNames(datasets["notification.message-sensitive"])
	for _, field := range []string{
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
	} {
		require.Contains(t, sensitiveFields, field)
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
	for table, columns := range excludedColumns {
		for column := range columns {
			require.Contains(t, string(scope), table+"."+column)
		}
	}
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

	fields := manifestFields(document)
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

func manifestFields(document map[string]any) map[string]map[string]bool {
	resources := map[string]map[string]bool{}
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		storage := dataset["storage"].(map[string]any)
		if storage["kind"] != "postgres" {
			continue
		}
		resource := storage["resource"].(string)
		if resources[resource] == nil {
			resources[resource] = map[string]bool{}
		}
		for field := range datasetFieldNames(dataset) {
			if !strings.Contains(field, ".") {
				resources[resource][field] = true
			}
		}
	}
	return resources
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

func datasetFieldNames(dataset map[string]any) map[string]bool {
	fields := map[string]bool{}
	for _, value := range dataset["fields"].([]any) {
		fields[value.(map[string]any)["name"].(string)] = true
	}
	return fields
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
