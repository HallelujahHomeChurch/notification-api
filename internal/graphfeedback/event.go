package graphfeedback

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/google/uuid"
)

var recipientHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// VerifiedEvent is produced only after mailbox provenance and the original
// submission were verified. Store independently rechecks the durable binding.
type VerifiedEvent struct {
	MailboxID          uuid.UUID
	DeliveryID         uuid.UUID
	Attempt            int
	SourceEventID      string
	RecipientHashKeyID string
	RecipientHash      string
	Outcome            contracts.DeliveryOutcome
	OccurredAt         time.Time
}

func (e VerifiedEvent) validFor(mailbox uuid.UUID) bool {
	return e.MailboxID == mailbox && e.DeliveryID != uuid.Nil && e.Attempt > 0 &&
		e.SourceEventID != "" && len(e.SourceEventID) <= 1024 &&
		e.RecipientHashKeyID != "" && len(e.RecipientHashKeyID) <= 128 &&
		recipientHashPattern.MatchString(e.RecipientHash) && !e.OccurredAt.IsZero() &&
		(e.Outcome == contracts.DeliveryOutcomeDelivered || e.Outcome == contracts.DeliveryOutcomeBounced)
}

func (e VerifiedEvent) key() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(e.MailboxID.String()+"\x00"+e.SourceEventID)))
}
