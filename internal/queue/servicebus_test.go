package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type recordingSender struct {
	message *azservicebus.Message
	err     error
}

func (s *recordingSender) SendMessage(_ context.Context, message *azservicebus.Message, _ *azservicebus.SendMessageOptions) error {
	s.message = message
	return s.err
}

func TestServiceBusPublishUsesDeliveryIDAsBodyAndMessageID(t *testing.T) {
	sender := &recordingSender{}
	publisher := newServiceBus(sender, nil)
	deliveryID := "db334a0d-2dba-4a22-a8bc-e96489d43f89"

	if err := publisher.Publish(context.Background(), deliveryID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := string(sender.message.Body), `{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89"}`; got != want {
		t.Fatalf("message body = %q, want %q", got, want)
	}
	if sender.message.MessageID == nil || *sender.message.MessageID != deliveryID {
		t.Fatalf("MessageID = %v, want %q", sender.message.MessageID, deliveryID)
	}
}

func TestServiceBusPublishReturnsBrokerError(t *testing.T) {
	brokerErr := errors.New("service bus unavailable")
	publisher := newServiceBus(&recordingSender{err: brokerErr}, nil)

	if err := publisher.Publish(context.Background(), "db334a0d-2dba-4a22-a8bc-e96489d43f89"); !errors.Is(err, brokerErr) {
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
