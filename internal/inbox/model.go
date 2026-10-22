package inbox

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalid     = errors.New("invalid inbox request")
	ErrConflict    = errors.New("inbox identity conflict")
	ErrUnavailable = errors.New("inbox unavailable")
	ErrNotFound    = errors.New("inbox item not found")
)

const Retention = 90 * 24 * time.Hour

// Resource is a closed union; validation rejects fields from another variant.
type Resource struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	UnitID string `json:"unitId,omitempty"`
	Series string `json:"series,omitempty"`
}
type Write struct {
	RecipientAccountID string    `json:"recipientAccountId"`
	Type               string    `json:"type"`
	LogicalKey         string    `json:"logicalKey"`
	Resource           Resource  `json:"resource"`
	EventVersion       int64     `json:"eventVersion"`
	OccurredAt         time.Time `json:"occurredAt"`
}
type Ref struct {
	ID       string   `json:"id"`
	Resource Resource `json:"resource"`
}
type Action struct {
	Type     string   `json:"type"`
	Resource Resource `json:"resource"`
	Locale   string   `json:"locale,omitempty"`
}
type Projection struct {
	TitleKey   string            `json:"titleKey"`
	MessageKey string            `json:"messageKey"`
	Parameters map[string]string `json:"parameters"`
	Action     *Action           `json:"action,omitempty"`
}
type Item struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Resource   Resource   `json:"resource"`
	OccurredAt time.Time  `json:"occurredAt"`
	ReadAt     *time.Time `json:"readAt,omitempty"`
	Projection Projection `json:"projection"`
	Sequence   int64      `json:"-"`
}
type Page struct {
	Items             []Item    `json:"items"`
	NextCursor        string    `json:"nextCursor,omitempty"`
	ReadSnapshotToken string    `json:"readSnapshotToken"`
	AsOf              time.Time `json:"asOf"`
}
type Summary struct {
	Items             []Item    `json:"items"`
	UnreadCount       int64     `json:"unreadCount"`
	ReadSnapshotToken string    `json:"readSnapshotToken"`
	AsOf              time.Time `json:"asOf"`
}
type VisibilityChecker interface {
	Check(context.Context, string, string, []Ref) (map[string]Projection, error)
}

type AcceptanceItem struct {
	Index  int    `json:"index"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
}
type Acceptance struct {
	Items []AcceptanceItem `json:"items"`
}

// ExportPage is an internal subject export, without business-owner projections.
type ExportItem struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Resource   Resource   `json:"resource"`
	OccurredAt time.Time  `json:"occurredAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ReadAt     *time.Time `json:"readAt,omitempty"`
}
type ExportPage struct {
	Items        []ExportItem `json:"items"`
	NextSequence int64        `json:"nextSequence,omitempty"`
}
