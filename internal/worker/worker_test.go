package worker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
)

var testKey = bytes.Repeat([]byte{1}, 32)

func TestWorkerDecryptsQueuedMessageWithRetainedKey(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	repository.claimResult.Claim.EncryptionKeyID = "v1"
	provider := &fakeProvider{receipt: providers.ProviderReceipt{Provider: "smtp"}}
	instance := newWorkerWithKeyring(repository, provider, map[string][]byte{
		"v1": testKey,
		"v2": bytes.Repeat([]byte{2}, 32),
	})

	if err := instance.Process(context.Background(), &fakeMessage{id: "delivery-1"}); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if provider.calls != 1 || repository.sentReceipt.Provider != "smtp" {
		t.Fatalf("provider calls=%d receipt=%#v", provider.calls, repository.sentReceipt)
	}
}

func TestWorkerReleasesLeaseWhenPersistedKeyIsMissing(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	repository.claimResult.Claim.EncryptionKeyID = "retired"
	provider := &fakeProvider{}
	message := &fakeMessage{id: "delivery-1"}

	err := newWorkerWithKeyring(
		repository,
		provider,
		map[string][]byte{"v2": bytes.Repeat([]byte{2}, 32)},
	).Process(context.Background(), message)
	if !errors.Is(err, notificationcrypto.ErrKeyNotConfigured) {
		t.Fatalf("Process() error = %v, want key configuration error", err)
	}
	if provider.calls != 0 || repository.releases != 1 ||
		repository.failedCode != "" || message.deadLettered != 0 {
		t.Fatalf(
			"provider=%d releases=%d failed=%q dead-letter=%d",
			provider.calls,
			repository.releases,
			repository.failedCode,
			message.deadLettered,
		)
	}
}

func TestAlreadySentCompletesWithoutProviderCall(t *testing.T) {
	repository := &fakeRepository{
		claimResult: claimResult{Status: statusSent},
	}
	provider := &fakeProvider{}
	message := &fakeMessage{id: "delivery-1"}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
	if message.completed != 1 || message.deadLettered != 0 {
		t.Fatalf("settlement complete=%d dead-letter=%d", message.completed, message.deadLettered)
	}
}

func TestProviderSuccessStoresReceiptBeforeCompletingBrokerMessage(t *testing.T) {
	events := []string{}
	repository := deliveryRepository(t, &events, 1)
	provider := &fakeProvider{
		events: &events,
		receipt: providers.ProviderReceipt{
			Provider:          "smtp",
			ProviderMessageID: "provider-1",
			AcceptedAt:        time.Unix(1_721_000_000, 0).UTC(),
		},
	}
	message := &fakeMessage{id: "delivery-1", events: &events}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	want := []string{"provider.send", "store.sent", "broker.complete"}
	if !equalStrings(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if repository.sentReceipt != provider.receipt {
		t.Fatalf("stored receipt = %#v, want %#v", repository.sentReceipt, provider.receipt)
	}
	if provider.payloads[0].MessageID != "<delivery-1@notification.alive.org.tw>" {
		t.Fatalf("Message-ID = %q", provider.payloads[0].MessageID)
	}
}

func TestProviderSuccessWithStoreFailureRedeliversWithStableMessageID(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	repository.transitionErr = errors.New("database unavailable")
	provider := &fakeProvider{receipt: providers.ProviderReceipt{Provider: "smtp"}}

	err := newWorker(repository, provider, testKey).Process(
		context.Background(),
		&fakeMessage{id: "delivery-1"},
	)
	if err == nil {
		t.Fatal("Process() error = nil")
	}
	if repository.releases != 1 {
		t.Fatalf("release calls = %d, want 1", repository.releases)
	}

	repository.transitionErr = nil
	if err := newWorker(repository, provider, testKey).Process(
		context.Background(),
		&fakeMessage{id: "delivery-1"},
	); err != nil {
		t.Fatalf("redelivery Process() error = %v", err)
	}
	if len(provider.payloads) != 2 ||
		provider.payloads[0].MessageID != provider.payloads[1].MessageID {
		t.Fatalf("provider payloads = %#v, want stable Message-ID", provider.payloads)
	}
}

func TestWorkerResolvesPersistedTemplateVersion(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	provider := &fakeProvider{receipt: providers.ProviderReceipt{Provider: "smtp"}}
	instance := newWorker(repository, provider, testKey)
	var resolvedVersion int
	instance.resolveTemplate = func(
		templateID string,
		version int,
		channel string,
	) (templates.Definition, error) {
		resolvedVersion = version
		return templates.ResolveVersion(templateID, version, channel)
	}

	if err := instance.Process(
		context.Background(),
		&fakeMessage{id: "delivery-1"},
	); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if resolvedVersion != 1 {
		t.Fatalf("resolved template version = %d, want persisted v1", resolvedVersion)
	}
}

func TestProviderSuccessFinishesAfterParentCancellation(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	ctx, cancel := context.WithCancel(context.Background())
	provider := &fakeProvider{send: func(context.Context) (providers.ProviderReceipt, error) {
		cancel()
		return providers.ProviderReceipt{Provider: "smtp"}, nil
	}}
	message := &fakeMessage{id: "delivery-1", rejectCanceledContext: true}

	if err := newWorker(repository, provider, testKey).Process(ctx, message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if repository.releases != 0 || message.completed != 1 {
		t.Fatalf("release=%d complete=%d", repository.releases, message.completed)
	}
}

func TestTemporaryFailureSchedulesRetryAndCompletesCurrentMessage(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	provider := &fakeProvider{
		err: &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "send"},
	}
	message := &fakeMessage{id: "delivery-1"}
	instance := newWorker(repository, provider, testKey)
	instance.retryDelay = func(int) time.Duration { return 3 * time.Minute }
	instance.newID = func() string { return "retry-outbox-1" }

	if err := instance.Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if repository.retryDelay != 3*time.Minute ||
		repository.retryCode != string(providers.ErrorTemporary) ||
		repository.retryOutboxID != "retry-outbox-1" {
		t.Fatalf(
			"retry delay=%s code=%q outbox=%q",
			repository.retryDelay,
			repository.retryCode,
			repository.retryOutboxID,
		)
	}
	if message.completed != 1 || message.deadLettered != 0 {
		t.Fatalf("settlement complete=%d dead-letter=%d", message.completed, message.deadLettered)
	}
}

func TestRateLimitedFailureUsesProviderRetryAfter(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	provider := &fakeProvider{
		err: &providers.ProviderError{
			Kind:       providers.ErrorRateLimited,
			Operation:  "send",
			RetryAfter: 17 * time.Minute,
		},
	}
	message := &fakeMessage{id: "delivery-1"}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if repository.retryDelay != 17*time.Minute ||
		repository.retryCode != string(providers.ErrorRateLimited) {
		t.Fatalf("retry delay=%s code=%q", repository.retryDelay, repository.retryCode)
	}
}

func TestAcceptanceUnknownSchedulesRetry(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	provider := &fakeProvider{
		err: &providers.ProviderError{
			Kind:      providers.ErrorAcceptanceUnknown,
			Operation: "data",
		},
	}
	message := &fakeMessage{id: "delivery-1"}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if repository.retryCode != string(providers.ErrorAcceptanceUnknown) {
		t.Fatalf("retry code = %q", repository.retryCode)
	}
}

func TestPermanentProviderErrorsFailAndDeadLetter(t *testing.T) {
	for _, kind := range []providers.ErrorKind{
		providers.ErrorPermanent,
		providers.ErrorInvalidEndpoint,
		providers.ErrorSuppressed,
	} {
		t.Run(string(kind), func(t *testing.T) {
			repository := deliveryRepository(t, nil, 1)
			provider := &fakeProvider{
				err: &providers.ProviderError{Kind: kind, Operation: "send"},
			}
			message := &fakeMessage{id: "delivery-1"}

			if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if repository.failedCode != string(kind) {
				t.Fatalf("failed code = %q, want %q", repository.failedCode, kind)
			}
			if message.deadLettered != 1 || message.deadLetterReason != string(kind) {
				t.Fatalf(
					"dead-letter count=%d reason=%q",
					message.deadLettered,
					message.deadLetterReason,
				)
			}
		})
	}
}

func TestRetryExhaustionMarksDeadLettered(t *testing.T) {
	repository := deliveryRepository(t, nil, maxAttempts)
	provider := &fakeProvider{
		err: &providers.ProviderError{Kind: providers.ErrorTemporary, Operation: "send"},
	}
	message := &fakeMessage{id: "delivery-1"}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if repository.deadLetterCode != string(providers.ErrorTemporary) {
		t.Fatalf("dead-letter code = %q", repository.deadLetterCode)
	}
	if message.deadLettered != 1 || message.completed != 0 {
		t.Fatalf("settlement complete=%d dead-letter=%d", message.completed, message.deadLettered)
	}
}

func TestCancellationReleasesLeaseWithoutSettling(t *testing.T) {
	repository := deliveryRepository(t, nil, 1)
	started := make(chan struct{})
	provider := &fakeProvider{send: func(ctx context.Context) (providers.ProviderReceipt, error) {
		close(started)
		<-ctx.Done()
		return providers.ProviderReceipt{}, ctx.Err()
	}}
	message := &fakeMessage{id: "delivery-1"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- newWorker(repository, provider, testKey).Process(ctx, message)
	}()
	<-started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Process() error = %v, want context canceled", err)
	}
	if repository.releases != 1 {
		t.Fatalf("release calls = %d, want 1", repository.releases)
	}
	if message.completed != 0 || message.deadLettered != 0 {
		t.Fatalf("settlement complete=%d dead-letter=%d", message.completed, message.deadLettered)
	}
}

func TestActiveLeaseDoesNotCallProvider(t *testing.T) {
	repository := &fakeRepository{
		claimResult: claimResult{Status: statusSending},
	}
	provider := &fakeProvider{}
	message := &fakeMessage{id: "delivery-1"}

	if err := newWorker(repository, provider, testKey).Process(context.Background(), message); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
	if message.completed != 1 {
		t.Fatalf("complete calls = %d, want 1", message.completed)
	}
}

func deliveryRepository(t *testing.T, events *[]string, attempt int) *fakeRepository {
	t.Helper()
	target, err := notificationcrypto.Encrypt(
		testKey,
		[]byte("message-1:target"),
		[]byte("user@example.com"),
	)
	if err != nil {
		t.Fatalf("encrypt target: %v", err)
	}
	payload, err := notificationcrypto.Encrypt(
		testKey,
		[]byte("message-1:payload"),
		[]byte(`{"locale":"zh-Hant","fields":{"verifyUrl":"https://account.alive.org.tw/verify-email?token=opaque"}}`),
	)
	if err != nil {
		t.Fatalf("encrypt payload: %v", err)
	}
	return &fakeRepository{
		events: events,
		claimResult: claimResult{
			Status:  statusSending,
			Claimed: true,
			Claim: claim{
				DeliveryID:        "delivery-1",
				MessageID:         "message-1",
				TemplateID:        "account.verify-email",
				TemplateVersion:   1,
				Channel:           "email",
				Attempt:           attempt,
				EncryptionKeyID:   "legacy-v1",
				TargetCiphertext:  target,
				PayloadCiphertext: payload,
			},
		},
	}
}

type fakeRepository struct {
	mu sync.Mutex

	claimResult claimResult
	claimErr    error
	events      *[]string

	sentReceipt    providers.ProviderReceipt
	retryDelay     time.Duration
	retryCode      string
	retryOutboxID  string
	failedCode     string
	deadLetterCode string
	releases       int
	transitionErr  error
}

func (r *fakeRepository) claim(context.Context, string, time.Duration) (claimResult, error) {
	return r.claimResult, r.claimErr
}

func (r *fakeRepository) markSent(
	ctx context.Context,
	_ claim,
	receipt providers.ProviderReceipt,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events != nil {
		*r.events = append(*r.events, "store.sent")
	}
	r.sentReceipt = receipt
	return r.transitionErr
}

func (r *fakeRepository) markRetry(
	_ context.Context,
	_ claim,
	code string,
	delay time.Duration,
	outboxID string,
) error {
	r.retryCode = code
	r.retryDelay = delay
	r.retryOutboxID = outboxID
	return r.transitionErr
}

func (r *fakeRepository) markFailed(_ context.Context, _ claim, code string) error {
	r.failedCode = code
	return r.transitionErr
}

func (r *fakeRepository) markDeadLettered(_ context.Context, _ claim, code string) error {
	r.deadLetterCode = code
	return r.transitionErr
}

func (r *fakeRepository) release(_ context.Context, _ claim) error {
	r.releases++
	return nil
}

type fakeProvider struct {
	calls    int
	events   *[]string
	payloads []providers.DeliveryPayload
	receipt  providers.ProviderReceipt
	err      error
	send     func(context.Context) (providers.ProviderReceipt, error)
}

func (p *fakeProvider) Send(ctx context.Context, payload providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	p.calls++
	p.payloads = append(p.payloads, payload)
	if p.events != nil {
		*p.events = append(*p.events, "provider.send")
	}
	if p.send != nil {
		return p.send(ctx)
	}
	return p.receipt, p.err
}

type fakeMessage struct {
	id                    string
	events                *[]string
	completed             int
	deadLettered          int
	deadLetterReason      string
	rejectCanceledContext bool
}

func (m *fakeMessage) DeliveryID() string {
	return m.id
}

func (m *fakeMessage) Complete(ctx context.Context) error {
	if m.rejectCanceledContext {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if m.events != nil {
		*m.events = append(*m.events, "broker.complete")
	}
	m.completed++
	return nil
}

func (m *fakeMessage) DeadLetter(_ context.Context, reason string) error {
	m.deadLettered++
	m.deadLetterReason = reason
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
