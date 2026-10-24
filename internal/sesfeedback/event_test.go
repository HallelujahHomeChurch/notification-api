package sesfeedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{DLQURL: "https://sqs.ap-northeast-1.amazonaws.com/123456789012/feedback-dlq", QueueURL: "https://sqs.ap-northeast-1.amazonaws.com/123456789012/feedback", TopicARN: "arn:aws:sns:ap-northeast-1:123456789012:feedback", AccountID: "123456789012", Region: "ap-northeast-1", Sender: "no-reply@example.test", ConfigurationSet: "notification"}
}
func testBody(edit func(map[string]any), envelopeEdit func(map[string]any)) string {
	now := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	m := map[string]any{"eventType": "Bounce", "mail": map[string]any{"messageId": "ses-message", "source": "no-reply@example.test", "sourceArn": "arn:aws:ses:ap-northeast-1:123456789012:identity/example.test", "sendingAccountId": "123456789012", "destination": []string{"member@example.test"}, "timestamp": now, "tags": map[string][]string{"ses:configuration-set": {"notification"}, "hhc_delivery_id": {"11111111-1111-1111-1111-111111111111"}}}, "bounce": map[string]any{"bounceType": "Permanent", "timestamp": now}}
	if edit != nil {
		edit(m)
	}
	data, _ := json.Marshal(m)
	e := map[string]any{"Type": "Notification", "MessageId": uuid.NewString(), "TopicArn": testConfig().TopicARN, "Message": string(data)}
	if envelopeEdit != nil {
		envelopeEdit(e)
	}
	data, _ = json.Marshal(e)
	return string(data)
}
func TestParseValidatesQueueOriginAndReducesPayload(t *testing.T) {
	e, err := Parse(testBody(nil, nil), testConfig())
	if err != nil || e.Kind != "hard_bounce" || e.DeliveryID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("event=%#v err=%v", e, err)
	}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		outer func(map[string]any)
	}{
		{name: "wrong_topic", outer: func(m map[string]any) { m["TopicArn"] = "other" }},
		{name: "subscription_url", outer: func(m map[string]any) {
			m["Type"] = "SubscriptionConfirmation"
			m["SubscribeURL"] = "http://169.254.169.254/"
		}},
		{name: "wrong_sender", edit: func(m map[string]any) { m["mail"].(map[string]any)["source"] = "HHC <attacker@example.test>" }},
		{name: "wrong_account", edit: func(m map[string]any) { m["mail"].(map[string]any)["sendingAccountId"] = "999999999999" }},
		{name: "wrong_identity", edit: func(m map[string]any) { m["mail"].(map[string]any)["sourceArn"] = "other" }},
		{name: "multiple_recipients", edit: func(m map[string]any) {
			m["mail"].(map[string]any)["destination"] = []string{"a@example.test", "b@example.test"}
		}},
		{name: "missing_configset", edit: func(m map[string]any) { m["mail"].(map[string]any)["tags"] = map[string][]string{} }},
		{name: "invalid_delivery", edit: func(m map[string]any) {
			m["mail"].(map[string]any)["tags"].(map[string][]string)["hhc_delivery_id"] = []string{"not-a-uuid"}
		}},
		{name: "future", edit: func(m map[string]any) {
			m["bounce"].(map[string]any)["timestamp"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(testBody(tc.edit, tc.outer), testConfig()); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("accepted %s: %v", tc.name, err)
			}
		})
	}
	if _, err := Parse(strings.Repeat("x", 512*1024+1), testConfig()); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("oversized payload accepted")
	}
	if _, err := Parse(testBody(func(m map[string]any) { m["newFutureField"] = "ignored" }, nil), testConfig()); err != nil {
		t.Fatal(err)
	}
	e, err = Parse(testBody(func(m map[string]any) { m["bounce"].(map[string]any)["bounceType"] = "Transient" }, nil), testConfig())
	if err != nil || e.Kind != "soft_bounce" {
		t.Fatal("soft bounce classified as permanent")
	}
	if _, err := Parse(testBody(func(m map[string]any) { m["mail"].(map[string]any)["source"] = "HHC Church <no-reply@example.test>" }, nil), testConfig()); err != nil {
		t.Fatalf("SES display-name source rejected: %v", err)
	}
	c := testConfig()
	c.QueueURL = "https://attacker.test/feedback"
	if c.Validate() == nil {
		t.Fatal("external queue endpoint accepted")
	}
	c = testConfig()
	c.Sender = "invalid"
	if _, err := Parse(testBody(nil, nil), c); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}

type fakeQueue struct {
	messages   []types.Message
	deleted    int
	attributes map[string]string
}

func (q *fakeQueue) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return &sqs.ReceiveMessageOutput{Messages: q.messages}, nil
}
func (q *fakeQueue) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.deleted++
	return &sqs.DeleteMessageOutput{}, nil
}

type handleFunc func(context.Context, Event) error

func (f handleFunc) Handle(ctx context.Context, e Event) error { return f(ctx, e) }
func TestConsumerDeletesOnlyCommittedEventsAndDoesNotLogPII(t *testing.T) {
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(old)
	q := &fakeQueue{messages: []types.Message{{Body: aws.String(testBody(nil, nil)), ReceiptHandle: aws.String("opaque-receipt")}}}
	c := &Consumer{config: testConfig(), client: q, store: handleFunc(func(context.Context, Event) error { return errors.New("secret-token member@example.test") })}
	if err := c.Poll(context.Background()); err != nil || q.deleted != 0 {
		t.Fatal("uncommitted event deleted")
	}
	if strings.Contains(log.String(), "member@example.test") || strings.Contains(log.String(), "secret-token") {
		t.Fatal("provider data logged")
	}
	log.Reset()
	c.store = handleFunc(func(context.Context, Event) error { return ErrDeliveryInFlight })
	if err := c.Poll(context.Background()); err != nil || q.deleted != 0 || strings.Contains(log.String(), "ses_feedback_persist_failed") {
		t.Fatal("normal in-flight feedback must retry quietly without deletion")
	}
	c.store = handleFunc(func(context.Context, Event) error { return nil })
	if err := c.Poll(context.Background()); err != nil || q.deleted != 1 {
		t.Fatal("committed event not deleted")
	}
	q.messages[0].Body = aws.String("invalid")
	if err := c.Poll(context.Background()); err != nil || q.deleted != 1 {
		t.Fatal("invalid event deleted instead of leaving it for DLQ")
	}
}

func (q *fakeQueue) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{Attributes: q.attributes}, nil
}
func TestQueuePreflightAndDeadLetterMonitoring(t *testing.T) {
	q := &fakeQueue{attributes: map[string]string{"QueueArn": "wrong"}}
	c := &Consumer{config: testConfig(), client: q}
	if c.checkQueues(context.Background()) == nil {
		t.Fatal("wrong queue accepted")
	}
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(old)
	q.attributes = map[string]string{"ApproximateNumberOfMessages": "1"}
	if err := c.checkDLQ(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "ses_feedback_dlq_nonempty") {
		t.Fatal("dead-letter backlog was not reported")
	}
}
