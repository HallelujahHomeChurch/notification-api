package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

type SESConfig struct {
	Region, RoleARN, ManagedIdentityClientID, TokenScope string
	From, FromName, ConfigurationSet                     string
}

type sesClient interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

type SES struct {
	config      SESConfig
	credentials aws.CredentialsProvider
	client      sesClient
}

func ValidateSESConfig(c SESConfig) error {
	role, err := arn.Parse(c.RoleARN)
	if err != nil || role.Service != "iam" || role.Partition != "aws" || len(role.AccountID) != 12 || !strings.HasPrefix(role.Resource, "role/") || len(role.Resource) <= 5 {
		return errors.New("SES requires an explicit IAM role ARN")
	}
	if _, err := uuid.Parse(c.ManagedIdentityClientID); err != nil {
		return errors.New("SES requires a managed identity client ID")
	}
	if c.Region == "" || !strings.HasSuffix(c.TokenScope, "/.default") || strings.TrimSuffix(c.TokenScope, "/.default") == "" || c.ConfigurationSet == "" {
		return errors.New("SES region, managed identity, token scope and configuration set are required")
	}
	if !plainAddress(c.From) || strings.ContainsAny(c.FromName, "\r\n") {
		return errors.New("invalid SES sender")
	}
	return nil
}

func NewSES(c SESConfig) (*SES, error) {
	if err := ValidateSESConfig(c); err != nil {
		return nil, err
	}
	credentials, err := NewFederatedCredentials(c)
	if err != nil {
		return nil, err
	}
	client := sesv2.New(sesv2.Options{Region: c.Region, Credentials: credentials, RetryMaxAttempts: 1})
	return &SES{config: c, credentials: credentials, client: client}, nil
}

func (*SES) Family() string { return "ses" }

func NewFederatedCredentials(c SESConfig) (*aws.CredentialsCache, error) {
	if err := ValidateSESConfig(c); err != nil {
		return nil, err
	}
	// Explicit identity only: neither developer Azure credentials nor AWS keys/profiles are consulted.
	identity, err := azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(c.ManagedIdentityClientID)})
	if err != nil {
		return nil, fmt.Errorf("create SES managed identity: %w", err)
	}
	stsClient := sts.New(sts.Options{Region: c.Region, Credentials: aws.AnonymousCredentials{}, RetryMaxAttempts: 1})
	federated := &federatedCredentials{identity: identity, scope: c.TokenScope, roleARN: c.RoleARN, sts: stsClient}
	credentials := aws.NewCredentialsCache(federated, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = 5 * time.Minute })
	return credentials, nil
}

type webIdentitySTS interface {
	AssumeRoleWithWebIdentity(context.Context, *sts.AssumeRoleWithWebIdentityInput, ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error)
}
type federatedCredentials struct {
	identity       azcore.TokenCredential
	scope, roleARN string
	sts            webIdentitySTS
}

func (f *federatedCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	token, err := f.identity.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{f.scope}})
	if err != nil || token.Token == "" {
		return aws.Credentials{}, errors.New("SES managed identity token unavailable")
	}
	output, err := f.sts.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String(f.roleARN), RoleSessionName: aws.String("notification-worker"), WebIdentityToken: aws.String(token.Token), DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return aws.Credentials{}, errors.New("SES role exchange failed")
	}
	if output == nil || output.Credentials == nil {
		return aws.Credentials{}, errors.New("SES role exchange missing credentials")
	}
	c := output.Credentials
	if aws.ToString(c.AccessKeyId) == "" || aws.ToString(c.SecretAccessKey) == "" || aws.ToString(c.SessionToken) == "" || c.Expiration == nil || !c.Expiration.After(time.Now()) {
		return aws.Credentials{}, errors.New("SES role exchange returned invalid credentials")
	}
	return aws.Credentials{AccessKeyID: *c.AccessKeyId, SecretAccessKey: *c.SecretAccessKey, SessionToken: *c.SessionToken, CanExpire: true, Expires: *c.Expiration, Source: "AzureManagedIdentityWebIdentity"}, nil
}

func (s *SES) Send(ctx context.Context, p DeliveryPayload) (ProviderReceipt, error) {
	if _, err := uuid.Parse(p.DeliveryID); err != nil {
		return ProviderReceipt{}, sesFailure(ErrorInvalidEndpoint, "validate", errors.New("invalid delivery ID"))
	}
	// Reuse the same MIME trust-boundary validation as SMTP without requiring an SMTP endpoint.
	if err := validateEmailPayload(p); err != nil {
		return ProviderReceipt{}, sesFailure(ErrorInvalidEndpoint, "validate", err)
	}
	var raw bytes.Buffer
	if err := writeMessage(&raw, s.config.From, s.config.FromName, p); err != nil {
		return ProviderReceipt{}, sesFailure(ErrorInvalidEndpoint, "message", err)
	}
	// Fetch credentials before SendEmail so STS/Entra failures are known not to have submitted mail.
	if _, err := s.credentials.Retrieve(ctx); err != nil {
		return ProviderReceipt{}, sesFailure(ErrorTemporary, "auth", errors.New("SES credentials unavailable"))
	}
	if err := ctx.Err(); err != nil {
		return ProviderReceipt{}, sesFailure(ErrorTemporary, "auth", err)
	}
	output, err := s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.config.From), Destination: &types.Destination{ToAddresses: []string{p.Recipient}},
		ConfigurationSetName: aws.String(s.config.ConfigurationSet),
		EmailTags:            []types.MessageTag{{Name: aws.String("hhc_delivery_id"), Value: aws.String(p.DeliveryID)}},
		Content:              &types.EmailContent{Raw: &types.RawMessage{Data: raw.Bytes()}},
	})
	if err != nil {
		kind := ErrorAcceptanceUnknown
		var api smithy.APIError
		if errors.As(err, &api) {
			switch api.ErrorCode() {
			case "TooManyRequestsException", "ThrottlingException":
				kind = ErrorRateLimited
			case "BadRequestException", "MessageRejected", "MailFromDomainNotVerifiedException", "NotFoundException", "AccountSuspendedException", "SendingPausedException", "AccessDeniedException":
				kind = ErrorPermanent
			}
		}
		return ProviderReceipt{}, sesFailure(kind, "data", err)
	}
	if output == nil || aws.ToString(output.MessageId) == "" {
		return ProviderReceipt{}, sesFailure(ErrorAcceptanceUnknown, "data", errors.New("missing SES receipt"))
	}
	return ProviderReceipt{Provider: "ses", ProviderMessageID: aws.ToString(output.MessageId), AcceptedAt: time.Now().UTC()}, nil
}

func sesFailure(kind ErrorKind, operation string, err error) error {
	// ProviderError.Error deliberately excludes raw SDK errors, tokens and recipient data.
	pe := &ProviderError{Kind: kind, Operation: operation, ProviderFamily: "ses", cause: err}
	var response *awshttp.ResponseError
	if errors.As(err, &response) {
		pe.HTTPStatus = response.HTTPStatusCode()
		pe.ProviderRequestID = response.ServiceRequestID()
	}
	return pe
}
