package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type recordingReceiver struct {
	mu              sync.Mutex
	messages        []*azservicebus.ReceivedMessage
	completed       []*azservicebus.ReceivedMessage
	deadLetters     []string
	deadLettered    chan struct{}
	renewed         chan struct{}
	renewCount      int
	renewErr        error
	blockComplete   bool
	blockDeadLetter bool
	closed          bool
}

func (r *recordingReceiver) ReceiveMessages(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
	r.mu.Lock()
	if len(r.messages) > 0 {
		message := r.messages[0]
		r.messages = r.messages[1:]
		r.mu.Unlock()
		return []*azservicebus.ReceivedMessage{message}, nil
	}
	r.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *recordingReceiver) CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, _ *azservicebus.CompleteMessageOptions) error {
	if r.blockComplete {
		<-ctx.Done()
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed = append(r.completed, message)
	return nil
}

func (r *recordingReceiver) DeadLetterMessage(ctx context.Context, _ *azservicebus.ReceivedMessage, options *azservicebus.DeadLetterOptions) error {
	if r.blockDeadLetter {
		<-ctx.Done()
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadLetters = append(r.deadLetters, *options.Reason)
	if r.deadLettered != nil {
		r.deadLettered <- struct{}{}
	}
	return nil
}

func (r *recordingReceiver) RenewMessageLock(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewCount++
	if r.renewed != nil {
		r.renewed <- struct{}{}
	}
	return r.renewErr
}

func (r *recordingReceiver) Close(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func TestServiceBusConsumerProcessesExactDeliveryEnvelope(t *testing.T) {
	receiver := &recordingReceiver{messages: []*azservicebus.ReceivedMessage{
		{Body: []byte(`{"deliveryId":"DB334A0D-2DBA-4A22-A8BC-E96489D43F89"}`)},
	}}
	consumer := newServiceBusConsumer(receiver, nil)
	ctx, cancel := context.WithCancel(context.Background())

	err := consumer.Run(ctx, func(ctx context.Context, message BrokerMessage) error {
		if got, want := message.DeliveryID(), "db334a0d-2dba-4a22-a8bc-e96489d43f89"; got != want {
			t.Fatalf("DeliveryID() = %q, want %q", got, want)
		}
		if err := message.Complete(ctx); err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if len(receiver.completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(receiver.completed))
	}
}

func TestServiceBusConsumerRenewsPeekLockUntilHandlerCompletes(t *testing.T) {
	receiver := &recordingReceiver{
		messages: []*azservicebus.ReceivedMessage{
			{Body: []byte(`{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89"}`)},
		},
		renewed: make(chan struct{}, 3),
	}
	consumer := newServiceBusConsumer(receiver, nil)
	consumer.renewInterval = 5 * time.Millisecond
	consumer.settlementTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := consumer.Run(ctx, func(ctx context.Context, message BrokerMessage) error {
		for range 3 {
			select {
			case <-receiver.renewed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := message.Complete(ctx); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled after completion", err)
	}
	if receiver.renewCount < 3 {
		t.Fatalf("renew count = %d, want at least 3", receiver.renewCount)
	}
	if len(receiver.completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(receiver.completed))
	}
}

func TestServiceBusConsumerRenewalFailureCancelsHandlerAndLeavesMessageUnsettled(t *testing.T) {
	renewErr := errors.New("lock renewal failed")
	receiver := &recordingReceiver{
		messages: []*azservicebus.ReceivedMessage{
			{Body: []byte(`{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89"}`)},
		},
		renewErr: renewErr,
	}
	consumer := newServiceBusConsumer(receiver, nil)
	consumer.renewInterval = 5 * time.Millisecond
	consumer.settlementTimeout = 50 * time.Millisecond
	handlerCanceled := make(chan struct{})

	err := consumer.Run(context.Background(), func(ctx context.Context, message BrokerMessage) error {
		<-ctx.Done()
		close(handlerCanceled)
		return message.Complete(context.Background())
	})
	if !errors.Is(err, renewErr) {
		t.Fatalf("Run() error = %v, want %v", err, renewErr)
	}
	select {
	case <-handlerCanceled:
	case <-time.After(time.Second):
		t.Fatal("renewal failure did not cancel handler")
	}
	if len(receiver.completed) != 0 || len(receiver.deadLetters) != 0 {
		t.Fatal("renewal failure settled broker message")
	}
}

func TestServiceBusConsumerDeadLettersMalformedOrUnboundedEnvelope(t *testing.T) {
	receiver := &recordingReceiver{messages: []*azservicebus.ReceivedMessage{
		{Body: []byte(`{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89","extra":true}`)},
		{Body: []byte(`{"deliveryId":"not-a-uuid"}`)},
		{Body: []byte(strings.Repeat("x", maxBrokerMessageBody+1))},
	}, deadLettered: make(chan struct{}, 3)}
	consumer := newServiceBusConsumer(receiver, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(ctx, func(context.Context, BrokerMessage) error {
			t.Error("malformed message reached handler")
			return nil
		})
	}()

	for range 3 {
		<-receiver.deadLettered
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	for _, reason := range receiver.deadLetters {
		if reason != "invalid_message" {
			t.Fatalf("dead-letter reason = %q, want invalid_message", reason)
		}
	}
}

func TestServiceBusConsumerLeavesMessageUnsettledWhenHandlerFails(t *testing.T) {
	handlerErr := errors.New("database unavailable")
	receiver := &recordingReceiver{messages: []*azservicebus.ReceivedMessage{
		{Body: []byte(`{"deliveryId":"db334a0d-2dba-4a22-a8bc-e96489d43f89"}`)},
	}}
	consumer := newServiceBusConsumer(receiver, nil)

	err := consumer.Run(context.Background(), func(context.Context, BrokerMessage) error {
		return handlerErr
	})
	if !errors.Is(err, handlerErr) {
		t.Fatalf("Run() error = %v, want %v", err, handlerErr)
	}
	if len(receiver.completed) != 0 || len(receiver.deadLetters) != 0 {
		t.Fatal("handler failure settled broker message")
	}
}

func TestServiceBusMessageDeadLetterUsesPeekLockSettlement(t *testing.T) {
	receiver := &recordingReceiver{}
	message := &azservicebus.ReceivedMessage{}
	brokerMessage := serviceBusMessage{
		receiver:          receiver,
		message:           message,
		deliveryID:        "db334a0d-2dba-4a22-a8bc-e96489d43f89",
		settlementTimeout: time.Second,
	}

	if err := brokerMessage.DeadLetter(context.Background(), "permanent"); err != nil {
		t.Fatalf("DeadLetter() error = %v", err)
	}
	if got := receiver.deadLetters; len(got) != 1 || got[0] != "permanent" {
		t.Fatalf("dead letters = %v, want permanent", got)
	}
}

func TestServiceBusMessageBoundsSettlement(t *testing.T) {
	for _, test := range []struct {
		name   string
		config func(*recordingReceiver)
		settle func(*serviceBusMessage) error
	}{
		{
			name:   "complete",
			config: func(r *recordingReceiver) { r.blockComplete = true },
			settle: func(message *serviceBusMessage) error {
				return message.Complete(context.Background())
			},
		},
		{
			name:   "dead-letter",
			config: func(r *recordingReceiver) { r.blockDeadLetter = true },
			settle: func(message *serviceBusMessage) error {
				return message.DeadLetter(context.Background(), "permanent")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			receiver := &recordingReceiver{}
			test.config(receiver)
			message := serviceBusMessage{
				receiver:          receiver,
				message:           &azservicebus.ReceivedMessage{},
				deliveryID:        "db334a0d-2dba-4a22-a8bc-e96489d43f89",
				settlementTimeout: 20 * time.Millisecond,
			}

			started := time.Now()
			err := test.settle(&message)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("settlement error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("settlement elapsed = %s, want bounded", elapsed)
			}
		})
	}
}

func TestServiceBusConsumerBoundsMalformedMessageDeadLetter(t *testing.T) {
	receiver := &recordingReceiver{
		messages:        []*azservicebus.ReceivedMessage{{Body: []byte(`{"bad":true}`)}},
		blockDeadLetter: true,
	}
	consumer := newServiceBusConsumer(receiver, nil)
	consumer.settlementTimeout = 20 * time.Millisecond

	started := time.Now()
	err := consumer.Run(context.Background(), func(context.Context, BrokerMessage) error {
		t.Fatal("malformed message reached handler")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want settlement deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Run() elapsed = %s, want bounded malformed settlement", elapsed)
	}
}

func TestServiceBusConsumerCloseClosesReceiverAndClient(t *testing.T) {
	receiver := &recordingReceiver{}
	clientClosed := false
	consumer := newServiceBusConsumer(receiver, func(context.Context) error {
		clientClosed = true
		return nil
	})

	if err := consumer.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !receiver.closed || !clientClosed {
		t.Fatalf("closed receiver=%v client=%v", receiver.closed, clientClosed)
	}
}
