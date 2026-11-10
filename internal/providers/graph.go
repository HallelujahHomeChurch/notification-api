package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/google/uuid"
)

const graphOrigin = "https://graph.microsoft.com"

type GraphConfig struct {
	SenderUserID            string
	ManagedIdentityClientID string
	From                    string
	FromName                string
	Logger                  *log.Logger
}

type Graph struct {
	config   GraphConfig
	identity azcore.TokenCredential
	client   *http.Client
}

func ValidateGraphConfig(config GraphConfig) error {
	if !graphUUID(config.SenderUserID) || !graphUUID(config.ManagedIdentityClientID) {
		return errors.New("Graph requires a sender mailbox ID and a named managed identity client ID")
	}
	if !plainAddress(config.From) || strings.ContainsAny(config.FromName, "\r\n") {
		return errors.New("invalid Graph sender")
	}
	return nil
}

func NewGraph(config GraphConfig) (*Graph, error) {
	if err := ValidateGraphConfig(config); err != nil {
		return nil, err
	}
	identity, err := azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(config.ManagedIdentityClientID)})
	if err != nil {
		return nil, errors.New("cannot initialize Graph managed identity")
	}
	return &Graph{
		config:   config,
		identity: identity,
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (*Graph) Family() string { return "graph" }

func (g *Graph) SenderMailboxID() string { return g.config.SenderUserID }

// Prepare completes MIME and token work before the worker reserves a send permit.
func (g *Graph) Prepare(ctx context.Context, payload DeliveryPayload) (func(context.Context) (ProviderReceipt, error), error) {
	if err := validateEmailPayload(payload); err != nil {
		return nil, g.failure(ErrorInvalidEndpoint, "validate", 0, "", 0)
	}
	var mime bytes.Buffer
	if err := writeEmailMIME(&mime, g.config.From, g.config.FromName, payload); err != nil {
		return nil, g.failure(ErrorInvalidEndpoint, "message", 0, "", 0)
	}
	access, err := g.identity.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{graphOrigin + "/.default"}})
	if err != nil || access.Token == "" {
		return nil, g.failure(ErrorTemporary, "auth", 0, "", 0)
	}
	encoded := base64.StdEncoding.EncodeToString(mime.Bytes())
	req, err := http.NewRequest(http.MethodPost, graphOrigin+"/v1.0/users/"+g.config.SenderUserID+"/sendMail", strings.NewReader(encoded))
	if err != nil {
		return nil, g.failure(ErrorPermanent, "validate", 0, "", 0)
	}
	req.Header.Set("Authorization", "Bearer "+access.Token)
	req.Header.Set("Content-Type", "text/plain")
	req.GetBody = nil
	var used atomic.Bool
	return func(ctx context.Context) (ProviderReceipt, error) {
		if !used.CompareAndSwap(false, true) {
			return ProviderReceipt{}, g.failure(ErrorPermanent, "data", 0, "", 0)
		}
		return g.submit(ctx, req)
	}, nil
}

func (g *Graph) Send(ctx context.Context, payload DeliveryPayload) (ProviderReceipt, error) {
	prepared, err := g.Prepare(ctx, payload)
	if err != nil {
		return ProviderReceipt{}, err
	}
	return prepared(ctx)
}

func (g *Graph) submit(ctx context.Context, request *http.Request) (ProviderReceipt, error) {
	response, err := g.client.Do(request.WithContext(ctx))
	if err != nil {
		return ProviderReceipt{}, g.failure(ErrorAcceptanceUnknown, "data", 0, "", 0)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("request-id")
	if !graphUUID(requestID) {
		requestID = ""
	}
	switch response.StatusCode {
	case http.StatusAccepted:
		if g.config.Logger != nil {
			g.config.Logger.Print("event=notification_provider_success provider=graph")
		}
		return ProviderReceipt{Provider: "graph", ProviderRequestID: requestID, AcceptedAt: time.Now().UTC()}, nil
	case http.StatusTooManyRequests:
		return ProviderReceipt{}, g.failure(ErrorRateLimited, "data", response.StatusCode, requestID, parseRetryAfter(response.Header.Get("Retry-After")))
	case http.StatusUnauthorized:
		return ProviderReceipt{}, g.failure(ErrorTemporary, "auth", response.StatusCode, requestID, 0)
	case http.StatusForbidden:
		return ProviderReceipt{}, g.failure(ErrorPermanent, "auth", response.StatusCode, requestID, 0)
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return ProviderReceipt{}, g.failure(ErrorPermanent, "data", response.StatusCode, requestID, 0)
	default:
		return ProviderReceipt{}, g.failure(ErrorAcceptanceUnknown, "data", response.StatusCode, requestID, 0)
	}
}

func (g *Graph) failure(kind ErrorKind, operation string, status int, requestID string, retryAfter time.Duration) error {
	if g.config.Logger != nil {
		g.config.Logger.Printf("event=notification_provider_failure provider=graph kind=%s operation=%s http_status=%d", kind, operation, status)
	}
	return &ProviderError{
		Kind: kind, Operation: operation, ProviderFamily: "graph", HTTPStatus: status,
		ProviderRequestID: requestID, RetryAfter: retryAfter,
	}
}

func graphUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
