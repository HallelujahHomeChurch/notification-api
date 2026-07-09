package notify

import (
	"context"
)

type MemoryQueue struct {
	ch chan Message
}

func NewMemoryQueue(size int) *MemoryQueue {
	if size <= 0 {
		size = 100
	}
	return &MemoryQueue{ch: make(chan Message, size)}
}

func (q *MemoryQueue) Enqueue(ctx context.Context, message Message) error {
	select {
	case q.ch <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *MemoryQueue) Consume(ctx context.Context, handle func(context.Context, Message) error) error {
	for {
		select {
		case message := <-q.ch:
			_ = handle(ctx, message)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
