package sesfeedback

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type queueClient interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}
type handler interface {
	Handle(context.Context, Event) error
}
type Consumer struct {
	config Config
	client queueClient
	store  handler
}

func New(c Config, credentials aws.CredentialsProvider, store handler) (*Consumer, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Consumer{config: c, store: store, client: sqs.New(sqs.Options{Region: c.Region, Credentials: credentials, RetryMaxAttempts: 3})}, nil
}
func (c *Consumer) Run(ctx context.Context) error {
	if err := c.checkQueues(ctx); err != nil {
		return errors.New("SES feedback queue preflight failed")
	}
	var nextHealthCheck time.Time
	for ctx.Err() == nil {
		if !time.Now().Before(nextHealthCheck) {
			if err := c.checkDLQ(ctx); err != nil {
				slog.Error("ses_feedback_health_failed")
			}
			nextHealthCheck = time.Now().Add(time.Minute)
		}
		if err := c.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Error("ses_feedback_poll_failed")
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	return ctx.Err()
}
func (c *Consumer) Poll(ctx context.Context) error {
	receiveCtx, cancelReceive := context.WithTimeout(ctx, 25*time.Second)
	response, err := c.client.ReceiveMessage(receiveCtx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.config.QueueURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 20, VisibilityTimeout: 60})
	cancelReceive()
	if err != nil {
		return err
	}
	for _, message := range response.Messages {
		event, err := Parse(aws.ToString(message.Body), c.config)
		if err != nil {
			slog.Warn("ses_feedback_rejected")
			continue
		}
		if event.Kind != "setup" {
			persistCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.store.Handle(persistCtx, event)
			cancel()
			if err != nil {
				if !errors.Is(err, ErrDeliveryInFlight) {
					slog.Error("ses_feedback_persist_failed")
				}
				continue
			}
		}
		// Delete only after durable processing; a lost delete response is safe to replay.
		deleteCtx, cancelDelete := context.WithTimeout(ctx, 5*time.Second)
		_, err = c.client.DeleteMessage(deleteCtx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.config.QueueURL), ReceiptHandle: message.ReceiptHandle})
		cancelDelete()
		if err != nil {
			return err
		}
		if event.Kind == "setup" {
			slog.Info("ses_feedback_setup_received")
		}
	}
	return nil
}

func (c *Consumer) queueARN(endpoint string) string {
	return "arn:aws:sqs:" + c.config.Region + ":" + c.config.AccountID + ":" + endpoint[strings.LastIndex(endpoint, "/")+1:]
}

func (c *Consumer) checkQueues(ctx context.Context) error {
	for _, endpoint := range []string{c.config.QueueURL, c.config.DLQURL} {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		attributes, err := c.client.GetQueueAttributes(checkCtx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(endpoint), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn, types.QueueAttributeNameRedrivePolicy}})
		cancel()
		if err != nil {
			return err
		}
		if attributes.Attributes["QueueArn"] != c.queueARN(endpoint) {
			return errors.New("unexpected SES feedback queue")
		}
		if endpoint == c.config.QueueURL {
			var redrive struct{ DeadLetterTargetArn string }
			if json.Unmarshal([]byte(attributes.Attributes["RedrivePolicy"]), &redrive) != nil || redrive.DeadLetterTargetArn != c.queueARN(c.config.DLQURL) {
				return errors.New("unexpected SES feedback dead-letter queue")
			}
		}
	}
	return nil
}

func (c *Consumer) checkDLQ(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err := c.client.GetQueueAttributes(checkCtx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(c.config.DLQURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		return err
	}
	for _, name := range []string{"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"} {
		if a.Attributes[name] != "0" && a.Attributes[name] != "" {
			slog.Error("ses_feedback_dlq_nonempty")
			break
		}
	}
	return nil
}
