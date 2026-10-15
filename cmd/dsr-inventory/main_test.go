package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestArgumentsAndTLS(t *testing.T) {
	id := "55a8fe75-9a3e-428c-b055-4f0498b82503"
	ids, err := parseIDs([]string{id, strings.ToUpper(id)})
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids: %v %v", ids, err)
	}
	for _, args := range [][]string{nil, {"bad"}, {"00000000-0000-0000-0000-000000000000"}} {
		if _, err := parseIDs(args); err == nil {
			t.Fatal("invalid IDs accepted")
		}
	}
	for _, dsn := range []string{"", "postgres://u:p@localhost/notification?sslmode=require", "postgres://u:p@hhc-pg.postgres.database.azure.com/engagement?sslmode=require", "postgres://u:p@hhc-pg.postgres.database.azure.com/notification?sslmode=disable", "postgres://u:p@hhc-pg.postgres.database.azure.com/notification?sslmode=prefer"} {
		if _, err := databaseConfig(dsn); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	cfg, err := databaseConfig("postgres://u:p@hhc-pg.postgres.database.azure.com/notification?sslmode=require")
	if err != nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName != databaseHost || cfg.TLSConfig.VerifyConnection != nil || len(cfg.Fallbacks) != 0 {
		t.Fatal("TLS verification missing")
	}
}

func TestFrameAndBounds(t *testing.T) {
	var out bytes.Buffer
	data := []byte(`{"complete":true}`)
	if err := writeFrame(&out, data); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("DSR_INVENTORY_V1_BEGIN\n%s\nSHA256 %x\nDSR_INVENTORY_V1_COMPLETE\n", data, sha256.Sum256(data))
	if out.String() != want {
		t.Fatal("invalid frame")
	}
	out.Reset()
	if err := writeFrame(&out, bytes.Repeat([]byte("x"), maxBytes+1)); err == nil || out.Len() != 0 {
		t.Fatal("oversized frame emitted")
	}
	out.Reset()
	if err := execute(context.Background(), nil, &out); err == nil || out.Len() != 0 {
		t.Fatal("invalid invocation emitted output")
	}
}
