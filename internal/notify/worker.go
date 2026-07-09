package notify

import (
	"context"
	"log"
)

type Consumer interface {
	Consume(context.Context, func(context.Context, Message) error) error
}

func RunWorker(ctx context.Context, consumer Consumer, sender Sender, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	return consumer.Consume(ctx, func(ctx context.Context, message Message) error {
		email, err := BuildEmail(message)
		if err != nil {
			logger.Printf("notification build failed template=%s to=%s error=%v", message.Template, message.To, err)
			return err
		}
		if err := sender.Send(ctx, email); err != nil {
			logger.Printf("notification send failed template=%s to=%s error=%v", message.Template, message.To, err)
			return err
		}
		logger.Printf("notification sent template=%s to=%s", message.Template, message.To)
		return nil
	})
}
