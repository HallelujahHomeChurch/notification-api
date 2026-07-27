package migrations

import (
	"strings"
	"testing"
)

func TestInitialSchemaContainsLedgerConstraints(t *testing.T) {
	contents, err := files.ReadFile("sql/001_initial.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	schema := strings.ToLower(string(contents))

	for _, want := range []string{
		"create table notification_messages",
		"unique (caller_app_id, idempotency_key)",
		"create table notification_deliveries",
		"references notification_messages",
		"create table notification_outbox",
		"references notification_deliveries",
		"create table notification_rate_limits",
		"bucket_key text primary key",
		"create index",
		"where status = 'queued'",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("initial schema missing %q", want)
		}
	}
	for _, forbidden := range []string{"recipient", "email_address", "phone_number"} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("initial schema contains plaintext target field %q", forbidden)
		}
	}
}

func TestMigrationChecksumIsDeterministic(t *testing.T) {
	first := checksum([]byte("create table example();"))
	second := checksum([]byte("create table example();"))
	if first != second {
		t.Fatal("checksum() is not deterministic")
	}
	if first == checksum([]byte("create table changed();")) {
		t.Fatal("checksum() did not change with migration contents")
	}
}
