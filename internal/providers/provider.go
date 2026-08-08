package providers

import (
	"context"
	"fmt"
	"time"
)

type ErrorKind string

const (
	ErrorTemporary         ErrorKind = "temporary"
	ErrorPermanent         ErrorKind = "permanent"
	ErrorInvalidEndpoint   ErrorKind = "invalid_endpoint"
	ErrorSuppressed        ErrorKind = "suppressed"
	ErrorRateLimited       ErrorKind = "rate_limited"
	ErrorAcceptanceUnknown ErrorKind = "acceptance_unknown"
)

type DeliveryPayload struct {
	Recipient           string
	Title               string
	Subject             string
	Body                string
	ClickBehavior       string
	ActionURL           string
	HTMLBody            string
	MessageID           string
	ListUnsubscribe     string
	OneClickUnsubscribe bool
}

type ProviderReceipt struct {
	Provider          string
	ProviderMessageID string
	AcceptedAt        time.Time
}

type ProviderError struct {
	Kind              ErrorKind
	Operation         string
	RetryAfter        time.Duration
	HTTPStatus        int
	ProviderRequestID string
	ProviderFamily    string
	ProviderReason    string
	cause             error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider %s failed: %s", e.Operation, e.Kind)
}

func (e *ProviderError) Unwrap() error {
	return e.cause
}

func (e *ProviderError) Retryable() bool {
	return e.Kind == ErrorTemporary ||
		e.Kind == ErrorRateLimited ||
		e.Kind == ErrorAcceptanceUnknown
}

type Provider interface {
	Send(context.Context, DeliveryPayload) (ProviderReceipt, error)
}
