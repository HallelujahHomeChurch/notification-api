package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

const maxManifestBytes = 4096

// Exact reviewed A4 cohort; only row versions may change in operator manifests.
var cohortMessages = []string{"55a8fe75-9a3e-428c-b055-4f0498b82503", "6c4b0f04-c3ed-463a-8647-c6aefe4d6c8f", "d7fed053-0fee-401a-88db-f6c21f3fcaa8"}
var cohortDeliveries = []string{"be4d4132-25a7-409a-817c-52ee59b5594e", "19126b7a-5318-4842-8b64-a1601869875c", "4d4babd6-fccc-4ee4-9071-8e8171d74723"}
var cohortOutbox = []string{"0f554e7a-ac2d-4306-af13-d48f8bdbdf8a", "b2cfabb2-b99d-409a-9036-19962ccf57e9", "7b01266f-6873-4187-922f-6c71ded2a90d"}

// Filled from the reviewed A4 preview's ordered columns, compact JSON, SHA-256.
const baselineColumnsSHA256 = "6589fb3f90ab83703328d33492116af57b0dc2013771c316b155f8d81f91a750"

type rowVersion struct {
	ID   string `json:"id"`
	Xmin string `json:"xmin"`
}
type deliveryVersion struct {
	ID        string `json:"id"`
	MessageID string `json:"messageId"`
	Xmin      string `json:"xmin"`
}
type outboxVersion struct {
	ID         string `json:"id"`
	DeliveryID string `json:"deliveryId"`
	Xmin       string `json:"xmin"`
}
type retirementManifest struct {
	Messages   []rowVersion      `json:"messages"`
	Deliveries []deliveryVersion `json:"deliveries"`
	Outbox     []outboxVersion   `json:"outbox"`
}

func parseManifest(arg string) (retirementManifest, error) {
	var m retirementManifest
	if len(arg) > base64.StdEncoding.EncodedLen(maxManifestBytes) {
		return m, errors.New("manifest bound exceeded")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(arg)
	if err != nil || len(raw) == 0 || len(raw) > maxManifestBytes {
		return m, errors.New("invalid manifest")
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, errors.New("invalid manifest fields")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return m, errors.New("extra manifest content")
	}
	if len(m.Messages) != len(cohortMessages) || len(m.Deliveries) != len(cohortDeliveries) || len(m.Outbox) != len(cohortOutbox) {
		return m, errors.New("incomplete cohort")
	}
	expectedMessages, expectedDeliveries, expectedOutbox := map[string]bool{}, map[string]string{}, map[string]string{}
	for i, id := range cohortMessages {
		expectedMessages[id] = true
		expectedDeliveries[cohortDeliveries[i]] = id
		expectedOutbox[cohortOutbox[i]] = cohortDeliveries[i]
	}
	for _, row := range m.Messages {
		if !expectedMessages[row.ID] || !validXmin(row.Xmin) {
			return m, errors.New("invalid message version")
		}
		delete(expectedMessages, row.ID)
	}
	for _, row := range m.Deliveries {
		if expectedDeliveries[row.ID] == "" || expectedDeliveries[row.ID] != row.MessageID || !validXmin(row.Xmin) {
			return m, errors.New("invalid delivery version")
		}
		delete(expectedDeliveries, row.ID)
	}
	for _, row := range m.Outbox {
		if expectedOutbox[row.ID] == "" || expectedOutbox[row.ID] != row.DeliveryID || !validXmin(row.Xmin) {
			return m, errors.New("invalid outbox version")
		}
		delete(expectedOutbox, row.ID)
	}
	sort.Slice(m.Messages, func(i, j int) bool { return m.Messages[i].ID < m.Messages[j].ID })
	sort.Slice(m.Deliveries, func(i, j int) bool { return m.Deliveries[i].ID < m.Deliveries[j].ID })
	sort.Slice(m.Outbox, func(i, j int) bool { return m.Outbox[i].ID < m.Outbox[j].ID })
	return m, nil
}

func validXmin(value string) bool {
	n, err := strconv.ParseUint(value, 10, 32)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == value
}

// encoding/json accepts duplicate keys; manifests must not have ambiguous fields.
func uniqueJSON(dec *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("manifest nesting exceeded")
	}
	token, err := dec.Token()
	if err != nil {
		return errors.New("invalid manifest JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return errors.New("invalid manifest key")
			}
			name, ok := key.(string)
			switch name {
			case "messages", "deliveries", "outbox", "id", "xmin", "messageId", "deliveryId":
			default:
				return errors.New("unknown manifest key")
			}
			if !ok || seen[name] {
				return errors.New("duplicate manifest key")
			}
			seen[name] = true
			if err := uniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := uniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid manifest structure")
	}
	if _, err := dec.Token(); err != nil {
		return errors.New("invalid manifest end")
	}
	return nil
}

type protectedBackup struct {
	ProtectedBackup bool                         `json:"protectedBackup"`
	Database        string                       `json:"database"`
	ObservedAt      time.Time                    `json:"observedAt"`
	Manifest        retirementManifest           `json:"manifest"`
	ManifestSHA256  string                       `json:"manifestSHA256"`
	ContentSHA256   map[string]string            `json:"contentSHA256"`
	Rows            map[string][]json.RawMessage `json:"rows"`
	Complete        bool                         `json:"complete"`
}

type retirementReceipt struct {
	Database       string             `json:"database"`
	CommittedAt    time.Time          `json:"committedAt"`
	ManifestSHA256 string             `json:"manifestSHA256"`
	Deleted        retirementManifest `json:"deleted"`
	Counts         map[string]int     `json:"counts"`
	Complete       bool               `json:"complete"`
}

func contentHash(v any) string {
	data, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func operate(ctx context.Context, db *sql.DB, expected, mode string, m retirementManifest) ([]byte, error) {
	// Validate even internal callers; the only variable permitted is the cohort's xmin.
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, errors.New("invalid manifest")
	}
	m, err = parseManifest(base64.StdEncoding.EncodeToString(encoded))
	if err != nil {
		return nil, err
	}
	if mode != "--backup" && mode != "--retire" {
		return nil, errors.New("invalid operation")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var tx *sql.Tx
	if mode == "--backup" {
		tx, err = beginSnapshot(ctx, db)
	} else {
		tx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err == nil {
			err = setTransactionLimits(ctx, tx)
		}
	}
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return nil, errors.New("operation transaction unavailable")
	}
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name); err != nil || name != expected {
		return nil, errors.New("unexpected operation database")
	}
	if mode == "--retire" {
		for _, delivery := range m.Deliveries {
			var acquired bool
			if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+delivery.ID).Scan(&acquired); err != nil || !acquired {
				return nil, errors.New("delivery fence unavailable")
			}
		}
		// Same lock mode as DELETE: ordinary DML remains compatible while DDL is fenced.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE public.notification_messages,public.notification_deliveries,public.notification_outbox IN ROW EXCLUSIVE MODE`); err != nil {
			return nil, errors.New("schema fence unavailable")
		}
		for _, q := range []query{
			{sql: `SELECT id FROM public.notification_messages WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, args: []any{cohortMessages}},
			{sql: `SELECT id FROM public.notification_deliveries WHERE message_id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, args: []any{cohortMessages}},
			{sql: `SELECT id FROM public.notification_outbox WHERE delivery_id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, args: []any{cohortDeliveries}},
		} {
			rows, err := tx.QueryContext(ctx, q.sql, q.args...)
			if err != nil {
				return nil, errors.New("row fence unavailable")
			}
			n := 0
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return nil, errors.New("row fence incomplete")
				}
				n++
				if n > len(cohortMessages) {
					rows.Close()
					return nil, errors.New("unexpected linked row")
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil || n != len(cohortMessages) {
				return nil, errors.New("row fence incomplete")
			}
		}
	}
	sections := map[string][]json.RawMessage{}
	queries := []query{
		{"messages", `SELECT row_to_json(r) FROM (SELECT id,xmin::text AS xmin,status,caller_app_id,template_id,resource_type FROM public.notification_messages WHERE id=ANY($1::uuid[]) ORDER BY id LIMIT 10001) r`, []any{cohortMessages}},
		{"deliveries", `SELECT row_to_json(r) FROM (SELECT id,message_id,xmin::text AS xmin,status,lease_expires_at,sent_at FROM public.notification_deliveries WHERE message_id=ANY($1::uuid[]) ORDER BY id LIMIT 10001) r`, []any{cohortMessages}},
		{"outbox", `SELECT row_to_json(r) FROM (SELECT id,delivery_id,xmin::text AS xmin,status,lease_expires_at,published_at FROM public.notification_outbox WHERE delivery_id=ANY($1::uuid[]) ORDER BY id LIMIT 10001) r`, []any{cohortDeliveries}},
		{"heldFences", `SELECT row_to_json(r) FROM (SELECT d.id FROM public.notification_deliveries d WHERE d.message_id=ANY($1::uuid[]) AND EXISTS(SELECT 1 FROM pg_catalog.pg_locks l WHERE l.locktype='advisory' AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()) AND l.classid=((hashtextextended('notification-dsr-delivery:'||d.id::text,0)>>32)&4294967295)::oid AND l.objid=(hashtextextended('notification-dsr-delivery:'||d.id::text,0)&4294967295)::oid AND l.objsubid=1 AND l.granted AND l.pid<>pg_backend_pid()) ORDER BY d.id LIMIT 10001) r`, []any{cohortMessages}},
		{"unsafeRelations", `SELECT row_to_json(r) FROM (SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('notification_messages','notification_deliveries','notification_outbox') AND (c.relkind<>'r' OR c.relrowsecurity OR c.relforcerowsecurity OR c.relispartition OR EXISTS(SELECT 1 FROM pg_catalog.pg_rewrite w WHERE w.ev_class=c.oid)) ORDER BY c.relname LIMIT 10001) r`, nil},
		{"unknown", `SELECT row_to_json(r) FROM (SELECT id FROM public.notification_messages WHERE template_id LIKE 'account.dsr-%' AND NOT(id=ANY($1::uuid[])) ORDER BY id LIMIT 10001) r`, []any{cohortMessages}},
	}
	queries = append(queries, catalogQueries([]string{"notification_messages", "notification_deliveries", "notification_outbox"})...)
	if err := collect(ctx, tx, queries, sections); err != nil {
		return nil, err
	}
	if err := validateOperationRows(m, sections); err != nil {
		return nil, err
	}
	if mode == "--backup" {
		// This is the sole path that reads full private rows; no decryption is performed.
		raw := map[string][]json.RawMessage{}
		if err := collect(ctx, tx, []query{
			{"messages", `SELECT row_to_json(m) FROM public.notification_messages m WHERE id=ANY($1::uuid[]) ORDER BY id LIMIT 10001`, []any{cohortMessages}},
			{"deliveries", `SELECT row_to_json(d) FROM public.notification_deliveries d WHERE message_id=ANY($1::uuid[]) ORDER BY id LIMIT 10001`, []any{cohortMessages}},
			{"outbox", `SELECT row_to_json(o) FROM public.notification_outbox o WHERE delivery_id=ANY($1::uuid[]) ORDER BY id LIMIT 10001`, []any{cohortDeliveries}},
		}, raw); err != nil {
			return nil, err
		}
		data, err := json.Marshal(protectedBackup{ProtectedBackup: true, Database: name, ObservedAt: time.Now().UTC(), Manifest: m, ManifestSHA256: contentHash(m), ContentSHA256: map[string]string{"messages": contentHash(raw["messages"]), "deliveries": contentHash(raw["deliveries"]), "outbox": contentHash(raw["outbox"])}, Rows: raw, Complete: true})
		if err != nil || len(data) > maxBytes {
			return nil, errors.New("backup bound exceeded")
		}
		if err := tx.Commit(); err != nil {
			return nil, errors.New("backup snapshot incomplete")
		}
		return data, nil
	}
	for _, q := range deletionQueries(m) {
		rows, err := tx.QueryContext(ctx, q.sql, q.args...)
		if err != nil {
			return nil, errors.New("retirement delete failed")
		}
		returned := []rowVersion{}
		for rows.Next() {
			var row rowVersion
			if err := rows.Scan(&row.ID, &row.Xmin); err != nil {
				rows.Close()
				return nil, errors.New("retirement returning unavailable")
			}
			returned = append(returned, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !sameVersions(returned, expectedVersions(m, q.name)) {
			return nil, errors.New("retirement returning mismatch")
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("retirement commit unconfirmed")
	}
	return json.Marshal(retirementReceipt{Database: name, CommittedAt: time.Now().UTC(), ManifestSHA256: contentHash(m), Deleted: m, Counts: map[string]int{"messages": len(m.Messages), "deliveries": len(m.Deliveries), "outbox": len(m.Outbox)}, Complete: true})
}

func validateOperationRows(m retirementManifest, sections map[string][]json.RawMessage) error {
	if len(sections["unknown"]) != 0 || len(sections["heldFences"]) != 0 || len(sections["unsafeRelations"]) != 0 || len(sections["triggers"]) != 0 || contentHash(sections["columns"]) != baselineColumnsSHA256 {
		return errors.New("unexpected operation schema or cohort")
	}
	if err := validateDependencies(sections["foreignKeys"]); err != nil {
		return err
	}
	for _, name := range []string{"messages", "deliveries", "outbox"} {
		if len(sections[name]) != len(cohortMessages) {
			return errors.New("incomplete operation cohort")
		}
		actual := []rowVersion{}
		for _, raw := range sections[name] {
			var row struct {
				ID         string     `json:"id"`
				Xmin       string     `json:"xmin"`
				MessageID  string     `json:"message_id"`
				DeliveryID string     `json:"delivery_id"`
				Status     string     `json:"status"`
				Caller     string     `json:"caller_app_id"`
				Template   string     `json:"template_id"`
				Resource   string     `json:"resource_type"`
				Lease      *time.Time `json:"lease_expires_at"`
				Sent       *time.Time `json:"sent_at"`
				Published  *time.Time `json:"published_at"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				return errors.New("invalid operation metadata")
			}
			actual = append(actual, rowVersion{ID: row.ID, Xmin: row.Xmin})
			switch name {
			case "messages":
				if row.Status != "sent" || row.Caller != "account-api" || row.Template != "account.dsr-received" || row.Resource != "account" {
					return errors.New("unexpected message state")
				}
			case "deliveries":
				expected := ""
				for _, v := range m.Deliveries {
					if v.ID == row.ID {
						expected = v.MessageID
					}
				}
				if row.MessageID != expected || row.Status != "sent" || row.Lease != nil || row.Sent == nil {
					return errors.New("unexpected delivery state")
				}
			case "outbox":
				expected := ""
				for _, v := range m.Outbox {
					if v.ID == row.ID {
						expected = v.DeliveryID
					}
				}
				if row.DeliveryID != expected || row.Status != "published" || row.Lease != nil || row.Published == nil {
					return errors.New("unexpected outbox state")
				}
			}
		}
		if !sameVersions(actual, expectedVersions(m, name)) {
			return errors.New("operation version drift")
		}
	}
	return nil
}

func expectedVersions(m retirementManifest, name string) []rowVersion {
	switch name {
	case "messages":
		return m.Messages
	case "deliveries":
		rows := []rowVersion{}
		for _, v := range m.Deliveries {
			rows = append(rows, rowVersion{v.ID, v.Xmin})
		}
		return rows
	case "outbox":
		rows := []rowVersion{}
		for _, v := range m.Outbox {
			rows = append(rows, rowVersion{v.ID, v.Xmin})
		}
		return rows
	}
	return nil
}
func sameVersions(a, b []rowVersion) bool {
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func deletionQueries(m retirementManifest) []query {
	result := []query{}
	for _, name := range []string{"outbox", "deliveries", "messages"} {
		ids, versions := []string{}, []string{}
		for _, row := range expectedVersions(m, name) {
			ids = append(ids, row.ID)
			versions = append(versions, row.Xmin)
		}
		table := "public.notification_" + name
		result = append(result, query{name: name, sql: `DELETE FROM ` + table + ` t USING unnest($1::uuid[],$2::text[]) approved(id,xmin) WHERE t.id=approved.id AND t.xmin::text=approved.xmin RETURNING t.id,t.xmin::text`, args: []any{ids, versions}})
	}
	return result
}
