// Package sesfeedback consumes only an IAM-restricted SNS-to-SQS queue; it exposes no HTTP callback.
package sesfeedback

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/google/uuid"
)

var ErrInvalidEvent = errors.New("invalid SES feedback event")
var ErrUnmatchedEvent = errors.New("unmatched SES feedback event")

type Config struct{ QueueURL, DLQURL, TopicARN, AccountID, Region, Sender, ConfigurationSet string }

func (c Config) Validate() error {
	for _, endpoint := range []string{c.QueueURL, c.DLQURL} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host != "sqs."+c.Region+".amazonaws.com" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || !strings.HasPrefix(u.Path, "/"+c.AccountID+"/") || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 {
			return errors.New("invalid SES feedback queue URL")
		}
	}
	if c.QueueURL == c.DLQURL {
		return errors.New("SES feedback requires a separate dead-letter queue")
	}
	topic, err := arn.Parse(c.TopicARN)
	if err != nil || topic.Service != "sns" || topic.Region != c.Region || topic.AccountID != c.AccountID || !contracts.ValidEmail(c.Sender) || c.ConfigurationSet == "" {
		return errors.New("invalid SES feedback topic or sender settings")
	}
	return nil
}

type Event struct {
	SNSMessageID, MessageID, DeliveryID, Kind string
	At                                        time.Time
}

func Parse(body string, c Config) (Event, error) {
	if c.Validate() != nil {
		return Event{}, ErrInvalidEvent
	}
	var envelope struct{ Type, MessageId, TopicArn, Message string }
	if len(body) > 512*1024 || json.Unmarshal([]byte(body), &envelope) != nil || envelope.Type != "Notification" || envelope.TopicArn != c.TopicARN {
		return Event{}, ErrInvalidEvent
	}
	if _, err := uuid.Parse(envelope.MessageId); err != nil {
		return Event{}, ErrInvalidEvent
	}
	var message struct {
		EventType string `json:"eventType"`
		Mail      struct {
			MessageID                           string `json:"messageId"`
			Source, SourceArn, SendingAccountId string
			Destination                         []string
			Timestamp                           time.Time
			Tags                                map[string][]string
		}
		Bounce struct {
			BounceType string
			Timestamp  time.Time
		}
		Complaint, Delivery, DeliveryDelay struct{ Timestamp time.Time }
	}
	if json.Unmarshal([]byte(envelope.Message), &message) != nil {
		return Event{}, ErrInvalidEvent
	}
	if message.Mail.Source != c.Sender || message.Mail.SendingAccountId != c.AccountID || message.Mail.SourceArn != "arn:aws:ses:"+c.Region+":"+c.AccountID+":identity/"+strings.Split(c.Sender, "@")[1] || len(message.Mail.Destination) != 1 || message.Mail.MessageID == "" || len(message.Mail.MessageID) > 256 {
		return Event{}, ErrInvalidEvent
	}
	tags := message.Mail.Tags
	if len(tags["ses:configuration-set"]) != 1 || tags["ses:configuration-set"][0] != c.ConfigurationSet {
		return Event{}, ErrInvalidEvent
	}
	e := Event{SNSMessageID: envelope.MessageId, MessageID: message.Mail.MessageID, At: message.Mail.Timestamp}
	if ids := tags["hhc_delivery_id"]; len(ids) != 0 {
		if len(ids) != 1 {
			return Event{}, ErrInvalidEvent
		}
		id, err := uuid.Parse(ids[0])
		if err != nil {
			return Event{}, ErrInvalidEvent
		}
		e.DeliveryID = id.String()
	}
	switch message.EventType {
	case "Send":
		e.Kind = "send"
	case "Delivery":
		e.Kind = "delivery"
		e.At = message.Delivery.Timestamp
	case "Bounce":
		e.Kind = "soft_bounce"
		if message.Bounce.BounceType == "Permanent" {
			e.Kind = "hard_bounce"
		}
		e.At = message.Bounce.Timestamp
	case "Complaint":
		e.Kind = "complaint"
		e.At = message.Complaint.Timestamp
	case "DeliveryDelay":
		e.Kind = "delay"
		e.At = message.DeliveryDelay.Timestamp
	case "Reject":
		e.Kind = "reject"
	case "Rendering Failure":
		e.Kind = "rendering_failure"
	default:
		return Event{}, ErrInvalidEvent
	}
	if e.At.IsZero() || e.At.After(time.Now().Add(5*time.Minute)) {
		return Event{}, ErrInvalidEvent
	}
	return e, nil
}
