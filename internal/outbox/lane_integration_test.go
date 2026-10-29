//go:build integration

package outbox

import (
	"context"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
)

func TestPostgresOutboxLanesExcludeCampaignBacklog(t *testing.T) {
	db := integrationDatabase(t)
	bulk := insertOutboxFixture(t, db, "pending", nil)
	transaction := insertOutboxFixture(t, db, "pending", nil)
	push := insertOutboxFixture(t, db, "pending", nil)
	for _, fixture := range []struct{ id, channel, template string }{
		{bulk, "email", "engagement.newsletter"},
		{transaction, "email", "account.verify-email"},
		{push, "web_push", "engagement.web-push"},
	} {
		if _, err := db.Exec(`UPDATE notification_messages m SET channel=$2,template_id=$3 FROM notification_outbox o JOIN notification_deliveries d ON d.id=o.delivery_id WHERE o.id=$1 AND m.id=d.message_id`, fixture.id, fixture.channel, fixture.template); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		lane queue.Lane
		id   string
	}{{queue.LaneTransactional, transaction}, {queue.LaneWebPush, push}, {queue.LaneBulk, bulk}} {
		c, ok, err := (postgresStore{db: db, lane: tc.lane}).claimNext(context.Background(), leaseDuration)
		if err != nil || !ok || c.OutboxID != tc.id {
			t.Fatalf("lane %s claimed %+v, ok=%v: %v", tc.lane, c, ok, err)
		}
		if _, ok, err := (postgresStore{db: db, lane: tc.lane}).claimNext(context.Background(), leaseDuration); err != nil || ok {
			t.Fatalf("lane %s consumed another lane: ok=%v %v", tc.lane, ok, err)
		}
	}
}
