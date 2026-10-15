// Temporary A4 owner-local metadata inventory; remove after A5 acceptance.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	databaseHost = "hhc-pg.postgres.database.azure.com"
	databaseName = "notification"
	maxRows      = 10000
	maxBytes     = 4 << 20
)

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "DSR inventory failed; no completed inventory is available")
		os.Exit(1)
	}
}

func parseIDs(args []string) ([]string, error) {
	if len(args) == 0 || len(args) > maxRows {
		return nil, errors.New("explicit bounded message IDs required")
	}
	seen := map[string]bool{}
	for _, arg := range args {
		id, err := uuid.Parse(arg)
		if err != nil || id == uuid.Nil || len(arg) != 36 {
			return nil, errors.New("invalid message ID")
		}
		seen[id.String()] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func databaseConfig(dsn string) (*pgx.ConnConfig, error) {
	if dsn == "" {
		return nil, errors.New("resident database required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != databaseHost || cfg.Port != 5432 || cfg.Database != databaseName || cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 {
		return nil, errors.New("exact TLS database required")
	}
	// Always use system roots and hostname verification, including resident sslmode=require.
	cfg.TLSConfig = &tls.Config{ServerName: databaseHost, MinVersion: tls.VersionTLS12}
	cfg.ConnectTimeout = 10 * time.Second
	return cfg, nil
}

func execute(ctx context.Context, args []string, out io.Writer) error {
	ids, err := parseIDs(args)
	if err != nil {
		return err
	}
	cfg, err := databaseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	db.SetMaxOpenConns(1)
	data, err := inventory(ctx, db, databaseName, ids)
	if err != nil {
		return err
	}
	return writeFrame(out, data)
}

func writeFrame(out io.Writer, data []byte) error {
	if len(data) > maxBytes || !json.Valid(data) {
		return errors.New("inventory encoding or bound failure")
	}
	_, err := fmt.Fprintf(out, "DSR_INVENTORY_V1_BEGIN\n%s\nSHA256 %x\nDSR_INVENTORY_V1_COMPLETE\n", data, sha256.Sum256(data))
	return err
}

type snapshot struct {
	Database         string                       `json:"database"`
	ObservationStart time.Time                    `json:"observationStart"`
	ObservationEnd   time.Time                    `json:"observationEnd"`
	RequestedIDs     []string                     `json:"requestedMessageIds"`
	Sections         map[string][]json.RawMessage `json:"sections"`
	Complete         bool                         `json:"complete"`
}

type query struct {
	name, sql string
	args      []any
}

func beginSnapshot(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, errors.New("snapshot unavailable")
	}
	for _, statement := range []string{`SET LOCAL statement_timeout='15s'`, `SET LOCAL lock_timeout='2s'`, `SET LOCAL idle_in_transaction_session_timeout='30s'`, `SET LOCAL TIME ZONE 'UTC'`, `SET LOCAL search_path=pg_catalog,public`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return nil, errors.New("snapshot limits unavailable")
		}
	}
	return tx, nil
}

const allowedTemplates = `('account.dsr-received','account.dsr-information-required','account.dsr-completed','account.dsr-action-required','account.dsr-rejected','account.dsr-extended')`
const scopedMessages = `caller_app_id='account-api' AND template_id IN ` + allowedTemplates

func inventory(ctx context.Context, db *sql.DB, expected string, ids []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := beginSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	m := snapshot{ObservationStart: time.Now().UTC(), RequestedIDs: ids, Sections: map[string][]json.RawMessage{}}
	if err := tx.QueryRowContext(ctx, `SELECT current_database()`).Scan(&m.Database); err != nil || m.Database != expected {
		return nil, errors.New("unexpected database")
	}
	queries := []query{
		{"messages", `SELECT row_to_json(r) FROM (SELECT id,template_id,status,resource_type,resource_id,created_at,updated_at,terminal_at,payload_purged_at,expires_at,xmin::text AS xmin FROM public.notification_messages WHERE id=ANY($1::uuid[]) AND ` + scopedMessages + ` ORDER BY id LIMIT 10001) r`, []any{ids}},
		{"deliveries", `SELECT row_to_json(r) FROM (SELECT id,message_id,status,attempt_count,next_attempt_at,lease_expires_at,sent_at,created_at,updated_at,xmin::text AS xmin FROM public.notification_deliveries WHERE message_id=ANY($1::uuid[]) ORDER BY id LIMIT 10001) r`, []any{ids}},
		{"outbox", `SELECT row_to_json(r) FROM (SELECT o.id,o.delivery_id,o.status,o.attempt_count,o.next_attempt_at,o.lease_expires_at,o.published_at,o.created_at,o.updated_at,o.xmin::text AS xmin FROM public.notification_outbox o JOIN public.notification_deliveries d ON d.id=o.delivery_id WHERE d.message_id=ANY($1::uuid[]) ORDER BY o.id LIMIT 10001) r`, []any{ids}},
		{"unknownNotApproved", `SELECT row_to_json(r) FROM (SELECT id,caller_app_id,template_id,status,xmin::text AS xmin FROM public.notification_messages WHERE template_id LIKE 'account.dsr-%' AND NOT(id=ANY($1::uuid[])) ORDER BY id LIMIT 10001) r`, []any{ids}},
		{"deliveryFences", `SELECT row_to_json(r) FROM (SELECT d.id AS delivery_id,EXISTS(SELECT 1 FROM pg_catalog.pg_locks l WHERE l.locktype='advisory' AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()) AND l.classid=((hashtextextended('notification-dsr-delivery:'||d.id::text,0)>>32)&4294967295)::oid AND l.objid=(hashtextextended('notification-dsr-delivery:'||d.id::text,0)&4294967295)::oid AND l.objsubid=1 AND l.granted) AS held FROM public.notification_deliveries d WHERE message_id=ANY($1::uuid[]) ORDER BY d.id LIMIT 10001) r`, []any{ids}},
	}
	queries = append(queries, catalogQueries([]string{"notification_messages", "notification_deliveries", "notification_outbox"})...)
	if err := collect(ctx, tx, queries, m.Sections); err != nil {
		return nil, err
	}
	if len(m.Sections["messages"]) != len(ids) {
		return nil, errors.New("requested message linkage incomplete")
	}
	if err := validateDependencies(m.Sections["foreignKeys"]); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("snapshot incomplete")
	}
	m.ObservationEnd = time.Now().UTC()
	m.Complete = true
	data, err := json.Marshal(m)
	if err != nil || len(data) > maxBytes {
		return nil, errors.New("inventory size or encoding failure")
	}
	return data, nil
}

func catalogQueries(tables []string) []query {
	return []query{
		{"columns", `SELECT row_to_json(r) FROM (SELECT n.nspname AS schema,c.relname AS table_name,a.attnum,a.attname,format_type(a.atttypid,a.atttypmod) AS type,a.attnotnull FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname=ANY($1::text[]) AND a.attnum>0 AND NOT a.attisdropped ORDER BY c.relname,a.attnum LIMIT 10001) r`, []any{tables}},
		{"foreignKeys", `SELECT row_to_json(r) FROM (SELECT c.conname,ns.nspname AS source_schema,s.relname AS source,nt.nspname AS target_schema,t.relname AS target,pg_get_constraintdef(c.oid) AS definition FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_class s ON s.oid=c.conrelid JOIN pg_catalog.pg_namespace ns ON ns.oid=s.relnamespace JOIN pg_catalog.pg_class t ON t.oid=c.confrelid JOIN pg_catalog.pg_namespace nt ON nt.oid=t.relnamespace WHERE c.contype='f' AND ((ns.nspname='public' AND s.relname=ANY($1::text[])) OR (nt.nspname='public' AND t.relname=ANY($1::text[]))) ORDER BY ns.nspname,s.relname,c.conname LIMIT 10001) r`, []any{tables}},
		{"triggers", `SELECT row_to_json(r) FROM (SELECT n.nspname AS schema,c.relname AS table_name,t.tgname,t.tgenabled,pg_get_triggerdef(t.oid) AS definition FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname=ANY($1::text[]) AND NOT t.tgisinternal ORDER BY c.relname,t.tgname LIMIT 10001) r`, []any{tables}},
	}
}

func collect(ctx context.Context, tx *sql.Tx, queries []query, sections map[string][]json.RawMessage) error {
	count, size := 0, 0
	for _, q := range queries {
		sections[q.name] = []json.RawMessage{}
		rows, err := tx.QueryContext(ctx, q.sql, q.args...)
		if err != nil {
			return errors.New("inventory query unavailable")
		}
		for rows.Next() {
			var row json.RawMessage
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return errors.New("inventory row unavailable")
			}
			count++
			size += len(row)
			if count > maxRows || size > maxBytes || !json.Valid(row) {
				rows.Close()
				return errors.New("inventory bound exceeded")
			}
			sections[q.name] = append(sections[q.name], row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return errors.New("inventory incomplete")
		}
	}
	return nil
}

func validateDependencies(rows []json.RawMessage) error {
	expected := map[string]string{"notification_deliveries": "notification_messages", "notification_outbox": "notification_deliveries"}
	if len(rows) != 2 {
		return errors.New("unknown dependency")
	}
	for _, row := range rows {
		var fk struct {
			SourceSchema string `json:"source_schema"`
			Source       string `json:"source"`
			TargetSchema string `json:"target_schema"`
			Target       string `json:"target"`
			Definition   string `json:"definition"`
		}
		if json.Unmarshal(row, &fk) != nil || fk.SourceSchema != "public" || fk.TargetSchema != "public" || expected[fk.Source] != fk.Target {
			return errors.New("unknown dependency")
		}
		want := "FOREIGN KEY (message_id) REFERENCES notification_messages(id) ON DELETE CASCADE"
		if fk.Source == "notification_outbox" {
			want = "FOREIGN KEY (delivery_id) REFERENCES notification_deliveries(id) ON DELETE CASCADE"
		}
		if fk.Definition != want {
			return errors.New("unknown dependency definition")
		}
		delete(expected, fk.Source)
	}
	if len(expected) != 0 {
		return errors.New("incomplete dependency inventory")
	}
	return nil
}
