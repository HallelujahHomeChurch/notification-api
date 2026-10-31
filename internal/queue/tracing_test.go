package queue

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestPublishConsumerLinkWithoutPrivateMessageMetadata(t *testing.T) {
	old := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(provider)
	defer func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(old) }()
	sender := &recordingSender{}
	if err := newServiceBus(sender, nil).Publish(context.Background(), "transport-id", "db334a0d-2dba-4a22-a8bc-e96489d43f89"); err != nil {
		t.Fatal(err)
	}
	published := sender.messages[0]
	if len(published.ApplicationProperties) != 1 {
		t.Fatal("unexpected queue metadata")
	}
	producer := exp.GetSpans()[0]
	receiver := &recordingReceiver{}
	consumer := newServiceBusConsumer(receiver, nil)
	message := &serviceBusMessage{receiver: receiver, message: &azservicebus.ReceivedMessage{ApplicationProperties: published.ApplicationProperties}}
	if err := consumer.handle(context.Background(), message, func(ctx context.Context, _ BrokerMessage) error {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() || sc.TraceID() == producer.SpanContext.TraceID() {
			t.Fatal("consumer must start independent root")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 2 || len(spans[1].Links) != 1 || spans[1].Links[0].SpanContext.TraceID() != producer.SpanContext.TraceID() || spans[1].Links[0].SpanContext.SpanID() != producer.SpanContext.SpanID() {
		t.Fatal("producer-consumer link missing")
	}
	for _, span := range spans {
		if len(span.Attributes) != 0 || len(span.Events) != 0 || span.Status.Description != "" {
			t.Fatal("private queue metadata exported")
		}
	}
}
func TestConsumerRejectsMalformedLink(t *testing.T) {
	old := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(provider)
	defer func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(old) }()
	for _, value := range []any{"token", 42, "00-00000000000000000000000000000000-1234567890123456-01"} {
		message := &serviceBusMessage{message: &azservicebus.ReceivedMessage{ApplicationProperties: map[string]any{"traceparent": value, "token": "secret"}}}
		if err := newServiceBusConsumer(nil, nil).handle(context.Background(), message, func(context.Context, BrokerMessage) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, span := range exp.GetSpans() {
		if len(span.Links) != 0 {
			t.Fatal("invalid parent linked")
		}
	}
}
