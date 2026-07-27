package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type recordingSender struct {
	messages []*azservicebus.Message
	err      error
}

func (s *recordingSender) SendMessage(_ context.Context, message *azservicebus.Message, _ *azservicebus.SendMessageOptions) error {
	s.messages = append(s.messages, message)
	return s.err
}

func TestServiceBusPublishUsesOutboxIDAsStableMessageID(t *testing.T) {
	sender := &recordingSender{}
	publisher := newServiceBus(sender, nil)
	outboxID := "0bc93418-a9ba-4283-b4e3-49564200f4b5"
	retryOutboxID := "70e3dbdb-5d91-41ea-9a49-dfb7bb92b500"
	deliveryID := "db334a0d-2dba-4a22-a8bc-e96489d43f89"

	for _, transportID := range []string{outboxID, outboxID, retryOutboxID} {
		if err := publisher.Publish(context.Background(), transportID, deliveryID); err != nil {
			t.Fatalf("Publish(%q) error = %v", transportID, err)
		}
	}
	for _, message := range sender.messages {
		if got, want := string(message.Body), `{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89"}`; got != want {
			t.Fatalf("message body = %q, want %q", got, want)
		}
	}
	for index, want := range []string{outboxID, outboxID, retryOutboxID} {
		if sender.messages[index].MessageID == nil || *sender.messages[index].MessageID != want {
			t.Fatalf("message %d MessageID = %v, want %q", index, sender.messages[index].MessageID, want)
		}
	}
}

func TestServiceBusPublishReturnsBrokerError(t *testing.T) {
	brokerErr := errors.New("service bus unavailable")
	publisher := newServiceBus(&recordingSender{err: brokerErr}, nil)

	if err := publisher.Publish(
		context.Background(),
		"0bc93418-a9ba-4283-b4e3-49564200f4b5",
		"db334a0d-2dba-4a22-a8bc-e96489d43f89",
	); !errors.Is(err, brokerErr) {
		t.Fatalf("Publish() error = %v, want %v", err, brokerErr)
	}
}

func TestNewServiceBusRequiresQueueAndAuthenticationSource(t *testing.T) {
	tests := []struct {
		name   string
		config ServiceBusConfig
	}{
		{name: "queue", config: ServiceBusConfig{NamespaceFQDN: "example.servicebus.windows.net"}},
		{name: "authentication", config: ServiceBusConfig{QueueName: "notifications-email"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewServiceBus(test.config); err == nil {
				t.Fatal("NewServiceBus() error = nil")
			}
		})
	}
}
