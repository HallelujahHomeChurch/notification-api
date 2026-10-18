package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }
func (s *Store) AcceptBatch(ctx context.Context, producer string, items []Write, now time.Time) (Acceptance, error) {
	if len(items) < 1 || len(items) > 100 {
		return Acceptance{}, ErrInvalid
	}
	recipients := map[string]bool{}
	for _, w := range items {
		if !validWrite(producer, w) || w.OccurredAt.After(now.Add(5*time.Minute)) {
			return Acceptance{}, ErrInvalid
		}
		recipients[w.RecipientAccountID] = true
	}
	ids := make([]string, 0, len(recipients))
	for id := range recipients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Acceptance{}, err
	}
	defer tx.Rollback()
	fenced := map[string]bool{}
	for _, id := range ids {
		// Share the cleanup lock before allocating a sequence, in a fixed recipient order.
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "notification-account-subject:"+id); err != nil {
			return Acceptance{}, err
		}
		h := sha256.Sum256([]byte("notification-account-cleanup:" + id))
		var cleaned bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_cleanup_operations WHERE subject_ref=$1)`, hex.EncodeToString(h[:])).Scan(&cleaned); err != nil {
			return Acceptance{}, err
		}
		fenced[id] = cleaned
		if cleaned {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_recipient_state(recipient_account_id) VALUES($1) ON CONFLICT DO NOTHING`, id); err != nil {
			return Acceptance{}, err
		}
		var sequence int64
		if err = tx.QueryRowContext(ctx, `SELECT committed_sequence FROM inbox_recipient_state WHERE recipient_account_id=$1 FOR UPDATE`, id).Scan(&sequence); err != nil {
			return Acceptance{}, err
		}
	}
	result := Acceptance{Items: make([]AcceptanceItem, len(items))}
	for index, w := range items {
		result.Items[index].Index = index
		if fenced[w.RecipientAccountID] {
			result.Items[index].Status = "cleaned"
			continue
		}
		if !w.OccurredAt.Add(Retention).After(now) {
			result.Items[index].Status = "expired"
			continue
		}
		var oldType, oldID string
		var oldResource []byte
		err = tx.QueryRowContext(ctx, `SELECT id,event_type,resource FROM notification_inbox WHERE producer=$1 AND logical_notification_key=$2 AND recipient_account_id=$3`, producer, w.LogicalKey, w.RecipientAccountID).Scan(&oldID, &oldType, &oldResource)
		if err == nil {
			var r Resource
			if json.Unmarshal(oldResource, &r) != nil || r != w.Resource || oldType != w.Type {
				return Acceptance{}, ErrConflict
			}
			result.Items[index] = AcceptanceItem{Index: index, Status: "duplicate", ID: oldID}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, err
		}
		var seq int64
		if err = tx.QueryRowContext(ctx, `UPDATE inbox_recipient_state SET committed_sequence=committed_sequence+1 WHERE recipient_account_id=$1 RETURNING committed_sequence`, w.RecipientAccountID).Scan(&seq); err != nil {
			return Acceptance{}, err
		}
		resource, _ := json.Marshal(w.Resource)
		id := uuid.NewString()
		if _, err = tx.ExecContext(ctx, `INSERT INTO notification_inbox(id,recipient_account_id,recipient_sequence,producer,event_type,logical_notification_key,resource,event_version,occurred_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, w.RecipientAccountID, seq, producer, w.Type, w.LogicalKey, resource, w.EventVersion, w.OccurredAt, w.OccurredAt.Add(Retention)); err != nil {
			return Acceptance{}, err
		}
		result.Items[index] = AcceptanceItem{Index: index, Status: "accepted", ID: id}
	}
	if err := tx.Commit(); err != nil {
		return Acceptance{}, err
	}
	return result, nil
}
func (s *Store) snapshot(ctx context.Context, actor string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT committed_sequence FROM inbox_recipient_state WHERE recipient_account_id=$1`, actor).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}
func (s *Store) candidates(ctx context.Context, actor, filter string, seq int64, asOf time.Time, before time.Time, beforeID string, limit int) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,event_type,resource,occurred_at,read_at,recipient_sequence FROM notification_inbox WHERE recipient_account_id=$1 AND recipient_sequence<=$2 AND expires_at>$3 AND ($4<>'unread' OR read_at IS NULL) AND ($5='' OR (occurred_at,id)<($6::timestamptz,$5::uuid)) ORDER BY occurred_at DESC,id DESC LIMIT $7`, actor, seq, asOf, filter, beforeID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var item Item
		var raw []byte
		if err = rows.Scan(&item.ID, &item.Type, &raw, &item.OccurredAt, &item.ReadAt, &item.Sequence); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &item.Resource); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
func (s *Store) markRead(ctx context.Context, actor, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE notification_inbox SET read_at=COALESCE(read_at,$3) WHERE recipient_account_id=$1 AND id=$2 AND expires_at>$3`, actor, id, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}
func (s *Store) markAllRead(ctx context.Context, actor string, seq int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_inbox SET read_at=$3 WHERE recipient_account_id=$1 AND recipient_sequence<=$2 AND read_at IS NULL`, actor, seq, now)
	return err
}

// Export contains only the subject's retained references and read state.
func (s *Store) Export(ctx context.Context, actor string, afterSequence int64) (ExportPage, error) {
	if !validActor(actor) || afterSequence < 0 {
		return ExportPage{}, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,event_type,resource,occurred_at,expires_at,read_at,recipient_sequence FROM notification_inbox WHERE recipient_account_id=$1 AND recipient_sequence>$2 AND expires_at>clock_timestamp() ORDER BY recipient_sequence LIMIT 101`, actor, afterSequence)
	if err != nil {
		return ExportPage{}, err
	}
	defer rows.Close()
	page := ExportPage{Items: []ExportItem{}}
	var lastSequence int64
	for rows.Next() {
		var item ExportItem
		var raw []byte
		var sequence int64
		if err = rows.Scan(&item.ID, &item.Type, &raw, &item.OccurredAt, &item.ExpiresAt, &item.ReadAt, &sequence); err != nil {
			return ExportPage{}, err
		}
		if len(page.Items) == 100 {
			page.NextSequence = lastSequence
			break
		}
		if err = json.Unmarshal(raw, &item.Resource); err != nil {
			return ExportPage{}, err
		}
		page.Items = append(page.Items, item)
		lastSequence = sequence
	}
	return page, rows.Err()
}
