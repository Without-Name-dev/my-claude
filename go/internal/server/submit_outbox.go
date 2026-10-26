package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const submitRetryInterval = 15 * time.Second

type submitFingerprint struct {
	Text        string                      `json:"text"`
	Action      string                      `json:"action"`
	Attachments []protocol.SubmitAttachment `json:"attachments,omitempty"`
}

func fingerprintSubmit(msg protocol.Submit) (string, error) {
	body, err := json.Marshal(submitFingerprint{
		Text:        msg.Text,
		Action:      msg.Action,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}

func encodeSubmitFrame(msg protocol.Submit) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Envelope{Type: protocol.TypeSubmit, Data: data})
}

// persistSubmit writes the pending user item and its retryable connector frame
// in one transaction. Request identity is scoped to the owning session.
func (a *App) persistSubmit(
	ctx context.Context,
	session store.Session,
	msg protocol.Submit,
	frame []byte,
	payloadHash string,
	item store.TimelineItem,
	previousStatus string,
) (int64, bool, store.SubmitOutboxItem, error) {
	var seq int64
	var inserted bool
	var existing store.SubmitOutboxItem
	err := a.db.WithSession(session.ID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			found, err := a.db.SubmitOutboxByID(ctx, tx, session.ID, msg.RequestID)
			if err == nil {
				existing = found
				if found.PayloadHash != payloadHash {
					return store.ErrSubmitRequestConflict
				}
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}

			now := store.NowMS()
			seq, err = store.AllocateSeq(ctx, tx, session.ID, now)
			if err != nil {
				return err
			}
			orderSeq, err := store.NextTimelineOrderSeq(ctx, tx, session.ID, now)
			if err != nil {
				return err
			}
			item.OrderSeq = orderSeq
			item.ItemTime = &item.OrderSeq
			item.UpdatedSeq = seq
			if err := store.UpsertTimelineItem(ctx, tx, item, now); err != nil {
				return err
			}
			if err := store.InsertSubmitOutbox(ctx, tx, store.SubmitOutboxItem{
				SessionID:         session.ID,
				RequestID:         msg.RequestID,
				ConnectorID:       session.ConnectorID,
				ExternalSessionID: session.ExternalID,
				Payload:           frame,
				PayloadHash:       payloadHash,
				PreviousStatus:    previousStatus,
				EnqueueSeq:        seq,
				CreatedAt:         now,
				UpdatedAt:         now,
			}); err != nil {
				return err
			}
			inserted = true
			return nil
		})
	})
	return seq, inserted, existing, err
}

// replayPendingSubmits sends durable rows to a connector that advertised
// submit acknowledgements. A recent attempt is retried on reconnect or after
// the bounded retry interval while the connector remains online.
func (a *App) replayPendingSubmits(ctx context.Context, connectorID string, force bool) {
	if connectorID == "" || !a.connectors.SupportsSubmitAck(connectorID) {
		return
	}
	a.submitReplayMu.Lock()
	defer a.submitReplayMu.Unlock()

	cutoff := time.Now().Add(-submitRetryInterval).UnixMilli()
	if force {
		cutoff = math.MaxInt64
	}
	items, err := a.db.PendingSubmitOutbox(ctx, connectorID, cutoff, 256)
	if err != nil {
		a.log.Warn("could not read pending connector submits", "connector", connectorID, "error", err)
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UnixMilli()
		marked, err := a.db.MarkSubmitAttempt(ctx, item.SessionID, item.RequestID, now)
		if err != nil {
			a.log.Warn("could not record connector submit attempt", "session", item.SessionID, "request", item.RequestID, "error", err)
			continue
		}
		if !marked || len(item.Payload) == 0 {
			continue
		}
		if !a.connectors.SendRaw(connectorID, item.Payload) {
			a.log.Debug("connector submit remains pending", "connector", connectorID, "session", item.SessionID, "request", item.RequestID)
			return
		}
	}
}

func (a *App) applySubmitAck(ctx context.Context, conn *connectorConn, ack protocol.SubmitAck) {
	if ack.SessionID == "" || ack.RequestID == "" {
		return
	}
	if ack.Accepted {
		err := a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, _, err := store.ResolveSubmitAck(ctx, tx, conn.id, ack.SessionID,
				ack.ExternalSessionID, ack.RequestID, true, "", store.NowMS())
			return err
		})
		if err != nil {
			a.log.Warn("could not commit submit acknowledgement", "session", ack.SessionID, "request", ack.RequestID, "error", err)
		}
		return
	}

	var seq int64
	var itemID string
	var previousStatus string
	err := a.db.WithSession(ack.SessionID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			item, applied, err := store.ResolveSubmitAck(ctx, tx, conn.id, ack.SessionID,
				ack.ExternalSessionID, ack.RequestID, false, ack.Error, store.NowMS())
			if err != nil || !applied {
				return err
			}
			previousStatus = item.PreviousStatus
			seq, err = store.AllocateSeq(ctx, tx, ack.SessionID, store.NowMS())
			if err != nil {
				return err
			}
			itemID, err = a.updateSubmittedMessageStatus(ctx, tx, ack.SessionID, ack.RequestID, "failed", seq)
			return err
		})
	})
	if err != nil {
		a.log.Warn("could not commit rejected connector submit", "session", ack.SessionID, "request", ack.RequestID, "error", err)
		return
	}
	if itemID != "" {
		a.publishItem(ctx, ack.SessionID, itemID, seq, false)
	}
	if previousStatus != "" {
		if a.hasPendingUserMessages(ctx, ack.SessionID) {
			previousStatus = "waiting"
		}
		a.setSessionStatus(ctx, ack.SessionID, previousStatus)
	}
	if ack.Error != "" {
		a.log.Info("connector rejected submit", "connector", conn.id, "session", ack.SessionID, "request", ack.RequestID, "error", ack.Error)
	}
}

func (a *App) existingSubmit(ctx context.Context, sessionID, requestID, payloadHash string) (bool, store.SubmitOutboxItem, error) {
	item, err := a.db.SubmitOutboxByID(ctx, a.db.Read(), sessionID, requestID)
	if errors.Is(err, store.ErrNotFound) {
		return false, store.SubmitOutboxItem{}, nil
	}
	if err != nil {
		return false, store.SubmitOutboxItem{}, err
	}
	if item.PayloadHash != payloadHash {
		return true, item, httpx.Conflict("clientMessageId was already used for different message content")
	}
	if item.State == "failed" {
		return true, item, httpx.Conflict("the connector permanently rejected this message")
	}
	return true, item, nil
}
