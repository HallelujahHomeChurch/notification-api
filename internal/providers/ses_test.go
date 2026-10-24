package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
)

type credentialFunc func(context.Context) (aws.Credentials, error)

func (f credentialFunc) Retrieve(ctx context.Context) (aws.Credentials, error) { return f(ctx) }

type sesFunc func(context.Context, *sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error)

func (f sesFunc) SendEmail(ctx context.Context, p *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	return f(ctx, p)
}

type tokenFunc func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (f tokenFunc) GetToken(ctx context.Context, p policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f(ctx, p)
}

type stsFunc func(context.Context, *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)

func (f stsFunc) AssumeRoleWithWebIdentity(ctx context.Context, p *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return f(ctx, p)
}

func sesTestConfig() SESConfig {
	return SESConfig{Region: "ap-northeast-1", RoleARN: "arn:aws:iam::123456789012:role/sender", ManagedIdentityClientID: "11111111-1111-1111-1111-111111111111", TokenScope: "api://22222222-2222-2222-2222-222222222222/.default", From: "sender@example.test", FromName: "Church", ConfigurationSet: "transactional"}
}
func sesTestPayload() DeliveryPayload {
	return DeliveryPayload{DeliveryID: "11111111-1111-1111-1111-111111111111", Recipient: "member@example.test", Subject: "驗證", Body: "123456", HTMLBody: "<p>123456</p>", MessageID: "<delivery@example.test>", ListUnsubscribe: "<https://example.test/unsubscribe>", OneClickUnsubscribe: true}
}
func readyCredentials(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test", SessionToken: "test"}, nil
}

func TestSESSendsRawMIMEAndReturnsActualReceipt(t *testing.T) {
	s := &SES{config: sesTestConfig(), credentials: credentialFunc(readyCredentials)}
	calls := 0
	s.client = sesFunc(func(ctx context.Context, in *sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error) {
		calls++
		raw := string(in.Content.Raw.Data)
		for _, want := range []string{"multipart/alternative", "Message-ID: <delivery@example.test>", "List-Unsubscribe-Post: List-Unsubscribe=One-Click", "text/plain", "text/html"} {
			if !strings.Contains(raw, want) {
				t.Errorf("missing MIME field %q", want)
			}
		}
		if len(in.Destination.ToAddresses) != 1 || *in.ConfigurationSetName != "transactional" || len(in.EmailTags) != 1 || *in.EmailTags[0].Value != sesTestPayload().DeliveryID {
			t.Fatal("destination/configuration/correlation mismatch")
		}
		return &sesv2.SendEmailOutput{MessageId: aws.String("actual-ses-id")}, nil
	})
	r, err := s.Send(context.Background(), sesTestPayload())
	if err != nil || calls != 1 || r.Provider != "ses" || r.ProviderMessageID != "actual-ses-id" || r.AcceptedAt.IsZero() {
		t.Fatalf("receipt=%#v calls=%d err=%v", r, calls, err)
	}
}

func TestSESErrorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"throttled", &smithy.GenericAPIError{Code: "TooManyRequestsException"}, ErrorRateLimited},
		{"rejected", &smithy.GenericAPIError{Code: "MessageRejected"}, ErrorPermanent},
		{"transport", context.DeadlineExceeded, ErrorAcceptanceUnknown},
		{"server", &smithy.GenericAPIError{Code: "InternalServiceErrorException"}, ErrorAcceptanceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := &SES{config: sesTestConfig(), credentials: credentialFunc(readyCredentials), client: sesFunc(func(context.Context, *sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error) {
				calls++
				return nil, tc.err
			})}
			_, err := s.Send(context.Background(), sesTestPayload())
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.Kind != tc.want || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
	s := &SES{config: sesTestConfig(), credentials: credentialFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("sensitive-token")
	}), client: sesFunc(func(context.Context, *sesv2.SendEmailInput) (*sesv2.SendEmailOutput, error) {
		t.Fatal("credentials failure must not submit")
		return nil, nil
	})}
	_, err := s.Send(context.Background(), sesTestPayload())
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Kind != ErrorTemporary || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe auth error %v", err)
	}
	p := sesTestPayload()
	p.Subject = "valid\r\nBcc: attacker@example.test"
	_, err = s.Send(context.Background(), p)
	if !errors.As(err, &pe) || pe.Kind != ErrorInvalidEndpoint {
		t.Fatalf("header injection accepted: %v", err)
	}
}

func TestSESExplicitCredentialsAndNoHiddenRetries(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "unwanted-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unwanted-secret")
	s, err := NewSES(sesTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if s.client.(*sesv2.Client).Options().RetryMaxAttempts != 1 {
		t.Fatal("SES SDK retries would bypass durable attempts")
	}
	if _, ok := s.credentials.(*aws.CredentialsCache); !ok {
		t.Fatal("federated credentials must be cached")
	}
	c := sesTestConfig()
	c.ManagedIdentityClientID = ""
	if _, err := NewSES(c); err == nil {
		t.Fatal("implicit identity fallback allowed")
	}
}

func TestFederationPropagatesContextAndCachesConcurrentExchange(t *testing.T) {
	var calls atomic.Int32
	marker := struct{}{}
	ctx := context.WithValue(context.Background(), marker, true)
	f := &federatedCredentials{scope: "api://test/.default", roleARN: "arn:aws:iam::123456789012:role/sender"}
	f.identity = tokenFunc(func(ctx context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
		if ctx.Value(marker) != true || len(o.Scopes) != 1 || o.Scopes[0] != f.scope {
			t.Error("lost context or audience")
		}
		return azcore.AccessToken{Token: "opaque-test-token"}, nil
	})
	f.sts = stsFunc(func(ctx context.Context, in *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
		calls.Add(1)
		if ctx.Value(marker) != true || *in.RoleArn != f.roleARN || *in.WebIdentityToken != "opaque-test-token" || *in.DurationSeconds != 3600 {
			t.Error("wrong exchange")
		}
		return &sts.AssumeRoleWithWebIdentityOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("test"), SecretAccessKey: aws.String("test"), SessionToken: aws.String("test"), Expiration: aws.Time(time.Now().Add(time.Hour))}}, nil
	})
	cache := aws.NewCredentialsCache(f)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := cache.Retrieve(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent STS exchanges: %d", calls.Load())
	}
	f.identity = tokenFunc(func(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, ctx.Err()
	})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Retrieve(cancelled); err == nil || calls.Load() != 1 {
		t.Fatal("cancelled identity must not exchange")
	}
}

func TestSESSDKDoesNotRetryServerFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"internal error"}`))
	}))
	defer server.Close()
	s, err := NewSES(sesTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	options := s.client.(*sesv2.Client).Options()
	options.BaseEndpoint = aws.String(server.URL)
	options.HTTPClient = server.Client()
	options.Credentials = credentialFunc(readyCredentials)
	s.client = sesv2.New(options)
	s.credentials = options.Credentials
	_, err = s.Send(context.Background(), sesTestPayload())
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Kind != ErrorAcceptanceUnknown || requests.Load() != 1 {
		t.Fatalf("error=%v requests=%d", err, requests.Load())
	}
}
