package retention

import (
	"context"
	"testing"
	"time"
)

func TestRunOnceUsesRequiredRetentionWindowsAndBoundedBatch(t *testing.T) {
	repository := &fakeRepository{
		result: Result{Tombstoned: 2, MessagesDeleted: 1, RateBucketsDeleted: 3},
	}
	instance := newWorker(repository)
	instance.batchSize = 25

	result, err := instance.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result != repository.result {
		t.Fatalf("RunOnce() = %#v, want %#v", result, repository.result)
	}
	if repository.batchSize != 25 ||
		repository.payloadAge != 7*24*time.Hour ||
		repository.metadataAge != 730*24*time.Hour {
		t.Fatalf(
			"cleanup batch=%d payload=%s metadata=%s",
			repository.batchSize,
			repository.payloadAge,
			repository.metadataAge,
		)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	repository := &fakeRepository{}
	instance := newWorker(repository)
	instance.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := instance.Run(ctx); err != context.Canceled {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestRunDrainsEligibleBatchesBeforeWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	repository := &fakeRepository{}
	repository.retainFn = func() (Result, error) {
		repository.calls++
		if repository.calls == 1 {
			return Result{Tombstoned: 1}, nil
		}
		cancel()
		return Result{}, nil
	}
	instance := newWorker(repository)
	instance.interval = time.Hour

	if err := instance.Run(ctx); err != context.Canceled {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if repository.calls != 2 {
		t.Fatalf("retain calls = %d, want 2 before waiting", repository.calls)
	}
}

type fakeRepository struct {
	result      Result
	err         error
	retainFn    func() (Result, error)
	calls       int
	batchSize   int
	payloadAge  time.Duration
	metadataAge time.Duration
}

func (r *fakeRepository) retain(
	_ context.Context,
	batchSize int,
	payloadAge time.Duration,
	metadataAge time.Duration,
) (Result, error) {
	r.batchSize = batchSize
	r.payloadAge = payloadAge
	r.metadataAge = metadataAge
	if r.retainFn != nil {
		return r.retainFn()
	}
	return r.result, r.err
}
