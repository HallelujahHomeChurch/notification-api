package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type ServiceBusConfig struct {
	NamespaceFQDN    string
	ConnectionString string
	QueueName        string
}

type messageSender interface {
	SendMessage(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error
}

type ServiceBus struct {
	sender messageSender
	close  func(context.Context) error
}

func NewServiceBus(cfg ServiceBusConfig) (*ServiceBus, error) {
	if cfg.QueueName == "" {
		return nil, errors.New("service bus queue name is required")
	}

	var (
		client *azservicebus.Client
		err    error
	)
	if cfg.ConnectionString != "" {
		client, err = azservicebus.NewClientFromConnectionString(cfg.ConnectionString, nil)
	} else {
		if cfg.NamespaceFQDN == "" {
			return nil, errors.New("service bus namespace is required")
		}
		credential, credentialErr := azidentity.NewDefaultAzureCredential(nil)
		if credentialErr != nil {
			return nil, fmt.Errorf("create service bus credential: %w", credentialErr)
		}
		client, err = azservicebus.NewClient(cfg.NamespaceFQDN, credential, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("create service bus client: %w", err)
	}
	sender, err := client.NewSender(cfg.QueueName, nil)
	if err != nil {
		_ = client.Close(context.Background())
		return nil, fmt.Errorf("create service bus sender: %w", err)
	}
	return newServiceBus(sender, func(ctx context.Context) error {
		return errors.Join(sender.Close(ctx), client.Close(ctx))
	}), nil
}

func newServiceBus(sender messageSender, close func(context.Context) error) *ServiceBus {
	return &ServiceBus{sender: sender, close: close}
}

func (q *ServiceBus) Publish(ctx context.Context, messageID, deliveryID string) error {
	if messageID == "" {
		return errors.New("message ID is required")
	}
	if deliveryID == "" {
		return errors.New("delivery ID is required")
	}
	body, err := json.Marshal(struct {
		DeliveryID string `json:"deliveryId"`
	}{DeliveryID: deliveryID})
	if err != nil {
		return fmt.Errorf("encode service bus message: %w", err)
	}
	contentType := "application/json"
	if err := q.sender.SendMessage(ctx, &azservicebus.Message{
		Body:        body,
		ContentType: &contentType,
		MessageID:   &messageID,
	}, nil); err != nil {
		return fmt.Errorf("publish outbox message %s: %w", messageID, err)
	}
	return nil
}

func (q *ServiceBus) Close(ctx context.Context) error {
	if q.close == nil {
		return nil
	}
	return q.close(ctx)
}
