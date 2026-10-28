package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// historyCooldown keeps a client that keeps refetching an empty session from
// asking the connector every few hundred milliseconds.
const historyCooldown = 20 * time.Second

// historyRefreshInterval is how often a live conversation's transcript is
// pulled in.
//
// A prompt typed on the desktop never passes through the phone, so the
// transcript is the only place the server can learn about it. That copy used to
// be imported once, when the session was first opened, and the phone never saw
// anything typed afterwards. The connector skips a transcript whose size and
// mtime have not changed, so a quiet session costs one stat.
const historyRefreshInterval = 15 * time.Second

// historyRefreshLimit bounds one periodic import.
const historyRefreshLimit = 80

// historyItemPublishLimit is the number of imported rows above which a full
// timeline reset is published instead of one event per item.
//
// Large imports use one reset to avoid sending hundreds of individual events.
// Smaller incremental updates keep the reader's loaded window in place.
const historyItemPublishLimit = 80

// refreshLiveHistories keeps the server's timeline in step with every live
// conversation's transcript.
func (a *App) refreshLiveHistories(ctx context.Context) {
	ticker := time.NewTicker(historyRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for connectorID, live := range a.connectors.LiveByConnector() {
			if !a.connectors.Online(connectorID) {
				continue
			}
			sessions, err := a.db.SessionsForConnector(ctx, connectorID)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if session.ExternalID == "" {
					continue
				}
				if a.turns.activeForSession(session.ID) {
					// A turn this server is following writes its own rows. A
					// turn started in the terminal has no such watcher, so it
					// still needs the poll — skipping every "running" session
					// hid desktop work from the phone until it finished.
					continue
				}
				if _, ok := live[session.ExternalID]; !ok {
					continue
				}
				// Only the tail is needed: the refresh exists to pick up what
				// was typed since the last look, and a smaller window keeps a
				// long conversation cheap to re-import.
				a.requestHistory(ctx, session, historyRefreshLimit, false, false)
			}
		}
	}
}

// maybeRequestHistory requests a full reconciliation when a session is opened.
// The connector skips a version it has already sent and the server committed.
func (a *App) maybeRequestHistory(ctx context.Context, session store.Session) {
	items, err := a.db.TimelineItemsLatest(ctx, a.db.Read(), session.ID, 1)
	_, dirty, dirtyErr := a.db.HistoryImportState(ctx, a.db.Read(), session.ID)
	if dirtyErr != nil {
		a.log.Warn("could not read transcript reconciliation state", "session", session.ID, "error", dirtyErr)
	}
	force := err != nil || dirtyErr != nil || dirty || len(items) == 0
	a.requestHistory(ctx, session, maxHistoryRequest, true, force)
}

// requestHistory sends one transcript request with an explicit window.
func (a *App) requestHistory(ctx context.Context, session store.Session, limit int, full, force bool) {
	if session.ExternalID == "" || !a.connectors.Online(session.ConnectorID) {
		return
	}
	if !a.history.allow(session.ID, historyCooldown) {
		return
	}
	a.sendHistoryRequest(session, limit, full, force)
}

// requestChangedHistory bypasses the periodic request limiter because a new
// transcript version is a one-shot signal for a session that may stop being
// live before the next poll.
func (a *App) requestChangedHistory(ctx context.Context, session store.Session, limit int, full bool) {
	if session.ExternalID == "" || !a.connectors.Online(session.ConnectorID) {
		return
	}
	items, err := a.db.TimelineItemsLatest(ctx, a.db.Read(), session.ID, 1)
	force := err != nil || len(items) == 0
	a.sendHistoryRequest(session, limit, full || force, force)
}

func (a *App) sendHistoryRequest(session store.Session, limit int, full, force bool) {
	a.connectors.Send(session.ConnectorID, protocol.TypeHistoryRequest, protocol.HistoryRequest{
		SessionID:  session.ID,
		ExternalID: session.ExternalID,
		Limit:      limit,
		Full:       full,
		Force:      force,
	})
}

// maxHistoryRequest mirrors the connector's own cap.
const maxHistoryRequest = 400

// historyLimiter tracks the last request per session. Several sockets may ask
// for the same empty session at once, so the map is guarded.
type historyLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newHistoryLimiter() *historyLimiter {
	return &historyLimiter{last: map[string]time.Time{}}
}

func (l *historyLimiter) allow(sessionID string, cooldown time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[sessionID]; ok && now.Sub(at) < cooldown {
		return false
	}
	l.last[sessionID] = now
	return true
}

// applyHistory reconciles a transcript with what the session already shows.
//
// The connector answers with a window of the conversation, not the whole
// transcript, so items missing from it are not deleted. Existing rows are only
// rewritten when the transcript really differs: an unchanged item keeps its
// revision, and a changed one keeps the position it already had, so a resync
// cannot reorder or duplicate a conversation the person is reading.
//
// The whole import shares one revision: it is a single state change, and one
// snapshot event tells every open client to replace its timeline in one go.
// knownMessages counts messages already stored outside transcript imports.
// Counts preserve repeated identical prompts while still matching the copy
// Claude writes for messages sent through this server.
func (a *App) knownMessages(ctx context.Context, sessionID string) (map[string]int, []string, error) {
	known := map[string]int{}
	var assistants []string
	items, err := a.db.TimelineNonHistoryMessages(ctx, a.db.Read(), sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, item := range items {
		if role, text, ok := messageIdentity(item.Role, item.Payload); ok {
			known[role+"|"+text]++
			if role == "assistant" && text != "" {
				assistants = append(assistants, text)
			}
		}
	}
	return known, assistants, nil
}

// assistantPrefixLength is how much of a live assistant message is indexed for
// the fragment check. Sixteen characters are enough to make an accidental
// collision between two different messages implausible.
const assistantPrefixLength = 16

// assistantPrefixIndex keys live assistant messages by their opening text.
func assistantPrefixIndex(texts []string) map[string][]string {
	index := make(map[string][]string, len(texts))
	for _, text := range texts {
		if len(text) < assistantPrefixLength {
			continue
		}
		key := text[:assistantPrefixLength]
		index[key] = append(index[key], text)
	}
	return index
}

// isAssistantFragment reports whether a transcript message is one model step of
// a turn the server already recorded in full.
//
// Claude writes one assistant record per model step, so a single reply that
// used tools appears as several records: the opening prose, then the rest. The
// live turn recorded the whole reply as one row. Importing the opening as its
// own message put the start of the reply a second time, below the reply — the
// conversation looked like it had reordered itself.
func isAssistantFragment(text string, index map[string][]string) bool {
	if len(text) < assistantPrefixLength {
		return false
	}
	for _, existing := range index[text[:assistantPrefixLength]] {
		if len(existing) > len(text) && strings.HasPrefix(existing, text) {
			return true
		}
	}
	return false
}

// The connector prepends this block to the text it actually runs, so the
// transcript copy of a message with attachments differs from the plain text the
// server stored for the phone. Comparing them verbatim imported every such
// message a second time, permanently.
const (
	attachedFilesHeader   = "[attached files from the My Claude app"
	attachmentFailureHead = "[attachments that could not be delivered"
	// attachmentOnlyIdentity stands in for a message that was nothing but
	// attachments, so its stored row and its transcript copy still match.
	attachmentOnlyIdentity = "\x00attachments"
)

// stripAttachmentPrompt removes the connector's attachment preamble and returns
// only the words the person typed.
func stripAttachmentPrompt(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	isHeader := strings.HasPrefix(trimmed, attachedFilesHeader) ||
		strings.HasPrefix(trimmed, attachmentFailureHead)
	if !isHeader {
		return text
	}
	// The connector separates its blocks from the person's text with a blank
	// line; everything before that belongs to the attachment list.
	if index := strings.Index(trimmed, "\n\n"); index >= 0 {
		rest := strings.TrimSpace(trimmed[index+2:])
		if rest != "" {
			return rest
		}
	}
	return attachmentOnlyIdentity
}

// messageIdentity renders a message row as the key the importer compares.
// Whitespace is normalised so a re-wrapped transcript still matches.
func messageIdentity(role string, payload json.RawMessage) (string, string, bool) {
	if role == "" || len(payload) == 0 {
		return "", "", false
	}
	var decoded struct {
		Text        string            `json:"text"`
		Attachments []json.RawMessage `json:"attachments"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", "", false
	}
	text := decoded.Text
	if role == "user" {
		text = stripAttachmentPrompt(text)
	}
	if strings.TrimSpace(text) == "" && len(decoded.Attachments) > 0 {
		text = attachmentOnlyIdentity
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", "", false
	}
	return role, text, true
}

func (a *App) applyHistory(ctx context.Context, conn *connectorConn, msg protocol.History) bool {
	session, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID)
	if err != nil {
		a.log.Warn("history for an unknown session", "session", msg.SessionID)
		return false
	}
	fullImport := msg.Full && msg.Complete != nil
	if len(msg.Items) == 0 && !fullImport {
		return true
	}
	// Messages the server already recorded from the phone (or from a turn it
	// followed) must not be imported again: the transcript copy has a different
	// id, so only the role and the text identify it.
	known := map[string]int{}
	var assistantIndex map[string][]string
	var seq int64
	changed := 0
	written := make([]store.TimelineItem, 0, len(msg.Items))
	created := make([]bool, 0, len(msg.Items))
	err = a.db.WithSession(session.ID, func() error {
		if len(msg.Items) > 0 {
			var assistantTexts []string
			var err error
			known, assistantTexts, err = a.knownMessages(ctx, session.ID)
			if err != nil {
				return err
			}
			assistantIndex = assistantPrefixIndex(assistantTexts)
		}
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			rows := make([]store.TimelineItem, 0, len(msg.Items))
			for _, item := range msg.Items {
				row := historyItemRow(session.ID, item, 0)
				if role, text, ok := messageIdentity(item.Role, row.Payload); ok {
					key := role + "|" + text
					if known[key] > 0 {
						known[key]--
						continue
					}
					// One model step of a reply the server already recorded in
					// full: importing it would repeat the opening below the
					// reply itself.
					if role == "assistant" && isAssistantFragment(text, assistantIndex) {
						continue
					}
				}
				existing, err := a.db.TimelineItemByID(ctx, tx, session.ID, item.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				isNew := errors.Is(err, store.ErrNotFound)
				if err == nil {
					if historyItemUnchanged(existing, row) {
						continue
					}
					// A turn still streaming owns its row; its turn.end writes
					// the final text and a transcript copy must not race it.
					if a.turns.hasItem(item.ID) {
						continue
					}
					row.OrderSeq = existing.OrderSeq
					if existing.ItemTime != nil {
						row.ItemTime = existing.ItemTime
					}
				}
				rows = append(rows, row)
				created = append(created, isNew)
			}
			if len(rows) == 0 {
				return nil
			}
			allocated, err := store.AllocateSeq(ctx, tx, session.ID, store.NowMS())
			if err != nil {
				return err
			}
			seq = allocated
			for _, row := range rows {
				row.UpdatedSeq = allocated
				if err := store.UpsertTimelineItem(ctx, tx, row, store.NowMS()); err != nil {
					return err
				}
				written = append(written, row)
			}
			changed = len(rows)
			if fullImport && changed > 0 {
				if err := store.MarkHistoryImportDirty(ctx, tx, session.ID, store.NowMS()); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		a.log.Warn("could not store history", "error", err)
		return false
	}
	if fullImport {
		if !*msg.Complete {
			return true
		}
		generation, publishReset, err := a.db.HistoryImportState(ctx, a.db.Read(), session.ID)
		if err != nil {
			a.log.Warn("could not read transcript reconciliation state", "session", session.ID, "error", err)
			return false
		}
		if !publishReset {
			a.log.Debug("full transcript already in sync", "session", session.ID, "items", len(msg.Items))
			return true
		}
		if seq == 0 {
			seq = a.currentSeq(ctx, session.ID)
		}
		if !a.publishHistoryReset(ctx, session, seq) {
			return false
		}
		if err := a.db.ClearHistoryImportDirty(ctx, session.ID, generation); err != nil {
			a.log.Warn("could not clear transcript reconciliation state", "session", session.ID, "error", err)
			return false
		}
		return true
	}
	if changed == 0 {
		a.log.Debug("history already in sync", "session", session.ID, "items", len(msg.Items))
		return true
	}

	// A periodic refresh usually brings one or two messages. Publishing those
	// as item events keeps the phone's list intact; a full timeline reset for
	// every import made the page rebuild and jump while the reader was
	// scrolling. A first import (a whole page) still resets, which is cheaper
	// than hundreds of events and is the one case where the list is new anyway.
	if changed <= historyItemPublishLimit {
		for i, row := range written {
			a.publishItem(ctx, session.ID, row.ID, row.UpdatedSeq, created[i])
		}
		a.log.Info("imported transcript", "session", session.ID, "items", changed, "mode", "items")
		return true
	}

	return a.publishHistoryReset(ctx, session, seq)
}

func (a *App) publishHistoryReset(ctx context.Context, session store.Session, seq int64) bool {
	items, hasMore, err := a.timelineOlderPage(ctx, session.ID, 0, 500)
	if err != nil {
		return false
	}
	views := make([]ItemView, 0, len(items))
	for _, item := range items {
		views = append(views, toView(item, session.Runtime))
	}
	// A 500-item reset is a megabyte or more of JSON; one frame of that size
	// exceeds the clients' 1 MiB message limit and kills the whole stream.
	// Clients merge reset frames, so publish it in bounded chunks.
	const chunkBytes = 512 << 10
	publish := func(chunk []ItemView) bool {
		frame, err := newEnvelope(session.ID, seq, EventTimelineReset, map[string]any{
			"items":   chunk,
			"hasMore": hasMore,
		})
		if err != nil {
			return false
		}
		a.broker.Publish(session.ID, frame)
		return true
	}
	chunk := make([]ItemView, 0, 64)
	size := 0
	for _, view := range views {
		encoded, err := json.Marshal(view)
		if err == nil && size+len(encoded) > chunkBytes {
			if !publish(chunk) {
				return false
			}
			chunk = make([]ItemView, 0, 64)
			size = 0
		}
		chunk = append(chunk, view)
		size += len(encoded)
	}
	if len(chunk) > 0 {
		if !publish(chunk) {
			return false
		}
	} else if len(views) == 0 {
		if !publish(nil) {
			return false
		}
	}
	// The import allocated a revision, and AllocateSeq also moves the session's
	// seq, sort key and last-activity time. Opening a discovered conversation
	// must therefore reorder the list and flip its unread flag, which only a
	// dashboard snapshot can express. A transcript import is an inventory
	// change even though its payload is a timeline reset.
	a.notifySessionDashboard(ctx, session.ID)
	a.log.Info("imported transcript", "session", session.ID, "items", len(views), "mode", "reset")
	return true
}

// historyItemUnchanged reports whether the stored row already carries the
// transcript's state, so a resync does not bump a revision nobody can see.
//
// Position is not compared: the stored row already has one, and a transcript
// timestamp only decides where a brand-new item goes.
func historyItemUnchanged(existing, incoming store.TimelineItem) bool {
	return existing.Type == incoming.Type &&
		existing.Status == incoming.Status &&
		existing.Role == incoming.Role &&
		contentHash(existing.Payload) == contentHash(incoming.Payload)
}

// historyItemRow maps one transcript item onto the stored shape.
func historyItemRow(sessionID string, item protocol.HistoryItem, seq int64) store.TimelineItem {
	itemTime := item.Timestamp
	if itemTime == 0 {
		itemTime = store.NowMS()
	}
	var payload json.RawMessage
	if item.Type == "tool" {
		payload = toolContent(item.Tool, item.Input, item.Output, "")
	} else {
		payload = messageContent(item.Text)
	}
	return store.TimelineItem{
		SessionID:  sessionID,
		ID:         item.ID,
		Type:       item.Type,
		Status:     item.Status,
		Role:       item.Role,
		OrderSeq:   itemTime,
		UpdatedSeq: seq,
		ItemTime:   &itemTime,
		Payload:    payload,
	}
}
