package notify

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type ServiceBusQueue struct {
	sender   *azservicebus.Sender
	receiver *azservicebus.Receiver
}

func NewServiceBusQueue(connectionString, queueName string) (*ServiceBusQueue, error) {
	if connectionString == "" || queueName == "" {
		return nil, errors.New("service bus connection string and queue name are required")
	}
	client, err := azservicebus.NewClientFromConnectionString(connectionString, nil)
	if err != nil {
		return nil, err
	}
	sender, err := client.NewSender(queueName, nil)
	if err != nil {
		return nil, err
	}
	receiver, err := client.NewReceiverForQueue(queueName, nil)
	if err != nil {
		return nil, err
	}
	return &ServiceBusQueue{sender: sender, receiver: receiver}, nil
}

func (q *ServiceBusQueue) Enqueue(ctx context.Context, message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return q.sender.SendMessage(ctx, &azservicebus.Message{Body: body}, nil)
}

func (q *ServiceBusQueue) Consume(ctx context.Context, handle func(context.Context, Message) error) error {
	for {
		messages, err := q.receiver.ReceiveMessages(ctx, 1, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, raw := range messages {
			var message Message
			err := json.Unmarshal(raw.Body, &message)
			if err == nil {
				err = handle(ctx, message)
			}
			if err != nil {
				_ = q.receiver.AbandonMessage(ctx, raw, nil)
				continue
			}
			_ = q.receiver.CompleteMessage(ctx, raw, nil)
		}
	}
}
