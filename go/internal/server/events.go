package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// Event types the session websocket carries.
const (
	EventSubscribed     = "session.subscribed"
	EventItemCreated    = "timeline.item_created"
	EventItemUpdated    = "timeline.item_updated"
	EventTimelineReset  = "timeline.snapshot"
	EventSessionMeta    = "session.meta.updated"
	EventRuntimeState   = "runtime.state.updated"
	EventNoticeUpdated  = "runtime.notice.updated"
	EventNoticeSnapshot = "runtime.notice.snapshot"
	EventRefetch        = "session.refetch_required"
	EventKeepalive      = "keepalive"
)

// Envelope is the frame every persistent session event uses.
//
// seq is the session revision, not a per-event counter: one change may emit
// several events under the same sequence. eventId is derived from the sequence
// and the content, so a replayed event is byte-identical and a client can
// deduplicate on it.
type Envelope struct {
	ProtocolVersion string          `json:"protocolVersion"`
	EventID         string          `json:"eventId"`
	Sequence        int64           `json:"sequence"`
	Cursor          string          `json:"cursor"`
	Type            string          `json:"type"`
	SessionID       string          `json:"sessionId"`
	EmittedAt       string          `json:"emittedAt"`
	Payload         json.RawMessage `json:"payload"`
}

// Cursor renders a sequence the way clients parse it.
func Cursor(seq int64) string { return "seq:" + strconv.FormatInt(seq, 10) }

// ParseCursor reads a "seq:N" cursor.
func ParseCursor(value string) (int64, bool) {
	if len(value) < 4 || value[:4] != "seq:" {
		return 0, false
	}
	n, err := strconv.ParseInt(value[4:], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// newEnvelope builds one event frame.
func newEnvelope(sessionID string, seq int64, eventType string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte(eventType), raw...))
	env := Envelope{
		ProtocolVersion: "1.0",
		EventID:         fmt.Sprintf("evt_%d_%s", seq, hex.EncodeToString(sum[:])[:12]),
		Sequence:        seq,
		Cursor:          Cursor(seq),
		Type:            eventType,
		SessionID:       sessionID,
		EmittedAt:       timestampNow(),
		Payload:         raw,
	}
	return json.Marshal(env)
}

// timestampNow renders the ISO form clients parse.
func timestampNow() string { return isoTime(time.Now().UnixMilli()) }

func isoTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000000Z")
}

// ItemView is a timeline item as clients receive it.
type ItemView struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"sessionId"`
	Type        string          `json:"type"`
	Status      string          `json:"status"`
	Role        *string         `json:"role"`
	Content     json.RawMessage `json:"content"`
	Source      SourceView      `json:"source"`
	OrderSeq    int64           `json:"orderSeq"`
	Revision    int64           `json:"revision"`
	ContentHash string          `json:"contentHash"`
	CreatedAt   string          `json:"createdAt"`
	UpdatedAt   string          `json:"updatedAt"`
	CompletedAt *string         `json:"completedAt"`
	UpdatedSeq  int64           `json:"updatedSeq"`
}

// SourceView says where an item came from.
type SourceView struct {
	Runtime         string  `json:"runtime"`
	SessionID       *string `json:"sessionId,omitempty"`
	ItemID          *string `json:"itemId,omitempty"`
	ItemType        *string `json:"itemType,omitempty"`
	Event           *string `json:"event,omitempty"`
	DerivedKey      *string `json:"derivedKey,omitempty"`
	ClientMessageID *string `json:"clientMessageId,omitempty"`
}

// toView renders a stored item.
func toView(it store.TimelineItem, runtime string) ItemView {
	var role *string
	if it.Role != "" {
		r := it.Role
		role = &r
	}
	updated := isoTime(it.UpdatedSeq) // replaced below; sequence is not a clock
	_ = updated
	ts := it.OrderSeq
	created := isoTime(ts)
	completed := (*string)(nil)
	if it.Status == "done" || it.Status == "failed" || it.Status == "cancelled" {
		c := created
		completed = &c
	}
	return ItemView{
		ID:          it.ID,
		SessionID:   it.SessionID,
		Type:        it.Type,
		Status:      it.Status,
		Role:        role,
		Content:     it.Payload,
		Source:      sourceFor(it, runtime),
		OrderSeq:    it.OrderSeq,
		Revision:    it.UpdatedSeq,
		ContentHash: contentHash(it.Payload),
		CreatedAt:   created,
		UpdatedAt:   created,
		CompletedAt: completed,
		UpdatedSeq:  it.UpdatedSeq,
	}
}

// sourceFor says where an item came from.
//
// A message the person sent carries the id their client generated, so the
// optimistic bubble they already drew is replaced instead of duplicated. The
// id is encoded in the item id, which avoids a schema change for one field.
func sourceFor(it store.TimelineItem, runtime string) SourceView {
	source := SourceView{Runtime: runtime}
	if strings.HasPrefix(it.ID, "msg_") && strings.HasSuffix(it.ID, "_user") {
		id := strings.TrimSuffix(strings.TrimPrefix(it.ID, "msg_"), "_user")
		if id != "" {
			source.ClientMessageID = &id
		}
	}
	return source
}

// contentHash identifies a payload for "did this actually change" checks.
//
// Keys are sorted so two encoders that differ only in map iteration order
// produce the same hash.
func contentHash(payload []byte) string {
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])[:16]
	}
	canonical, err := canonicalJSON(value)
	if err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])[:16]
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16]
}

// canonicalJSON re-encodes a decoded value with sorted object keys.
func canonicalJSON(value any) ([]byte, error) {
	var sb []byte
	var walk func(v any) error
	walk = func(v any) error {
		switch typed := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for k := range typed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			sb = append(sb, '{')
			for i, k := range keys {
				if i > 0 {
					sb = append(sb, ',')
				}
				key, err := json.Marshal(k)
				if err != nil {
					return err
				}
				sb = append(sb, key...)
				sb = append(sb, ':')
				if err := walk(typed[k]); err != nil {
					return err
				}
			}
			sb = append(sb, '}')
		case []any:
			sb = append(sb, '[')
			for i, item := range typed {
				if i > 0 {
					sb = append(sb, ',')
				}
				if err := walk(item); err != nil {
					return err
				}
			}
			sb = append(sb, ']')
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			sb = append(sb, raw...)
		}
		return nil
	}
	if err := walk(value); err != nil {
		return nil, err
	}
	return sb, nil
}
