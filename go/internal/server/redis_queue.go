package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
	"github.com/redis/go-redis/v9"
)

// RedisQueue manages message queue operations with Redis
type RedisQueue struct {
	client    *redis.Client
	log       *slog.Logger
	available bool
}

// NewRedisQueue creates a new Redis queue manager
// Returns nil if REDIS_URL is not configured
func NewRedisQueue(redisURL string, log *slog.Logger) *RedisQueue {
	if redisURL == "" {
		log.Info("Redis not configured, queue will use SQLite only")
		return nil
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Error("failed to parse REDIS_URL", "error", err)
		return nil
	}

	// Set connection pool parameters
	opts.PoolSize = 10
	opts.MinIdleConns = 2
	opts.ConnMaxIdleTime = 5 * time.Minute

	client := redis.NewClient(opts)

	rq := &RedisQueue{
		client:    client,
		log:       log,
		available: true,
	}

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Error("Redis ping failed, will use SQLite fallback", "error", err)
		rq.available = false
	} else {
		log.Info("Redis connected successfully")
	}

	return rq
}

// Close closes the Redis connection
func (rq *RedisQueue) Close() error {
	if rq == nil || rq.client == nil {
		return nil
	}
	return rq.client.Close()
}

// IsAvailable returns whether Redis is currently available
func (rq *RedisQueue) IsAvailable() bool {
	if rq == nil {
		return false
	}
	return rq.available
}

// Enqueue pushes a message to the Redis queue
func (rq *RedisQueue) Enqueue(ctx context.Context, msg store.QueueMessage) error {
	if rq == nil || !rq.available {
		return fmt.Errorf("redis not available")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	queueKey := fmt.Sprintf("queue:%s", msg.SessionID)

	if err := rq.client.RPush(ctx, queueKey, data).Err(); err != nil {
		rq.log.Error("failed to push to Redis", "error", err)
		rq.available = false
		return err
	}

	return nil
}

// Dequeue pops a message from the Redis queue (blocking)
func (rq *RedisQueue) Dequeue(ctx context.Context, sessionID string, timeout time.Duration) (*store.QueueMessage, error) {
	if rq == nil || !rq.available {
		return nil, fmt.Errorf("redis not available")
	}

	queueKey := fmt.Sprintf("queue:%s", sessionID)

	result, err := rq.client.BLPop(ctx, timeout, queueKey).Result()
	if err != nil {
		if err == redis.Nil {
			// No message available (timeout)
			return nil, nil
		}
		rq.log.Error("failed to pop from Redis", "error", err)
		rq.available = false
		return nil, err
	}

	if len(result) < 2 {
		return nil, fmt.Errorf("unexpected BLPop result length: %d", len(result))
	}

	var msg store.QueueMessage
	if err := json.Unmarshal([]byte(result[1]), &msg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return &msg, nil
}

// PublishStatusChange publishes a status change event via Redis PubSub
func (rq *RedisQueue) PublishStatusChange(ctx context.Context, sessionID, msgID, status, errorMsg string) error {
	if rq == nil || !rq.available {
		return fmt.Errorf("redis not available")
	}

	channel := fmt.Sprintf("queue:status:%s", sessionID)

	payload := map[string]string{
		"messageId":    msgID,
		"status":       status,
		"errorMessage": errorMsg,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	if err := rq.client.Publish(ctx, channel, data).Err(); err != nil {
		rq.log.Error("failed to publish to Redis", "error", err)
		rq.available = false
		return err
	}

	return nil
}

// HealthCheck attempts to reconnect if Redis was previously unavailable
func (rq *RedisQueue) HealthCheck(ctx context.Context) {
	if rq == nil || rq.available {
		return
	}

	if err := rq.client.Ping(ctx).Err(); err == nil {
		rq.log.Info("Redis reconnected successfully")
		rq.available = true
	}
}
