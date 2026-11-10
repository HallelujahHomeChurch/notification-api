package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const fixtureGraphSender = "e91a5b3b-37a7-481e-ab6f-15e3185f5cf5"

type graphFixtureTransport func(*http.Request) (*http.Response, error)

func (f graphFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureGraph(t *testing.T, send graphFixtureTransport) *Graph {
	t.Helper()
	g, err := NewGraph(GraphConfig{
		SenderUserID: fixtureGraphSender, ManagedIdentityClientID: "5a2625bc-6208-4133-a58e-e219817b232c",
		From: "sender@example.test", FromName: "寄件者",
	})
	if err != nil {
		t.Fatal(err)
	}
	g.identity = tokenFunc(func(_ context.Context, p policy.TokenRequestOptions) (azcore.AccessToken, error) {
		if len(p.Scopes) != 1 || p.Scopes[0] != "https://graph.microsoft.com/.default" {
			t.Fatalf("unexpected identity scopes %q", p.Scopes)
		}
		return azcore.AccessToken{Token: "fixture-token"}, nil
	})
	g.client.Transport = send
	return g
}

func TestGraphSendsPreparedMIMEOnceAndStoresAcceptanceOnly(t *testing.T) {
	calls := 0
	requestID := "e2399af5-ff42-4ba4-9927-24512d5f819f"
	g := fixtureGraph(t, func(r *http.Request) (*http.Response, error) {
		calls++
		wantURL := "https://graph.microsoft.com/v1.0/users/" + fixtureGraphSender + "/sendMail"
		if r.Method != http.MethodPost || r.URL.String() != wantURL || r.GetBody != nil ||
			r.Header.Get("Content-Type") != "text/plain" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Fatalf("unexpected Graph POST method/path/headers: %s %s", r.Method, r.URL)
		}
		raw, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, r.Body))
		if err != nil {
			t.Fatal(err)
		}
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil || msg.Header.Get("Message-ID") != "<delivery@example.test>" ||
			msg.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" {
			t.Fatalf("bad Graph MIME: %v", err)
		}
		return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Request-Id": {requestID}}, Body: http.NoBody}, nil
	})
	prepared, err := g.Prepare(t.Context(), sesTestPayload())
	if err != nil || calls != 0 {
		t.Fatalf("prepare sent a request: calls=%d err=%v", calls, err)
	}
	receipt, err := prepared(t.Context())
	if err != nil || receipt.Provider != "graph" || receipt.ProviderMessageID != "" ||
		receipt.ProviderRequestID != requestID || receipt.AcceptedAt.IsZero() {
		t.Fatalf("Graph receipt = %#v, %v", receipt, err)
	}
	if _, err := prepared(t.Context()); err == nil || calls != 1 {
		t.Fatalf("prepared attempt replayed: calls=%d err=%v", calls, err)
	}
}

func TestGraphClassifiesSingleHTTPAttempt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		kind       ErrorKind
		retryAfter time.Duration
	}{
		{"forbidden", http.StatusForbidden, ErrorPermanent, 0},
		{"token rejected", http.StatusUnauthorized, ErrorTemporary, 0},
		{"throttled", http.StatusTooManyRequests, ErrorRateLimited, 11 * time.Second},
		{"invalid MIME", http.StatusBadRequest, ErrorPermanent, 0},
		{"server unavailable", http.StatusServiceUnavailable, ErrorAcceptanceUnknown, 0},
		{"request timeout", http.StatusRequestTimeout, ErrorAcceptanceUnknown, 0},
		{"redirect", http.StatusTemporaryRedirect, ErrorAcceptanceUnknown, 0},
		{"network timeout", 0, ErrorAcceptanceUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := fixtureGraph(t, func(*http.Request) (*http.Response, error) {
				calls++
				if tc.status == 0 {
					return nil, errors.New("sensitive network error")
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"Retry-After": {"11"}, "Location": {"https://untrusted.example.test"}},
					Body:       io.NopCloser(strings.NewReader("sensitive response body")),
				}, nil
			})
			_, err := g.Send(t.Context(), sesTestPayload())
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.kind || pe.HTTPStatus != tc.status ||
				pe.RetryAfter != tc.retryAfter || calls != 1 || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Graph failure = %v, calls=%d", err, calls)
			}
		})
	}
}

func TestGraphTokenAndHeaderValidationFailBeforeSubmission(t *testing.T) {
	g := fixtureGraph(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("request submitted before token and MIME validation")
		return nil, nil
	})
	g.identity = tokenFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, errors.New("sensitive identity failure")
	})
	payload := sesTestPayload()
	_, err := g.Send(t.Context(), payload)
	var failure *ProviderError
	if !errors.As(err, &failure) || failure.Kind != ErrorTemporary || failure.Operation != "auth" || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("Graph identity failure = %v", err)
	}
	payload.Subject = "bad\r\nBcc: forged@example.test"
	_, err = g.Send(t.Context(), payload)
	if !errors.As(err, &failure) || failure.Kind != ErrorInvalidEndpoint || failure.Operation != "validate" {
		t.Fatalf("Graph MIME injection failure = %v", err)
	}
}

func TestGraphReacquiresTokenOnNextDurableAttempt(t *testing.T) {
	submissions := 0
	g := fixtureGraph(t, func(r *http.Request) (*http.Response, error) {
		submissions++
		status := http.StatusUnauthorized
		if r.Header.Get("Authorization") == "Bearer renewed-fixture-token" {
			status = http.StatusAccepted
		}
		return &http.Response{StatusCode: status, Body: http.NoBody}, nil
	})
	acquisitions := 0
	g.identity = tokenFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		acquisitions++
		value := "original-fixture-token"
		if acquisitions == 2 {
			value = "renewed-fixture-token"
		}
		return azcore.AccessToken{Token: value}, nil
	})
	_, err := g.Send(t.Context(), sesTestPayload())
	var failure *ProviderError
	if !errors.As(err, &failure) || failure.Kind != ErrorTemporary || submissions != 1 || acquisitions != 1 {
		t.Fatalf("first durable attempt acquisitions=%d submissions=%d error=%v", acquisitions, submissions, err)
	}
	if _, err := g.Send(t.Context(), sesTestPayload()); err != nil || submissions != 2 || acquisitions != 2 {
		t.Fatalf("next durable attempt acquisitions=%d submissions=%d error=%v", acquisitions, submissions, err)
	}
}
