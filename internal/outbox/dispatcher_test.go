package outbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	claim       claim
	claimOK     bool
	claimErr    error
	claimScript []claimResult
	claimCalls  int
	published   []claim
	publishErrs []error
	retried     []claim
	retryDelays []time.Duration
}

type claimResult struct {
	claim claim
	ok    bool
	err   error
}

func (s *fakeStore) claimNext(context.Context, time.Duration) (claim, bool, error) {
	s.claimCalls++
	if len(s.claimScript) > 0 {
		result := s.claimScript[0]
		s.claimScript = s.claimScript[1:]
		return result.claim, result.ok, result.err
	}
	return s.claim, s.claimOK, s.claimErr
}

func (s *fakeStore) markPublished(_ context.Context, claimed claim) error {
	s.published = append(s.published, claimed)
	if len(s.publishErrs) > 0 {
		err := s.publishErrs[0]
		s.publishErrs = s.publishErrs[1:]
		return err
	}
	return nil
}

func (s *fakeStore) markRetry(_ context.Context, claimed claim, delay time.Duration) error {
	s.retried = append(s.retried, claimed)
	s.retryDelays = append(s.retryDelays, delay)
	return nil
}

type publisherFunc func(context.Context, string, string) error

func (fn publisherFunc) Publish(ctx context.Context, outboxID, deliveryID string) error {
	return fn(ctx, outboxID, deliveryID)
}

func TestDispatchMarksPublishedOnlyAfterBrokerAcknowledgement(t *testing.T) {
	store := &fakeStore{
		claim:   claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 1},
		claimOK: true,
	}
	publisher := publisherFunc(func(_ context.Context, outboxID, deliveryID string) error {
		if outboxID != "outbox-1" || deliveryID != "delivery-1" {
			t.Fatalf("outbox ID=%q delivery ID=%q", outboxID, deliveryID)
		}
		if len(store.published) != 0 {
			t.Fatal("outbox marked published before broker acknowledgement")
		}
		return nil
	})

	processed, err := newDispatcher(store, publisher).DispatchOne(context.Background())
	if err != nil {
		t.Fatalf("DispatchOne() error = %v", err)
	}
	if !processed || len(store.published) != 1 || len(store.retried) != 0 {
		t.Fatalf("processed=%v published=%d retried=%d", processed, len(store.published), len(store.retried))
	}
}

func TestDispatchFailureSchedulesBoundedJitteredBackoff(t *testing.T) {
	store := &fakeStore{
		claim:   claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 20},
		claimOK: true,
	}
	brokerErr := errors.New("broker unavailable")
	dispatcher := newDispatcher(store, publisherFunc(func(context.Context, string, string) error {
		return brokerErr
	}))
	dispatcher.random = func(int64) int64 { return 0 }

	processed, err := dispatcher.DispatchOne(context.Background())
	if !processed || !errors.Is(err, brokerErr) {
		t.Fatalf("DispatchOne() processed=%v error=%v", processed, err)
	}
	if len(store.published) != 0 || len(store.retried) != 1 {
		t.Fatalf("published=%d retried=%d", len(store.published), len(store.retried))
	}
	if got, want := store.retryDelays[0], 30*time.Second; got != want {
		t.Fatalf("retry delay = %v, want %v", got, want)
	}
}

func TestDispatchCancelsBlockedPublishBeforeLeaseExpires(t *testing.T) {
	if publishTimeout != 45*time.Second || publishTimeout >= leaseDuration {
		t.Fatalf("publish timeout = %v, lease = %v", publishTimeout, leaseDuration)
	}
	store := &fakeStore{
		claim:   claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 1},
		claimOK: true,
	}
	dispatcher := newDispatcher(store, publisherFunc(func(ctx context.Context, _, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("publisher context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 50*time.Millisecond {
			t.Fatalf("publisher deadline remaining = %v", remaining)
		}
		<-ctx.Done()
		return ctx.Err()
	}))
	dispatcher.publishTimeout = 25 * time.Millisecond

	processed, err := dispatcher.DispatchOne(context.Background())
	if !processed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DispatchOne() processed=%v error=%v", processed, err)
	}
	if len(store.published) != 0 || len(store.retried) != 1 {
		t.Fatalf("published=%d retried=%d", len(store.published), len(store.retried))
	}
}

func TestRunReturnsAfterPersistentDependencyFailures(t *testing.T) {
	store := &fakeStore{
		claim:   claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 1},
		claimOK: true,
	}
	dispatcher := newDispatcher(store, publisherFunc(func(context.Context, string, string) error {
		return errors.New("broker unavailable")
	}))
	dispatcher.maxConsecutiveFailures = 3
	dispatcher.wait = func(context.Context, time.Duration) error { return nil }

	err := dispatcher.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persistent outbox dependency failure") {
		t.Fatalf("Run() error = %v", err)
	}
	if store.claimCalls != 3 {
		t.Fatalf("claim calls = %d, want 3", store.claimCalls)
	}
}

func TestDispatchReturnsClaimFailureWithoutPublishing(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	store := &fakeStore{claimErr: databaseErr}
	published := false
	dispatcher := newDispatcher(store, publisherFunc(func(context.Context, string, string) error {
		published = true
		return nil
	}))

	processed, err := dispatcher.DispatchOne(context.Background())
	if processed || !errors.Is(err, databaseErr) || published {
		t.Fatalf("DispatchOne() processed=%v error=%v published=%v", processed, err, published)
	}
}

func TestRunResetsDatabaseFailuresAfterHealthyIdleCheck(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	stopErr := errors.New("stop test")
	store := &fakeStore{claimScript: []claimResult{
		{err: databaseErr},
		{},
		{err: databaseErr},
		{},
	}}
	dispatcher := newDispatcher(store, publisherFunc(func(context.Context, string, string) error {
		t.Fatal("publisher called without a claim")
		return nil
	}))
	dispatcher.maxConsecutiveFailures = 2
	waits := 0
	dispatcher.wait = func(context.Context, time.Duration) error {
		waits++
		if waits == 4 {
			return stopErr
		}
		return nil
	}

	if err := dispatcher.Run(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("Run() error = %v, want healthy idle checks to reset database failure count", err)
	}
}

func TestRunKeepsCompletionWriteFailuresAcrossIdlePolls(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	stopErr := errors.New("stop test")
	store := &fakeStore{
		claimScript: []claimResult{
			{claim: claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 1}, ok: true},
			{},
			{claim: claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 2}, ok: true},
			{},
			{claim: claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 3}, ok: true},
			{},
		},
		publishErrs: []error{databaseErr, databaseErr, databaseErr},
	}
	dispatcher := newDispatcher(store, publisherFunc(func(context.Context, string, string) error {
		return nil
	}))
	dispatcher.maxConsecutiveFailures = 3
	waits := 0
	dispatcher.wait = func(context.Context, time.Duration) error {
		waits++
		if waits == 6 {
			return stopErr
		}
		return nil
	}

	err := dispatcher.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persistent outbox dependency failure") {
		t.Fatalf("Run() error = %v, want persistent completion-write failure", err)
	}
	if len(store.published) != 3 {
		t.Fatalf("markPublished calls = %d, want 3", len(store.published))
	}
}

func TestDispatchKeepsTransportIDStableForRowRetriesAndDistinctForApplicationRetry(t *testing.T) {
	store := &fakeStore{claimScript: []claimResult{
		{claim: claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 1}, ok: true},
		{claim: claim{OutboxID: "outbox-1", DeliveryID: "delivery-1", Attempt: 2}, ok: true},
		{claim: claim{OutboxID: "outbox-2", DeliveryID: "delivery-1", Attempt: 1}, ok: true},
	}}
	var transportIDs []string
	brokerErr := errors.New("transport retry")
	dispatcher := newDispatcher(store, publisherFunc(func(
		_ context.Context,
		outboxID string,
		deliveryID string,
	) error {
		if deliveryID != "delivery-1" {
			t.Fatalf("delivery ID = %q", deliveryID)
		}
		transportIDs = append(transportIDs, outboxID)
		if len(transportIDs) == 1 {
			return brokerErr
		}
		return nil
	}))

	for attempt := range 3 {
		processed, err := dispatcher.DispatchOne(context.Background())
		if !processed {
			t.Fatalf("DispatchOne() processed=%v error=%v", processed, err)
		}
		if attempt == 0 && !errors.Is(err, brokerErr) {
			t.Fatalf("first DispatchOne() error = %v, want transport retry", err)
		}
		if attempt > 0 && err != nil {
			t.Fatalf("DispatchOne() attempt %d error = %v", attempt+1, err)
		}
	}
	want := []string{"outbox-1", "outbox-1", "outbox-2"}
	for index := range want {
		if transportIDs[index] != want[index] {
			t.Fatalf("transport IDs = %#v, want %#v", transportIDs, want)
		}
	}
}
