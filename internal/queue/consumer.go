package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/google/uuid"
)

const (
	maxBrokerMessageBody     = 1024
	defaultRenewInterval     = 30 * time.Second
	defaultSettlementTimeout = 5 * time.Second
)

var errLockRenewalLost = errors.New("service bus message lock renewal failed")

type messageReceiver interface {
	ReceiveMessages(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error)
	CompleteMessage(context.Context, *azservicebus.ReceivedMessage, *azservicebus.CompleteMessageOptions) error
	DeadLetterMessage(context.Context, *azservicebus.ReceivedMessage, *azservicebus.DeadLetterOptions) error
	RenewMessageLock(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error
	Close(context.Context) error
}

type ServiceBusConsumer struct {
	receiver          messageReceiver
	close             func(context.Context) error
	renewInterval     time.Duration
	settlementTimeout time.Duration
}

type serviceBusMessage struct {
	mu                sync.Mutex
	receiver          messageReceiver
	message           *azservicebus.ReceivedMessage
	deliveryID        string
	settlementTimeout time.Duration
	settled           bool
	settlementBlocked bool
}

func NewServiceBusConsumer(cfg ServiceBusConfig) (*ServiceBusConsumer, error) {
	if cfg.QueueName == "" {
		return nil, errors.New("service bus queue name is required")
	}
	client, err := newServiceBusClient(cfg)
	if err != nil {
		return nil, err
	}
	receiver, err := client.NewReceiverForQueue(cfg.QueueName, &azservicebus.ReceiverOptions{
		ReceiveMode: azservicebus.ReceiveModePeekLock,
	})
	if err != nil {
		_ = client.Close(context.Background())
		return nil, fmt.Errorf("create service bus receiver: %w", err)
	}
	return newServiceBusConsumer(receiver, func(ctx context.Context) error {
		return client.Close(ctx)
	}), nil
}

func newServiceBusConsumer(receiver messageReceiver, closeClient func(context.Context) error) *ServiceBusConsumer {
	return &ServiceBusConsumer{
		receiver:          receiver,
		close:             closeClient,
		renewInterval:     defaultRenewInterval,
		settlementTimeout: defaultSettlementTimeout,
	}
}

func (c *ServiceBusConsumer) Run(ctx context.Context, handle Handler) error {
	for {
		messages, err := c.receiver.ReceiveMessages(ctx, 1, nil)
		if err != nil {
			return fmt.Errorf("receive service bus message: %w", err)
		}
		for _, received := range messages {
			deliveryID, err := decodeDeliveryID(received)
			if err != nil {
				message := &serviceBusMessage{
					receiver:          c.receiver,
					message:           received,
					settlementTimeout: c.settlementTimeout,
				}
				if settleErr := message.DeadLetter(ctx, "invalid_message"); settleErr != nil {
					return fmt.Errorf("dead-letter invalid service bus message: %w", settleErr)
				}
				continue
			}
			message := &serviceBusMessage{
				receiver:          c.receiver,
				message:           received,
				deliveryID:        deliveryID,
				settlementTimeout: c.settlementTimeout,
			}
			if err := c.handle(ctx, message, handle); err != nil {
				return fmt.Errorf("process service bus delivery %s: %w", deliveryID, err)
			}
		}
	}
}

func (c *ServiceBusConsumer) handle(ctx context.Context, message *serviceBusMessage, handle Handler) error {
	handlerCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- handle(handlerCtx, message) }()

	ticker := time.NewTicker(c.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			cancel()
			return err
		case <-ctx.Done():
			cancel()
			return c.waitForHandler(result, ctx.Err())
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(ctx, c.settlementTimeout)
			err := message.renew(renewCtx)
			renewCancel()
			if err != nil {
				cancel()
				return c.waitForHandler(
					result,
					fmt.Errorf("renew service bus message lock: %w", err),
				)
			}
		}
	}
}

func (c *ServiceBusConsumer) waitForHandler(result <-chan error, cause error) error {
	timer := time.NewTimer(c.settlementTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil || errors.Is(err, context.Canceled) {
			return cause
		}
		return errors.Join(cause, err)
	case <-timer.C:
		return errors.Join(cause, errors.New("notification handler did not stop"))
	}
}

func (c *ServiceBusConsumer) Close(ctx context.Context) error {
	if c.receiver == nil {
		return nil
	}
	receiverErr := c.receiver.Close(ctx)
	var clientErr error
	if c.close != nil {
		clientErr = c.close(ctx)
	}
	return errors.Join(receiverErr, clientErr)
}

func (m *serviceBusMessage) DeliveryID() string {
	return m.deliveryID
}

func (m *serviceBusMessage) Complete(ctx context.Context) error {
	return m.settle(ctx, func(settleCtx context.Context) error {
		return m.receiver.CompleteMessage(settleCtx, m.message, nil)
	})
}

func (m *serviceBusMessage) DeadLetter(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "processing_failed"
	}
	if len(reason) > 128 {
		reason = reason[:128]
	}
	return m.settle(ctx, func(settleCtx context.Context) error {
		return m.receiver.DeadLetterMessage(settleCtx, m.message, &azservicebus.DeadLetterOptions{
			Reason:           &reason,
			ErrorDescription: &reason,
		})
	})
}

func (m *serviceBusMessage) renew(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settled {
		return nil
	}
	if err := m.receiver.RenewMessageLock(ctx, m.message, nil); err != nil {
		m.settlementBlocked = true
		return err
	}
	return nil
}

func (m *serviceBusMessage) settle(ctx context.Context, settle func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settlementBlocked {
		return errLockRenewalLost
	}
	if m.settled {
		return nil
	}
	timeout := m.settlementTimeout
	if timeout <= 0 {
		timeout = defaultSettlementTimeout
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := settle(settleCtx); err != nil {
		return err
	}
	m.settled = true
	return nil
}

func decodeDeliveryID(message *azservicebus.ReceivedMessage) (string, error) {
	if message == nil || len(message.Body) == 0 || len(message.Body) > maxBrokerMessageBody {
		return "", errors.New("invalid service bus message body")
	}
	var envelope struct {
		DeliveryID string `json:"deliveryId"`
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("service bus message must contain one JSON object")
	}
	parsed, err := uuid.Parse(envelope.DeliveryID)
	if err != nil {
		return "", errors.New("service bus deliveryId must be a UUID")
	}
	return parsed.String(), nil
}
