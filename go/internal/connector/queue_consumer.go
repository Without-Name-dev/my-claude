package connector

import (
	"context"
	"log/slog"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// QueueConsumer manages queue message consumption and execution
type QueueConsumer struct {
	sessionID string
	store     *store.Store
	log       *slog.Logger
	executor  MessageExecutor
	stopCh    chan struct{}
}

// MessageExecutor defines the interface for executing queue messages
type MessageExecutor interface {
	Execute(ctx context.Context, msg store.QueueMessage) error
}

// NewQueueConsumer creates a new queue consumer
func NewQueueConsumer(sessionID string, store *store.Store, executor MessageExecutor, log *slog.Logger) *QueueConsumer {
	return &QueueConsumer{
		sessionID: sessionID,
		store:     store,
		log:       log,
		executor:  executor,
		stopCh:    make(chan struct{}),
	}
}

// Start begins consuming messages from the queue
func (qc *QueueConsumer) Start(ctx context.Context) {
	qc.log.Info("starting queue consumer", "sessionID", qc.sessionID)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			qc.log.Info("queue consumer stopped by context")
			return
		case <-qc.stopCh:
			qc.log.Info("queue consumer stopped")
			return
		case <-ticker.C:
			qc.processNext(ctx)
		}
	}
}

// Stop stops the queue consumer
func (qc *QueueConsumer) Stop() {
	close(qc.stopCh)
}

// processNext attempts to process the next queued message
func (qc *QueueConsumer) processNext(ctx context.Context) {
	msg, err := qc.store.DequeueNextMessage(ctx, qc.sessionID)
	if err != nil {
		qc.log.Error("failed to dequeue message", "error", err)
		return
	}

	if msg == nil {
		// No messages in queue
		return
	}

	qc.log.Info("processing queue message", "messageID", msg.ID, "content", truncateString(msg.Content, 50))

	// Update status to running
	if err := qc.store.UpdateMessageStatus(ctx, msg.ID, "running", ""); err != nil {
		qc.log.Error("failed to update status to running", "error", err)
		return
	}

	// Execute the message
	if err := qc.executor.Execute(ctx, *msg); err != nil {
		qc.log.Error("failed to execute message", "error", err, "messageID", msg.ID)
		errMsg := err.Error()
		if len(errMsg) > 500 {
			errMsg = errMsg[:500] + "..."
		}
		_ = qc.store.UpdateMessageStatus(ctx, msg.ID, "error", errMsg)
		return
	}

	// Update status to completed
	if err := qc.store.UpdateMessageStatus(ctx, msg.ID, "completed", ""); err != nil {
		qc.log.Error("failed to update status to completed", "error", err)
	}

	qc.log.Info("queue message completed", "messageID", msg.ID)
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// RuntimeMessageExecutor implements MessageExecutor by sending to the runtime
type RuntimeMessageExecutor struct {
	sendMessage func(ctx context.Context, content string, attachments []string) error
	log         *slog.Logger
}

// NewRuntimeMessageExecutor creates a new runtime message executor
func NewRuntimeMessageExecutor(sendFn func(ctx context.Context, content string, attachments []string) error, log *slog.Logger) *RuntimeMessageExecutor {
	return &RuntimeMessageExecutor{
		sendMessage: sendFn,
		log:         log,
	}
}

// Execute sends the message to the runtime for execution
func (rme *RuntimeMessageExecutor) Execute(ctx context.Context, msg store.QueueMessage) error {
	// Parse attachments if present
	var attachments []string
	if msg.Attachments != nil {
		attachments = msg.Attachments
	}

	// Send message to runtime
	return rme.sendMessage(ctx, msg.Content, attachments)
}
