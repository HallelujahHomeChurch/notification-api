package inbox

import (
	"context"
	"errors"
	"time"
)

type Service struct {
	store   *Store
	checker VisibilityChecker
	tokens  tokens
}

func NewService(s *Store, c VisibilityChecker, keyID string, keys map[string][]byte) *Service {
	return &Service{s, c, newTokens(keyID, keys)}
}
func (s *Service) readToken(actor string, seq int64, now time.Time) string {
	return s.tokens.sign(token{Actor: actor, Purpose: "read", Sequence: seq, IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix()})
}
func (s *Service) projections(ctx context.Context, actor, locale string, items []Item) (map[string]Projection, error) {
	refs := make([]Ref, len(items))
	for i, item := range items {
		refs[i] = Ref{ID: item.ID, Resource: item.Resource}
	}
	if len(refs) == 0 {
		return map[string]Projection{}, nil
	}
	if s.checker == nil {
		return nil, ErrUnavailable
	}
	projected, err := s.checker.Check(ctx, actor, locale, refs)
	if err != nil {
		return nil, ErrUnavailable
	}
	known := map[string]Resource{}
	for _, ref := range refs {
		known[ref.ID] = ref.Resource
	}
	for id, p := range projected {
		r, ok := known[id]
		if !ok || !validProjection(p, r) {
			return nil, ErrUnavailable
		}
	}
	return projected, nil
}
func (s *Service) pageAt(ctx context.Context, actor, filter, cursor, locale string, limit int, seq int64, now time.Time) (Page, error) {
	result := Page{Items: []Item{}, AsOf: now, ReadSnapshotToken: s.readToken(actor, seq, now)}
	var before time.Time
	beforeID := ""
	if cursor != "" {
		v, err := s.tokens.verify(cursor, actor, "cursor", now)
		if err != nil || (v.Filter != filter || v.Locale != locale) {
			return Page{}, ErrInvalid
		}
		seq = v.Sequence
		before = v.BeforeTime
		beforeID = v.BeforeID
		result.ReadSnapshotToken = s.readToken(actor, seq, now)
	}
	scanned := 0
	more := false
	for scanned < 500 {
		candidates, err := s.store.candidates(ctx, actor, filter, seq, now, before, beforeID, 100)
		if err != nil {
			return Page{}, ErrUnavailable
		}
		projections, err := s.projections(ctx, actor, locale, candidates)
		if err != nil {
			return Page{}, err
		}
		for index, item := range candidates {
			scanned++
			before = item.OccurredAt
			beforeID = item.ID
			if p, visible := projections[item.ID]; visible {
				item.Projection = p
				result.Items = append(result.Items, item)
			}
			if len(result.Items) == limit {
				more = index < len(candidates)-1 || len(candidates) == 100
				break
			}
		}
		if len(result.Items) == limit {
			break
		}
		if len(candidates) < 100 {
			more = false
			break
		}
		more = true
	}
	if more {
		result.NextCursor = s.tokens.sign(token{Actor: actor, Purpose: "cursor", Locale: locale, Filter: filter, Sequence: seq, BeforeTime: before, BeforeID: beforeID, IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix()})
	}
	return result, nil
}
func (s *Service) Page(ctx context.Context, actor, filter, cursor string, limit int, locale string) (Page, error) {
	if !validLocale(locale) || !validActor(actor) || (filter != "all" && filter != "unread") || limit < 1 || limit > 50 {
		return Page{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	seq, err := s.store.snapshot(ctx, actor)
	if err != nil {
		return Page{}, ErrUnavailable
	}
	return s.pageAt(ctx, actor, filter, cursor, locale, limit, seq, time.Now().UTC())
}
func (s *Service) Summary(ctx context.Context, actor, locale string) (Summary, error) {
	if !validActor(actor) || !validLocale(locale) {
		return Summary{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	seq, err := s.store.snapshot(ctx, actor)
	if err != nil {
		return Summary{}, ErrUnavailable
	}
	page, err := s.pageAt(ctx, actor, "all", "", locale, 5, seq, now)
	if err != nil {
		return Summary{}, err
	}
	// A preview has no cursor field, so continue bounded pages until its latest five are known.
	for len(page.Items) < 5 && page.NextCursor != "" {
		next, err := s.pageAt(ctx, actor, "all", page.NextCursor, locale, 5-len(page.Items), seq, now)
		if err != nil {
			return Summary{}, err
		}
		page.Items = append(page.Items, next.Items...)
		page.NextCursor = next.NextCursor
	}
	// Exact count has its own deadline; full-page reads never depend on this scan.
	countCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	var count int64
	var before time.Time
	beforeID := ""
	for {
		if countCtx.Err() != nil {
			return Summary{}, ErrUnavailable
		}
		items, err := s.store.candidates(countCtx, actor, "unread", seq, now, before, beforeID, 100)
		if err != nil {
			return Summary{}, ErrUnavailable
		}
		projections, err := s.projections(countCtx, actor, locale, items)
		if err != nil {
			return Summary{}, err
		}
		count += int64(len(projections))
		if len(items) < 100 {
			break
		}
		last := items[len(items)-1]
		before = last.OccurredAt
		beforeID = last.ID
	}
	return Summary{Items: page.Items, UnreadCount: count, ReadSnapshotToken: page.ReadSnapshotToken, AsOf: now}, nil
}
func (s *Service) MarkRead(ctx context.Context, actor, id string) error {
	if !validActor(actor) || !validActor(id) {
		return ErrInvalid
	}
	err := s.store.markRead(ctx, actor, id, time.Now().UTC())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ErrUnavailable
	}
	return err
}
func (s *Service) MarkAllRead(ctx context.Context, actor, raw string) error {
	now := time.Now().UTC()
	v, err := s.tokens.verify(raw, actor, "read", now)
	if err != nil {
		return err
	}
	if err = s.store.markAllRead(ctx, actor, v.Sequence, now); err != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) AcceptBatch(ctx context.Context, producer string, writes []Write) (Acceptance, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.store.AcceptBatch(ctx, producer, writes, time.Now().UTC())
	if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
		return Acceptance{}, ErrUnavailable
	}
	return result, err
}
