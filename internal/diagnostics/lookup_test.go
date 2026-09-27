package diagnostics

import (
	"context"
	"testing"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
)

func TestLookupRejectsInvalidOrUnboundedRequests(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name  string
		email string
		from  time.Time
		to    time.Time
		keys  map[string][]byte
	}{
		{"invalid email", "not-an-email", now.Add(-time.Hour), now, map[string][]byte{"v1": []byte("key")}},
		{"missing keys", "user@example.test", now.Add(-time.Hour), now, nil},
		{"empty interval", "user@example.test", now, now, map[string][]byte{"v1": []byte("key")}},
		{"wide interval", "user@example.test", now.Add(-32 * 24 * time.Hour), now, map[string][]byte{"v1": []byte("key")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Lookup(context.Background(), nil, test.keys, test.email, test.from, test.to); err == nil {
				t.Fatal("Lookup() error = nil")
			}
		})
	}
}

func TestCandidateHashesNormalizeEmailAcrossKeyRotation(t *testing.T) {
	keys := map[string][]byte{"v2": []byte("new-key"), "v1": []byte("old-key")}
	ids, hashes := candidateHashes(keys, " User@Example.Test ")
	if len(ids) != 2 || ids[0] != "v1" || ids[1] != "v2" {
		t.Fatalf("ids = %v", ids)
	}
	for i, id := range ids {
		if hashes[i] != notificationcrypto.Hash(keys[id], []byte("user@example.test")) {
			t.Fatalf("hash for %s is incorrect", id)
		}
	}
}
