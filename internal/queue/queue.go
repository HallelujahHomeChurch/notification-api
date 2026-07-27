package queue

import "context"

type Publisher interface {
	Publish(context.Context, string, string) error
}

type BrokerMessage interface {
	DeliveryID() string
	Complete(context.Context) error
	DeadLetter(context.Context, string) error
}

type Handler func(context.Context, BrokerMessage) error
