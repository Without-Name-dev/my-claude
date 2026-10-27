package connector

import (
	"context"
	"log/slog"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// StartQueueConsumers starts queue consumers for all active sessions
func StartQueueConsumers(ctx context.Context, db *store.Store, bridge Bridge, log *slog.Logger) {
	// TODO: This is a placeholder for queue consumer integration
	// In a full implementation, this would:
	// 1. Query active sessions from the server
	// 2. Start a consumer for each session
	// 3. Monitor for new sessions and start consumers dynamically

	log.Info("queue consumer system initialized (placeholder)")

	// Example: Start a single consumer for testing
	// Uncomment when ready to integrate:
	// consumer := NewQueueConsumer("session-id", db, executor, log)
	// go consumer.Start(ctx)
}

// Bridge interface for sending messages to runtime
type Bridge interface {
	Submit(sessionID, externalSessionID, text string) error
}

// SimpleBridgeExecutor wraps bridge submission as MessageExecutor
type SimpleBridgeExecutor struct {
	bridge            Bridge
	sessionID         string
	externalSessionID string
	log               *slog.Logger
}

func NewSimpleBridgeExecutor(bridge Bridge, sessionID, externalSessionID string, log *slog.Logger) *SimpleBridgeExecutor {
	return &SimpleBridgeExecutor{
		bridge:            bridge,
		sessionID:         sessionID,
		externalSessionID: externalSessionID,
		log:               log,
	}
}

func (sbe *SimpleBridgeExecutor) Execute(ctx context.Context, msg store.QueueMessage) error {
	// Submit the message content to the bridge
	return sbe.bridge.Submit(sbe.sessionID, sbe.externalSessionID, msg.Content)
}
