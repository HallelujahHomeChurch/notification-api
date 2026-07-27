package queue

import "context"

type Publisher interface {
	Publish(context.Context, string) error
}
