package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type NoopLimiter struct{}

func (NoopLimiter) Allow(context.Context, Message) error {
	return nil
}

type DisabledLimiter struct{}

func (DisabledLimiter) Allow(context.Context, Message) error {
	return ErrDisabled
}

type RedisLimiter struct {
	client              redis.Cmdable
	cooldown            time.Duration
	recipientDailyLimit int
	globalDailyLimit    int
	now                 func() time.Time
}

func NewRedisLimiter(client redis.Cmdable, cooldown time.Duration, recipientDailyLimit, globalDailyLimit int) *RedisLimiter {
	return &RedisLimiter{
		client:              client,
		cooldown:            cooldown,
		recipientDailyLimit: recipientDailyLimit,
		globalDailyLimit:    globalDailyLimit,
		now:                 time.Now,
	}
}

func (l *RedisLimiter) Allow(ctx context.Context, message Message) error {
	if l.cooldown > 0 {
		ok, err := l.client.SetNX(ctx, fmt.Sprintf("notification:cooldown:%s:%s", message.Template, message.To), "1", l.cooldown).Result()
		if err != nil {
			return err
		}
		if !ok {
			return ErrRateLimited
		}
	}

	ttl := ttlUntilTomorrow(l.now().UTC())
	if l.recipientDailyLimit > 0 {
		key := fmt.Sprintf("notification:daily:recipient:%s:%s:%s", l.now().UTC().Format("20060102"), message.Template, message.To)
		count, err := incrWithTTL(ctx, l.client, key, ttl)
		if err != nil {
			return err
		}
		if count > int64(l.recipientDailyLimit) {
			return ErrRateLimited
		}
	}
	if l.globalDailyLimit > 0 {
		key := fmt.Sprintf("notification:daily:global:%s:%s", l.now().UTC().Format("20060102"), message.Template)
		count, err := incrWithTTL(ctx, l.client, key, ttl)
		if err != nil {
			return err
		}
		if count > int64(l.globalDailyLimit) {
			return ErrRateLimited
		}
	}
	return nil
}

func incrWithTTL(ctx context.Context, client redis.Cmdable, key string, ttl time.Duration) (int64, error) {
	count, err := client.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if count == 1 {
		_ = client.Expire(ctx, key, ttl).Err()
	}
	return count, nil
}

func ttlUntilTomorrow(now time.Time) time.Duration {
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return tomorrow.Sub(now)
}
