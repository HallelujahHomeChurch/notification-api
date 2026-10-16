package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func manifestArgument(t *testing.T, m retirementManifest) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestRetirementManifest(t *testing.T) {
	m := testVersionManifest()
	if _, err := parseManifest(manifestArgument(t, m)); err != nil {
		t.Fatal("valid fixed manifest rejected")
	}
	for _, arg := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte(`{}`)), base64.StdEncoding.EncodeToString([]byte(`{"extra":true}`)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), maxManifestBytes+1))} {
		if _, err := parseManifest(arg); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	for _, mutate := range []func(*retirementManifest){
		func(m *retirementManifest) { m.Messages[0].ID = "00000000-0000-0000-0000-000000000000" },
		func(m *retirementManifest) { m.Messages[0].ID = strings.ToUpper(m.Messages[0].ID) },
		func(m *retirementManifest) { m.Messages[0].Xmin = "0" },
		func(m *retirementManifest) { m.Messages[0].Xmin = "01" },
		func(m *retirementManifest) { m.Messages[0].Xmin = "4294967296" },
		func(m *retirementManifest) { m.Messages[0].Xmin = "-1" },
		func(m *retirementManifest) { m.Deliveries[0].ID = m.Deliveries[1].ID },
		func(m *retirementManifest) { m.Deliveries[0].MessageID = m.Deliveries[1].MessageID },
		func(m *retirementManifest) { m.Outbox[0].DeliveryID = m.Outbox[1].DeliveryID },
		func(m *retirementManifest) { m.Outbox = m.Outbox[:2] },
	} {
		m := testVersionManifest()
		mutate(&m)
		if _, err := parseManifest(manifestArgument(t, m)); err == nil {
			t.Fatal("manifest scope expansion accepted")
		}
	}
	arg := manifestArgument(t, testVersionManifest())
	raw, _ := base64.StdEncoding.DecodeString(arg)
	for _, malformed := range []string{string(raw) + `{}`, strings.Replace(string(raw), `"messages":`, `"unknown":[],"messages":`, 1), strings.Replace(string(raw), `"messages":`, `"messages":[],"messages":`, 1), strings.Replace(string(raw), `"id":`, `"ID":`, 1)} {
		if _, err := parseManifest(base64.StdEncoding.EncodeToString([]byte(malformed))); err == nil {
			t.Fatal("extra or duplicate JSON accepted")
		}
	}
	for _, args := range [][]string{{"--backup"}, {"--retire"}, {"--retire", arg, "extra"}, {"--backup", "bad"}} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid invocation emitted output")
		}
	}
}

func testVersionManifest() retirementManifest {
	m := retirementManifest{}
	for i, id := range cohortMessages {
		m.Messages = append(m.Messages, rowVersion{ID: id, Xmin: "123"})
		m.Deliveries = append(m.Deliveries, deliveryVersion{ID: cohortDeliveries[i], MessageID: id, Xmin: "123"})
		m.Outbox = append(m.Outbox, outboxVersion{ID: cohortOutbox[i], DeliveryID: cohortDeliveries[i], Xmin: "123"})
	}
	return m
}

func seededRetirement(t *testing.T) (*sql.DB, string, retirementManifest, string) {
	t.Helper()
	db, name := inventoryTestDB(t)
	for i, id := range cohortMessages {
		if _, err := db.Exec(`INSERT INTO notification_messages(id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,status,terminal_at) VALUES($1::uuid,'account-api',$1::text,'synthetic-hash','account.dsr-received',1,'email','email','synthetic-target',decode('000102ff','hex'),decode('fffefd00','hex'),'account','synthetic-account','sent',now())`, id); err != nil {
			t.Fatal("seed message failed")
		}
		if _, err := db.Exec(`INSERT INTO notification_deliveries(id,message_id,channel,provider,status,sent_at,provider_message_id,last_error_code) VALUES($1,$2,'email','smtp','sent',now(),'synthetic-provider','synthetic-error')`, cohortDeliveries[i], id); err != nil {
			t.Fatal("seed delivery failed")
		}
		if _, err := db.Exec(`INSERT INTO notification_outbox(id,delivery_id,status,published_at) VALUES($1,$2,'published',now())`, cohortOutbox[i], cohortDeliveries[i]); err != nil {
			t.Fatal("seed outbox failed")
		}
	}
	ordinary := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO notification_messages(id,caller_app_id,idempotency_key,request_hash,template_id,template_version,channel,target_type,target_hash,target_ciphertext,payload_ciphertext,resource_type,resource_id,status) VALUES($1::uuid,'account-api',$1::text,'ordinary-hash','account.verify-email',1,'email','email','ordinary-target','ordinary-ciphertext','ordinary-payload','account','ordinary-account','queued')`, ordinary); err != nil {
		t.Fatal("seed ordinary failed")
	}
	m := testVersionManifest()
	for i := range m.Messages {
		if err := db.QueryRow(`SELECT xmin::text FROM notification_messages WHERE id=$1`, m.Messages[i].ID).Scan(&m.Messages[i].Xmin); err != nil {
			t.Fatal(err)
		}
	}
	for i := range m.Deliveries {
		if err := db.QueryRow(`SELECT xmin::text FROM notification_deliveries WHERE id=$1`, m.Deliveries[i].ID).Scan(&m.Deliveries[i].Xmin); err != nil {
			t.Fatal(err)
		}
	}
	for i := range m.Outbox {
		if err := db.QueryRow(`SELECT xmin::text FROM notification_outbox WHERE id=$1`, m.Outbox[i].ID).Scan(&m.Outbox[i].Xmin); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := parseManifest(manifestArgument(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return db, name, parsed, ordinary
}

func TestProtectedBackupRestoreAndExactRetirement(t *testing.T) {
	db, name, m, ordinary := seededRetirement(t)
	ctx := context.Background()
	data, err := operate(ctx, db, name, "--backup", m)
	if err != nil {
		t.Fatal("protected backup failed", err)
	}
	var backup protectedBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		t.Fatal("backup JSON invalid")
	}
	if !backup.ProtectedBackup || !backup.Complete || backup.ManifestSHA256 != contentHash(m) {
		t.Fatal("backup framing metadata incomplete")
	}
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		if len(backup.Rows[table]) != 3 || backup.ContentSHA256[table] != contentHash(backup.Rows[table]) {
			t.Fatal("backup hash/count mismatch")
		}
	}
	var rawMessage map[string]any
	if err := json.Unmarshal(backup.Rows["messages"][0], &rawMessage); err != nil {
		t.Fatal(err)
	}
	if rawMessage["target_ciphertext"] != "\\x000102ff" || rawMessage["payload_ciphertext"] != "\\xfffefd00" {
		t.Fatal("ciphertext changed")
	}
	// Restore the complete synthetic row projections into a separate empty database.
	restored, restoreName := inventoryTestDB(t)
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		for _, row := range backup.Rows[table] {
			if _, err := restored.Exec(`INSERT INTO notification_`+table+` SELECT * FROM json_populate_record(NULL::notification_`+table+`,$1::json)`, string(row)); err != nil {
				t.Fatal("isolated restore failed")
			}
		}
	}
	tx, err := beginSnapshot(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string][]json.RawMessage{}
	queries := []query{}
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		queries = append(queries, query{name: table, sql: `SELECT row_to_json(r) FROM notification_` + table + ` r ORDER BY id`})
	}
	if err := collect(ctx, tx, queries, sections); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		if contentHash(sections[table]) != backup.ContentSHA256[table] {
			t.Fatal("isolated restore row hash mismatch")
		}
	}
	tx.Rollback()
	if restoreName == name {
		t.Fatal("restore DB not isolated")
	}
	var before string
	if err := db.QueryRow(`SELECT row_to_json(m)::text FROM notification_messages m WHERE id=$1`, ordinary).Scan(&before); err != nil {
		t.Fatal(err)
	}
	receiptJSON, err := operate(ctx, db, name, "--retire", m)
	if err != nil {
		t.Fatal("exact retirement failed", err)
	}
	var receipt retirementReceipt
	if err := json.Unmarshal(receiptJSON, &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Complete || receipt.ManifestSHA256 != backup.ManifestSHA256 || receipt.Counts["messages"] != 3 || receipt.Counts["deliveries"] != 3 || receipt.Counts["outbox"] != 3 {
		t.Fatal("retirement receipt incomplete")
	}
	for _, private := range []string{"target_ciphertext", "payload_ciphertext", "synthetic-provider", "synthetic-error"} {
		if strings.Contains(string(receiptJSON), private) {
			t.Fatal("private data in retirement receipt")
		}
	}
	var after string
	if err := db.QueryRow(`SELECT row_to_json(m)::text FROM notification_messages m WHERE id=$1`, ordinary).Scan(&after); err != nil || before != after {
		t.Fatal("ordinary notification changed")
	}
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		var n int
		ids := cohortMessages
		if table == "deliveries" {
			ids = cohortDeliveries
		}
		if table == "outbox" {
			ids = cohortOutbox
		}
		if err := db.QueryRow(`SELECT count(*) FROM notification_`+table+` WHERE id=ANY($1::uuid[])`, ids).Scan(&n); err != nil || n != 0 {
			t.Fatal("retirement left approved rows")
		}
	}
	if result, err := operate(ctx, db, name, "--retire", m); err == nil || result != nil {
		t.Fatal("replay reported completed retirement")
	}
	if err := db.QueryRow(`SELECT row_to_json(m)::text FROM notification_messages m WHERE id=$1`, ordinary).Scan(&after); err != nil || before != after {
		t.Fatal("replay touched ordinary row")
	}
}

func TestRetirementDriftRejectsBothModes(t *testing.T) {
	cases := []struct {
		name, sql     string
		manifestDrift bool
	}{
		{name: "wrong version", manifestDrift: true},
		{name: "message state", sql: `UPDATE notification_messages SET status='queued' WHERE id='55a8fe75-9a3e-428c-b055-4f0498b82503'`},
		{name: "wrong caller", sql: `UPDATE notification_messages SET caller_app_id='other-api' WHERE id='55a8fe75-9a3e-428c-b055-4f0498b82503'`},
		{name: "wrong template", sql: `UPDATE notification_messages SET template_id='account.verify-email' WHERE id='55a8fe75-9a3e-428c-b055-4f0498b82503'`},
		{name: "wrong linkage", sql: `UPDATE notification_deliveries SET message_id='6c4b0f04-c3ed-463a-8647-c6aefe4d6c8f' WHERE id='be4d4132-25a7-409a-817c-52ee59b5594e'`},
		{name: "active delivery lease", sql: `UPDATE notification_deliveries SET lease_expires_at=now()+interval '1 minute' WHERE id='be4d4132-25a7-409a-817c-52ee59b5594e'`},
		{name: "active outbox lease", sql: `UPDATE notification_outbox SET lease_expires_at=now()+interval '1 minute' WHERE id='0f554e7a-ac2d-4306-af13-d48f8bdbdf8a'`},
		{name: "changed outbox state", sql: `UPDATE notification_outbox SET status='pending' WHERE id='0f554e7a-ac2d-4306-af13-d48f8bdbdf8a'`},
		{name: "extra dependency", sql: `CREATE TABLE unknown_child(id uuid REFERENCES notification_messages(id))`},
		{name: "schema column", sql: `ALTER TABLE notification_messages ADD COLUMN unknown text`},
		{name: "RLS schema", sql: `ALTER TABLE notification_messages ENABLE ROW LEVEL SECURITY`},
		{name: "unknown rule", sql: `CREATE RULE unknown_rule AS ON DELETE TO notification_messages DO INSTEAD NOTHING`},
		{name: "unknown trigger", sql: `CREATE FUNCTION unknown_trigger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN OLD; END $$;CREATE TRIGGER unknown BEFORE DELETE ON notification_messages FOR EACH ROW EXECUTE FUNCTION unknown_trigger()`},
		{name: "unknown DSR", sql: `UPDATE notification_messages SET template_id='account.dsr-received' WHERE id NOT IN ('55a8fe75-9a3e-428c-b055-4f0498b82503','6c4b0f04-c3ed-463a-8647-c6aefe4d6c8f','d7fed053-0fee-401a-88db-f6c21f3fcaa8')`},
		{name: "extra child", sql: `INSERT INTO notification_outbox(id,delivery_id,status,published_at) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','be4d4132-25a7-409a-817c-52ee59b5594e','published',now())`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, name, m, _ := seededRetirement(t)
			if tc.sql != "" {
				if _, err := db.Exec(tc.sql); err != nil {
					t.Fatal("drift seed failed")
				}
			}
			if tc.manifestDrift {
				m.Messages[0].Xmin = "1"
			}
			for _, mode := range []string{"--backup", "--retire"} {
				if result, err := operate(context.Background(), db, name, mode, m); err == nil || result != nil {
					t.Fatal("drift accepted", mode)
				}
			}
			for _, table := range []string{"messages", "deliveries", "outbox"} {
				var n int
				ids := cohortMessages
				if table == "deliveries" {
					ids = cohortDeliveries
				}
				if table == "outbox" {
					ids = cohortOutbox
				}
				if err := db.QueryRow(`SELECT count(*) FROM notification_`+table+` WHERE id=ANY($1::uuid[])`, ids).Scan(&n); err != nil || n != 3 {
					t.Fatal("failed operation changed approved rows")
				}
			}
		})
	}
}

func TestHeldDeliveryFenceRejectsBackupAndRetirement(t *testing.T) {
	db, name, m, _ := seededRetirement(t)
	ctx := context.Background()
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "notification-dsr-delivery:"+cohortDeliveries[0]); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "notification-dsr-delivery:"+cohortDeliveries[0])
	for _, mode := range []string{"--backup", "--retire"} {
		if result, err := operate(ctx, db, name, mode, m); err == nil || result != nil {
			t.Fatal("held delivery fence accepted")
		}
	}
}

func TestParentDeleteFailureRollsBackChildren(t *testing.T) {
	db, name, m, _ := seededRetirement(t)
	db.SetMaxOpenConns(1)
	role := "retirement_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.Exec(`CREATE ROLE ` + role + `; GRANT USAGE ON SCHEMA public TO ` + role + `;GRANT SELECT,UPDATE ON notification_messages,notification_deliveries,notification_outbox TO ` + role + `;GRANT DELETE ON notification_deliveries,notification_outbox TO ` + role); err != nil {
		t.Fatal("local restricted role failed")
	}
	t.Cleanup(func() { db.Exec(`RESET ROLE`); db.Exec(`DROP OWNED BY ` + role); db.Exec(`DROP ROLE ` + role) })
	if _, err := db.Exec(`SET ROLE ` + role); err != nil {
		t.Fatal(err)
	}
	if result, err := operate(context.Background(), db, name, "--retire", m); err == nil || result != nil || err.Error() != "retirement delete failed" {
		t.Fatal("parent delete stage did not fail as expected")
	}
	for _, table := range []string{"messages", "deliveries", "outbox"} {
		var n int
		ids := cohortMessages
		if table == "deliveries" {
			ids = cohortDeliveries
		}
		if table == "outbox" {
			ids = cohortOutbox
		}
		if err := db.QueryRow(`SELECT count(*) FROM notification_`+table+` WHERE id=ANY($1::uuid[])`, ids).Scan(&n); err != nil || n != 3 {
			t.Fatal("failed parent delete did not roll back children")
		}
	}
}

func TestReturningSetComparison(t *testing.T) {
	expected := []rowVersion{{"a", "1"}, {"b", "2"}}
	for _, actual := range [][]rowVersion{{{"a", "1"}}, {{"a", "1"}, {"b", "3"}}, {{"a", "1"}, {"a", "1"}}, {{"a", "1"}, {"b", "2"}, {"c", "3"}}} {
		if sameVersions(actual, expected) {
			t.Fatal("RETURNING mismatch accepted")
		}
	}
	if !sameVersions([]rowVersion{{"b", "2"}, {"a", "1"}}, expected) {
		t.Fatal("exact RETURNING set rejected")
	}
}

func TestBackupFullRowByteBound(t *testing.T) {
	db, name, m, _ := seededRetirement(t)
	if _, err := db.Exec(`UPDATE notification_messages SET payload_ciphertext=convert_to(repeat('x',4194305),'UTF8') WHERE id=$1`, m.Messages[0].ID); err != nil {
		t.Fatal("oversized synthetic row seed failed")
	}
	if err := db.QueryRow(`SELECT xmin::text FROM notification_messages WHERE id=$1`, m.Messages[0].ID).Scan(&m.Messages[0].Xmin); err != nil {
		t.Fatal(err)
	}
	if data, err := operate(context.Background(), db, name, "--backup", m); err == nil || data != nil {
		t.Fatal("oversized full-row backup accepted")
	}
}

func TestOperationFramesAreDistinct(t *testing.T) {
	for _, kind := range []string{"BACKUP", "RETIREMENT"} {
		var out bytes.Buffer
		if err := writeOperationFrame(&out, []byte(`{"complete":true}`), kind); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), "DSR_"+kind+"_V1_BEGIN\n") || !strings.HasSuffix(out.String(), "DSR_"+kind+"_V1_COMPLETE\n") || strings.Contains(out.String(), "DSR_INVENTORY") {
			t.Fatal("operation frame ambiguous")
		}
		out.Reset()
		if err := writeOperationFrame(&out, []byte(`{`), kind); err == nil || out.Len() != 0 {
			t.Fatal("incomplete operation emitted frame")
		}
	}
}
