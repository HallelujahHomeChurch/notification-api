package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
	"github.com/google/uuid"
)

const (
	leaseDuration  = 60 * time.Second
	sendTimeout    = 45 * time.Second
	releaseTimeout = 5 * time.Second
	maxAttempts    = 5

	statusQueued       = string(contracts.DeliveryStatusQueued)
	statusSending      = string(contracts.DeliveryStatusSending)
	statusSent         = string(contracts.DeliveryStatusSent)
	statusFailed       = string(contracts.DeliveryStatusFailed)
	statusDeadLettered = string(contracts.DeliveryStatusDeadLettered)
	statusSuppressed   = string(contracts.DeliveryStatusSuppressed)
)

var ErrLeaseLost = errors.New("delivery lease lost")

type claim struct {
	DeliveryID             string
	MessageID              string
	TemplateID             string
	TemplateVersion        int
	Channel                string
	Attempt                int
	FailureCount           int
	ExpiresAt              time.Time
	EncryptionKeyID        string
	TargetCiphertext       []byte
	PayloadCiphertext      []byte
	EligibilityRef         *contracts.EligibilityRef
	EligibilityCampaignID  string
	EligibilityRecipientID string
}

type claimResult struct {
	Status  string
	Claimed bool
	Claim   claim
	session *deliverySession
}

type repository interface {
	claim(context.Context, string, time.Duration) (claimResult, error)
	markSent(context.Context, claim, providers.ProviderReceipt) error
	markRetry(context.Context, claim, string, time.Duration, string) error
	markFailed(context.Context, claim, string) error
	markDeadLettered(context.Context, claim, string) error
	markSuppressed(context.Context, claim, string) error
	release(context.Context, claim) error
}

type fencedClaimer interface {
	claimFenced(context.Context, string, time.Duration) (claimResult, func() error, error)
}

type Worker struct {
	email           *emailPolicy
	repository      repository
	providers       map[string]providers.Provider
	keys            map[string][]byte
	retryDelay      func(int) time.Duration
	newID           func() string
	resolveTemplate func(string, int, string) (templates.Definition, error)
	eligibility     interface {
		Check(context.Context, contracts.EligibilityRef) (bool, error)
	}
}

func (w *Worker) WithEligibilityChecker(checker interface {
	Check(context.Context, contracts.EligibilityRef) (bool, error)
}) *Worker {
	w.eligibility = checker
	return w
}

func New(db *sql.DB, provider providers.Provider, encryptionKey []byte) *Worker {
	return newWorker(postgresStore{db: db}, provider, encryptionKey)
}

func NewWithKeyring(db *sql.DB, provider providers.Provider, keys map[string][]byte) *Worker {
	return newWorkerWithKeyring(postgresStore{db: db}, provider, keys)
}

func NewWithProviders(db *sql.DB, configured map[string]providers.Provider, keys map[string][]byte) *Worker {
	return newWorkerWithProviders(postgresStore{db: db}, configured, keys)
}

func newWorker(repository repository, provider providers.Provider, encryptionKey []byte) *Worker {
	return newWorkerWithKeyring(
		repository,
		provider,
		map[string][]byte{"legacy-v1": encryptionKey},
	)
}

func newWorkerWithKeyring(
	repository repository,
	provider providers.Provider,
	keys map[string][]byte,
) *Worker {
	return newWorkerWithProviders(repository, map[string]providers.Provider{"email": provider}, keys)
}

func newWorkerWithProviders(
	repository repository,
	configured map[string]providers.Provider,
	keys map[string][]byte,
) *Worker {
	return &Worker{
		repository:      repository,
		providers:       configured,
		keys:            keys,
		retryDelay:      retryDelay,
		newID:           uuid.NewString,
		resolveTemplate: templates.ResolveVersion,
	}
}

func (w *Worker) Process(ctx context.Context, message queue.BrokerMessage) error {
	if err := w.validateEmailDatabase(); err != nil {
		return err
	}
	var (
		result       claimResult
		err          error
		releaseFence func() error
	)
	if fenced, ok := w.repository.(fencedClaimer); ok {
		result, releaseFence, err = fenced.claimFenced(ctx, message.DeliveryID(), leaseDuration)
	} else {
		result, err = w.repository.claim(ctx, message.DeliveryID(), leaseDuration)
	}
	if err != nil {
		return fmt.Errorf("claim delivery: %w", err)
	}
	if !result.Claimed {
		if result.Status == statusFailed || result.Status == statusDeadLettered || result.Status == "" {
			return message.DeadLetter(ctx, terminalReason(result.Status))
		}
		return message.Complete(ctx)
	}

	claimed := result.Claim
	if releaseFence != nil {
		fenceHeld := true
		release := func() error {
			if !fenceHeld {
				return nil
			}
			fenceHeld = false
			return releaseFence()
		}
		defer func() { _ = release() }()
		execution := w
		if result.session != nil {
			execution = result.session.bind(w)
		}
		return execution.processClaim(ctx, message, claimed, release, w.repository.release)
	}
	return w.processClaim(ctx, message, claimed, func() error { return nil }, w.repository.release)
}

func (w *Worker) processClaim(ctx context.Context, message queue.BrokerMessage, claimed claim, releaseFence func() error, releaseLease func(context.Context, claim) error) error {
	leaseHeld := true
	defer func() {
		if leaseHeld {
			_ = releaseFence()
			releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			_ = releaseLease(releaseCtx, claimed)
		}
	}()

	if claimed.Channel == "email" && w.email != nil {
		if err := w.email.observe(ctx, claimed); err != nil {
			return err
		}
	}
	payload, err := w.render(claimed)
	if err != nil {
		if errors.Is(err, notificationcrypto.ErrKeyNotConfigured) {
			return fmt.Errorf("render delivery: %w", err)
		}
		if transitionErr := w.repository.markFailed(ctx, claimed, "invalid_payload"); transitionErr != nil {
			return errors.Join(fmt.Errorf("render delivery: %w", err), transitionErr)
		}
		leaseHeld = false
		return message.DeadLetter(ctx, "invalid_payload")
	}

	if claimed.EligibilityRef != nil {
		if w.eligibility == nil {
			return errors.New("delivery eligibility checker is not configured")
		}
		eligibilityCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		allowed, eligibilityErr := w.eligibility.Check(eligibilityCtx, *claimed.EligibilityRef)
		cancel()
		if eligibilityErr != nil || !allowed {
			finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer finish()
			if eligibilityErr != nil {
				if claimed.FailureCount+1 < maxAttempts {
					if err := w.repository.markRetry(finishCtx, claimed, "eligibility_unavailable", w.retryDelay(claimed.Attempt), w.newID()); err != nil {
						return errors.Join(eligibilityErr, err)
					}
					leaseHeld = false
					return message.Complete(finishCtx)
				}
				if err := w.repository.markDeadLettered(finishCtx, claimed, "eligibility_unavailable"); err != nil {
					return errors.Join(eligibilityErr, err)
				}
				leaseHeld = false
				return message.DeadLetter(finishCtx, "eligibility_unavailable")
			} else if err := w.repository.markSuppressed(finishCtx, claimed, "eligibility_revoked"); err != nil {
				return err
			}
			leaseHeld = false
			return message.Complete(finishCtx)
		}
	}

	if claimed.Channel == "email" && w.email != nil {
		delay, gateErr := w.email.begin(ctx, claimed)
		// Short SES pacing stays in its isolated lane; exhausted windows still defer durably.
		if gateErr == nil && w.email.limits.PerSecond > 0 && delay > 0 && delay <= time.Second {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			delay, gateErr = w.email.begin(ctx, claimed)
		}
		if errors.Is(gateErr, errEmailExpired) {
			if err := w.repository.markDeadLettered(ctx, claimed, "expired"); err != nil {
				return err
			}
			leaseHeld = false
			return message.DeadLetter(ctx, "expired")
		}
		if errors.Is(gateErr, errEmailSuppressed) {
			if err := w.repository.markSuppressed(ctx, claimed, "technical_suppression"); err != nil {
				return err
			}
			leaseHeld = false
			return message.Complete(ctx)
		}
		if errors.Is(gateErr, errEmailProviderMismatch) {
			if err := w.repository.markDeadLettered(ctx, claimed, "provider_mismatch"); err != nil {
				return err
			}
			leaseHeld = false
			return message.DeadLetter(ctx, "provider_mismatch")
		}
		if errors.Is(gateErr, errEmailAlreadyAccepted) {
			receipt, err := w.email.acceptedReceipt(ctx, claimed)
			if err != nil {
				return err
			}
			if err = w.repository.markSent(ctx, claimed, receipt); err != nil {
				return err
			}
			leaseHeld = false
			return message.Complete(ctx)
		}
		if errors.Is(gateErr, errEmailAcceptanceUnknown) {
			if err := w.repository.markFailed(ctx, claimed, "acceptance_unknown"); err != nil {
				return err
			}
			leaseHeld = false
			return message.DeadLetter(ctx, "acceptance_unknown")
		}
		if gateErr != nil {
			return gateErr
		}
		if delay > 0 {
			if err := w.repository.markRetry(ctx, claimed, "rate_limited", delay, w.newID()); err != nil {
				return err
			}
			leaseHeld = false
			return message.Complete(ctx)
		}
	}
	deadline := time.Now().Add(sendTimeout)
	if !claimed.ExpiresAt.IsZero() && claimed.ExpiresAt.Before(deadline) {
		deadline = claimed.ExpiresAt
	}
	providerCtx, cancel := context.WithDeadline(ctx, deadline)
	provider, ok := w.providers[claimed.Channel]
	if !ok {
		cancel()
		return fmt.Errorf("provider for channel %q is not configured", claimed.Channel)
	}
	var receipt providers.ProviderReceipt
	providerErr := providerCtx.Err()
	expiredBeforeSend := providerErr != nil && !claimed.ExpiresAt.IsZero() && !claimed.ExpiresAt.After(time.Now())
	if providerErr == nil {
		receipt, providerErr = provider.Send(providerCtx, payload)
	} else {
		providerErr = &providers.ProviderError{Kind: providers.ErrorPermanent, Operation: "deadline"}
	}
	cancel()
	if claimed.Channel == "email" && w.email != nil {
		finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		recordErr := w.email.finish(finishCtx, claimed, receipt, providerErr)
		finish()
		if recordErr != nil {
			return fmt.Errorf("persist email attempt: %w", recordErr)
		}
	}
	if providerErr == nil {
		finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer finish()
		if err := w.repository.markSent(finishCtx, claimed, receipt); err != nil {
			return fmt.Errorf("mark delivery sent: %w", err)
		}
		leaseHeld = false
		return message.Complete(finishCtx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if expiredBeforeSend {
		if err := w.repository.markDeadLettered(ctx, claimed, "expired"); err != nil {
			return err
		}
		leaseHeld = false
		return message.DeadLetter(ctx, "expired")
	}

	kind, retryable, retryAfter := classifyProviderError(providerErr)
	if claimed.Channel == "email" && campaignEmail(claimed) && kind == providers.ErrorAcceptanceUnknown {
		if err := w.repository.markFailed(ctx, claimed, "acceptance_unknown"); err != nil {
			return err
		}
		leaseHeld = false
		return message.DeadLetter(ctx, "acceptance_unknown")
	}
	canRetry := claimed.FailureCount+1 < maxAttempts
	if claimed.Channel == "email" && kind == providers.ErrorRateLimited {
		canRetry = true
	}
	if retryable && canRetry {
		if retryAfter <= 0 {
			retryAfter = w.retryDelay(claimed.Attempt)
		}
		if err := w.repository.markRetry(
			ctx,
			claimed,
			string(kind),
			retryAfter,
			w.newID(),
		); err != nil {
			return fmt.Errorf("schedule delivery retry: %w", err)
		}
		leaseHeld = false
		return message.Complete(ctx)
	}

	if retryable {
		err = w.repository.markDeadLettered(ctx, claimed, string(kind))
	} else {
		err = w.repository.markFailed(ctx, claimed, string(kind))
	}
	if err != nil {
		return fmt.Errorf("mark delivery terminal: %w", err)
	}
	leaseHeld = false
	return message.DeadLetter(ctx, string(kind))
}

func (w *Worker) render(claimed claim) (providers.DeliveryPayload, error) {
	target, err := notificationcrypto.DecryptWithKeyID(
		w.keys,
		claimed.EncryptionKeyID,
		[]byte(claimed.MessageID+":target"),
		claimed.TargetCiphertext,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	payload, err := notificationcrypto.DecryptWithKeyID(
		w.keys,
		claimed.EncryptionKeyID,
		[]byte(claimed.MessageID+":payload"),
		claimed.PayloadCiphertext,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	var envelope struct {
		Locale string            `json:"locale"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return providers.DeliveryPayload{}, err
	}
	definition, err := w.resolveTemplate(
		claimed.TemplateID,
		claimed.TemplateVersion,
		claimed.Channel,
	)
	if err != nil {
		return providers.DeliveryPayload{}, err
	}
	switch claimed.Channel {
	case "email":
		email, err := templates.RenderEmail(definition, envelope.Locale, string(target), envelope.Fields)
		if err != nil {
			return providers.DeliveryPayload{}, err
		}
		return providers.DeliveryPayload{
			Recipient: email.To, Subject: email.Subject, Body: email.Body, HTMLBody: email.HTMLBody,
			DeliveryID:      claimed.DeliveryID,
			MessageID:       fmt.Sprintf("<%s@notification.alive.org.tw>", claimed.DeliveryID),
			ListUnsubscribe: email.ListUnsubscribe, OneClickUnsubscribe: email.OneClickUnsubscribe,
		}, nil
	case "web_push":
		push, err := templates.RenderWebPush(definition, envelope.Locale, string(target), envelope.Fields)
		if err != nil {
			return providers.DeliveryPayload{}, err
		}
		return providers.DeliveryPayload{
			Recipient: push.Target, Title: push.Title, Body: push.Body,
			ClickBehavior: push.ClickBehavior, ActionURL: push.ActionURL,
		}, nil
	default:
		return providers.DeliveryPayload{}, fmt.Errorf("unsupported delivery channel %q", claimed.Channel)
	}
}

func classifyProviderError(err error) (providers.ErrorKind, bool, time.Duration) {
	var providerErr *providers.ProviderError
	if !errors.As(err, &providerErr) {
		return providers.ErrorTemporary, true, 0
	}
	switch providerErr.Kind {
	case providers.ErrorTemporary, providers.ErrorRateLimited, providers.ErrorAcceptanceUnknown:
		return providerErr.Kind, true, providerErr.RetryAfter
	case providers.ErrorPermanent, providers.ErrorInvalidEndpoint, providers.ErrorSuppressed:
		return providerErr.Kind, false, 0
	default:
		return providers.ErrorPermanent, false, 0
	}
}

func retryDelay(attempt int) time.Duration {
	delay := time.Minute
	for current := 1; current < attempt && delay < time.Hour; current++ {
		delay *= 2
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func terminalReason(status string) string {
	if status == "" {
		return "delivery_not_found"
	}
	return status
}

var legacyCampaignKey = regexp.MustCompile(`^campaign:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}):(?:user|recipient):([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?::retry:[0-9]+)?$`)

// Older queued campaign messages predate explicit eligibility references.
// Only Engagement's fixed machine key can opt into the same private endpoint.
func legacyCampaignEligibility(caller, key string) *contracts.EligibilityRef {
	if caller != "engagement-api" {
		return nil
	}
	parts := legacyCampaignKey.FindStringSubmatch(key)
	if len(parts) != 3 {
		return nil
	}
	return &contracts.EligibilityRef{CampaignID: parts[1], RecipientID: parts[2]}
}
