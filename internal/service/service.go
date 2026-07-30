package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	notificationcrypto "github.com/HallelujahHomeChurch/notification-api/internal/crypto"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/HallelujahHomeChurch/notification-api/internal/templates"
	"github.com/google/uuid"
)

var (
	ErrInvalidRequest        = errors.New("invalid request")
	ErrForbiddenTemplate     = errors.New("forbidden template")
	ErrIdempotencyConflict   = errors.New("idempotency conflict")
	ErrRateLimited           = errors.New("rate limited")
	ErrNotFound              = errors.New("not found")
	ErrNotificationsDisabled = errors.New("notifications disabled")
)

type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s; retry after %s", ErrRateLimited, e.RetryAfter)
}

func (e *RateLimitError) Unwrap() error {
	return ErrRateLimited
}

type Config struct {
	ActiveEncryptionKeyID string
	EncryptionKeys        map[string][]byte
	ActiveHashKeyID       string
	HashKeys              map[string][]byte
	DataEncryptionKey     []byte
	HashKey               []byte
	NotificationsDisabled bool
	RateLimits            []store.RateLimit
}

type Result struct {
	MessageID       string
	Status          contracts.MessageStatus
	TemplateVersion int
	Replayed        bool
}

type repository interface {
	Create(context.Context, store.CreateParams) (store.CreateResult, error)
	Get(context.Context, string, string) (store.Message, error)
}

type Service struct {
	repository repository
	config     Config
}

func New(repository repository, config Config) *Service {
	if len(config.EncryptionKeys) == 0 && len(config.DataEncryptionKey) > 0 {
		config.ActiveEncryptionKeyID = "legacy-v1"
		config.EncryptionKeys = map[string][]byte{"legacy-v1": config.DataEncryptionKey}
	}
	if len(config.HashKeys) == 0 && len(config.HashKey) > 0 {
		config.ActiveHashKeyID = "legacy-v1"
		config.HashKeys = map[string][]byte{"legacy-v1": config.HashKey}
	}
	return &Service{repository: repository, config: config}
}

func (s *Service) Send(
	ctx context.Context,
	caller string,
	idempotencyKey string,
	request contracts.SendRequest,
) (Result, error) {
	if s.config.NotificationsDisabled {
		return Result{}, ErrNotificationsDisabled
	}
	if caller == "" || !validIdempotencyKey(idempotencyKey) {
		return Result{}, ErrInvalidRequest
	}

	definition, err := templates.Resolve(request.TemplateID, request.Channel)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if request.Target.Type != "email" || definition.Channel != "email" {
		return Result{}, ErrInvalidRequest
	}
	target, err := normalizeEmail(request.Target.Address)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Resource.Type) == "" || strings.TrimSpace(request.Resource.ID) == "" {
		return Result{}, ErrInvalidRequest
	}
	if !definition.SupportedLocale[request.Locale] {
		request.Locale = "en"
	}
	request.Target.Address = target
	validatedPayload, err := templates.Validate(definition, caller, request)
	if err != nil {
		if errors.Is(err, templates.ErrForbiddenCaller) {
			return Result{}, fmt.Errorf("%w: %v", ErrForbiddenTemplate, err)
		}
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	request.Payload = validatedPayload

	canonical, err := canonicalRequest(request, definition.Version)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize notification request: %w", err)
	}
	payload, err := json.Marshal(struct {
		Locale string            `json:"locale"`
		Fields map[string]string `json:"fields"`
	}{
		Locale: request.Locale,
		Fields: validatedPayload,
	})
	if err != nil {
		return Result{}, fmt.Errorf("encode notification payload: %w", err)
	}

	messageID := uuid.NewString()
	targetCiphertext, err := notificationcrypto.EncryptWithKeyID(
		s.config.EncryptionKeys,
		s.config.ActiveEncryptionKeyID,
		[]byte(messageID+":target"),
		[]byte(target),
	)
	if err != nil {
		return Result{}, fmt.Errorf("encrypt notification target: %w", err)
	}
	payloadCiphertext, err := notificationcrypto.EncryptWithKeyID(
		s.config.EncryptionKeys,
		s.config.ActiveEncryptionKeyID,
		[]byte(messageID+":payload"),
		payload,
	)
	if err != nil {
		return Result{}, fmt.Errorf("encrypt notification payload: %w", err)
	}

	requestHashes := hashIdentities(s.config.HashKeys, canonical)
	targetHashes := hashIdentities(s.config.HashKeys, []byte(target))
	requestHash, requestHashOK := requestHashes[s.config.ActiveHashKeyID]
	targetHash, targetHashOK := targetHashes[s.config.ActiveHashKeyID]
	if !requestHashOK || !targetHashOK {
		return Result{}, fmt.Errorf("active notification hash key is not configured")
	}

	created, err := s.repository.Create(ctx, store.CreateParams{
		MessageID:         messageID,
		DeliveryID:        uuid.NewString(),
		OutboxID:          uuid.NewString(),
		Caller:            caller,
		IdempotencyKey:    idempotencyKey,
		RequestHash:       requestHash,
		RequestHashes:     requestHashes,
		EncryptionKeyID:   s.config.ActiveEncryptionKeyID,
		HashKeyID:         s.config.ActiveHashKeyID,
		TemplateID:        definition.ID,
		TemplateVersion:   definition.Version,
		Channel:           definition.Channel,
		TargetType:        request.Target.Type,
		TargetHash:        targetHash,
		TargetHashes:      targetHashes,
		TargetCiphertext:  targetCiphertext,
		PayloadCiphertext: payloadCiphertext,
		ResourceType:      request.Resource.Type,
		ResourceID:        request.Resource.ID,
		Provider:          "smtp",
		RateLimits:        s.config.RateLimits,
		ExpiresAfter:      definition.TTL,
	})
	if err != nil {
		return Result{}, err
	}
	switch {
	case created.Conflict:
		return Result{}, ErrIdempotencyConflict
	case created.RetryAfter > 0:
		return Result{}, &RateLimitError{RetryAfter: created.RetryAfter}
	default:
		return resultFromMessage(created.Message, created.Replayed), nil
	}
}

func hashIdentities(keys map[string][]byte, value []byte) map[string]string {
	hashes := make(map[string]string, len(keys))
	for keyID, key := range keys {
		hashes[keyID] = notificationcrypto.Hash(key, value)
	}
	return hashes
}

func (s *Service) Get(ctx context.Context, caller, messageID string) (Result, error) {
	if caller == "" || messageID == "" {
		return Result{}, ErrNotFound
	}
	message, err := s.repository.Get(ctx, caller, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	return resultFromMessage(message, false), nil
}

func canonicalRequest(request contracts.SendRequest, templateVersion int) ([]byte, error) {
	return json.Marshal(struct {
		TemplateID      string             `json:"templateId"`
		TemplateVersion int                `json:"templateVersion"`
		Channel         string             `json:"channel"`
		Target          contracts.Target   `json:"target"`
		Locale          string             `json:"locale"`
		Payload         map[string]string  `json:"payload"`
		Resource        contracts.Resource `json:"resource"`
	}{
		TemplateID:      request.TemplateID,
		TemplateVersion: templateVersion,
		Channel:         request.Channel,
		Target:          request.Target,
		Locale:          request.Locale,
		Payload:         request.Payload,
		Resource:        request.Resource,
	})
}

func normalizeEmail(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Address != normalized {
		return "", fmt.Errorf("%w: invalid email", ErrInvalidRequest)
	}
	return normalized, nil
}

func validIdempotencyKey(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func resultFromMessage(message store.Message, replayed bool) Result {
	return Result{
		MessageID:       message.ID,
		Status:          message.Status,
		TemplateVersion: message.TemplateVersion,
		Replayed:        replayed,
	}
}
