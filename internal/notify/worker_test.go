package notify

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
)

func TestBuildEmailAdaptsLegacyVerificationTemplate(t *testing.T) {
	email, err := BuildEmail(Message{
		Template: TemplateEmailVerification,
		To:       "user@example.test",
		Data: map[string]string{
			"verify_url": "https://account.alive.org.tw/verify-email?token=opaque",
		},
	})
	if err != nil {
		t.Fatalf("BuildEmail() error = %v", err)
	}
	if email.Subject != "Verify your HHC account" {
		t.Fatalf("BuildEmail() subject = %q", email.Subject)
	}
	if email.Body != "Use this link to verify your HHC account:\n\nhttps://account.alive.org.tw/verify-email?token=opaque\n" {
		t.Fatalf("BuildEmail() body = %q", email.Body)
	}
}

func TestRunWorkerBridgesLegacyMessageWithoutSensitiveLogs(t *testing.T) {
	message := Message{
		Template: TemplateEmailVerification,
		To:       "person@example.test",
		Data: map[string]string{
			"verify_url": "https://account.alive.org.tw/verify-email?token=opaque",
		},
	}
	provider := &recordingProvider{}
	var output bytes.Buffer

	err := RunWorker(
		context.Background(),
		oneMessageConsumer{message: message},
		provider,
		log.New(&output, "", 0),
	)
	if err != nil {
		t.Fatalf("RunWorker() error = %v", err)
	}
	if provider.payload.Recipient != message.To {
		t.Fatalf("provider recipient = %q, want legacy recipient", provider.payload.Recipient)
	}
	for _, sensitive := range []string{message.To, message.Data["verify_url"]} {
		if strings.Contains(output.String(), sensitive) {
			t.Fatalf("worker log contains sensitive value %q: %q", sensitive, output.String())
		}
	}
}

type oneMessageConsumer struct {
	message Message
}

func (c oneMessageConsumer) Consume(ctx context.Context, handle func(context.Context, Message) error) error {
	return handle(ctx, c.message)
}

type recordingProvider struct {
	payload providers.DeliveryPayload
}

func (p *recordingProvider) Send(_ context.Context, payload providers.DeliveryPayload) (providers.ProviderReceipt, error) {
	p.payload = payload
	return providers.ProviderReceipt{Provider: "test"}, nil
}
