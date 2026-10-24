package server

import (
	"context"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// ConnectorView is the connector as clients render it.
type ConnectorView struct {
	ID            string  `json:"id"`
	UserID        string  `json:"userId"`
	Name          string  `json:"name"`
	ConnectorKind string  `json:"connectorKind"`
	DeviceOS      *string `json:"deviceOs"`
	Status        string  `json:"status"`
	LastSeenAt    *string `json:"lastSeenAt"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

// connectorView renders a connector, taking liveness from the hub rather than
// the stored row: a socket that died without a clean close leaves the row
// saying "online" until the next heartbeat sweep.
func (a *App) connectorView(c store.Connector) ConnectorView {
	status := "offline"
	if a.connectors.Online(c.ID) {
		status = "online"
	}
	var deviceOS *string
	if c.DeviceOS != "" {
		value := c.DeviceOS
		deviceOS = &value
	}
	var lastSeen *string
	if c.LastSeenAt != nil {
		value := isoTime(*c.LastSeenAt)
		lastSeen = &value
	}
	return ConnectorView{
		ID:            c.ID,
		UserID:        c.UserID,
		Name:          c.Name,
		ConnectorKind: c.Kind,
		DeviceOS:      deviceOS,
		Status:        status,
		LastSeenAt:    lastSeen,
		CreatedAt:     isoTime(c.CreatedAt),
		UpdatedAt:     isoTime(c.UpdatedAt),
	}
}

// SessionView is the session as the dashboard and the phone render it.
type SessionView struct {
	ID                          string  `json:"id"`
	ConnectorID                 string  `json:"connectorId"`
	ProjectID                   *string `json:"projectId"`
	ConnectorStatus             string  `json:"connectorStatus"`
	Runtime                     string  `json:"runtime"`
	RuntimeID                   string  `json:"runtimeId"`
	RuntimeType                 *string `json:"runtimeType"`
	RuntimeName                 *string `json:"runtimeName"`
	RuntimeTypeDisplayName      *string `json:"runtimeTypeDisplayName"`
	ExternalSessionID           *string `json:"externalSessionId"`
	Title                       *string `json:"title"`
	CWD                         *string `json:"cwd"`
	Status                      string  `json:"status"`
	Takeover                    bool    `json:"takeover"`
	Pinned                      bool    `json:"pinned"`
	PinnedAt                    *string `json:"pinnedAt"`
	Archived                    bool    `json:"archived"`
	ArchivedAt                  *string `json:"archivedAt"`
	UserArchived                bool    `json:"userArchived"`
	SourceAvailability          string  `json:"sourceAvailability"`
	SourceAvailabilityReason    *string `json:"sourceAvailabilityReason"`
	SourceAvailabilityUpdatedAt *string `json:"sourceAvailabilityUpdatedAt"`
	SourceObservationOrigin     *string `json:"sourceObservationOrigin"`
	ArchiveSource               *string `json:"archiveSource"`
	Unread                      bool    `json:"unread"`
	LastReadSeq                 int64   `json:"lastReadSeq"`
	LatestTurnEndSeq            int64   `json:"latestTurnEndSeq"`
	LastSyncedAt                *string `json:"lastSyncedAt"`
	SourceObservedAt            *string `json:"sourceObservedAt"`
	LastActivityAt              *string `json:"lastActivityAt"`
	LastItemAt                  *string `json:"lastItemAt"`
	LastItemOrderSeq            *int64  `json:"lastItemOrderSeq"`
	SortAt                      *string `json:"sortAt"`
	UpdatedSeq                  int64   `json:"updatedSeq"`
}

// sessionView renders a stored session.
func (a *App) sessionView(ctx context.Context, s store.Session) SessionView {
	connectorStatus := "offline"
	if a.connectors.Online(s.ConnectorID) {
		connectorStatus = "online"
	}
	view := SessionView{
		ID:                 s.ID,
		ConnectorID:        s.ConnectorID,
		ConnectorStatus:    connectorStatus,
		Runtime:            s.Runtime,
		RuntimeID:          s.RuntimeID,
		Status:             s.Status,
		Takeover:           s.Takeover,
		Pinned:             s.Pinned,
		Archived:           s.Archived,
		UserArchived:       s.Archived,
		SourceAvailability: availabilityOf(s.SourceState),
		Unread:             s.Seq > s.LastReadSeq,
		LastReadSeq:        s.LastReadSeq,
		LatestTurnEndSeq:   s.LastTurnEnd,
		UpdatedSeq:         s.UpdatedSeq,
	}
	runtime := s.Runtime
	view.RuntimeType = &runtime
	view.RuntimeName = nil
	if s.ProjectID != "" {
		value := s.ProjectID
		view.ProjectID = &value
	}
	if s.ExternalID != "" {
		value := s.ExternalID
		view.ExternalSessionID = &value
	}
	if s.Title != "" {
		value := s.Title
		view.Title = &value
	}
	if s.CWD != "" {
		value := s.CWD
		view.CWD = &value
	}
	if s.LastActivity != nil {
		value := isoTime(*s.LastActivity)
		view.LastActivityAt = &value
	}
	sortAt := isoTime(s.SortAt)
	view.SortAt = &sortAt
	return view
}

// availabilityOf maps the stored source state onto what clients expect.
func availabilityOf(sourceState string) string {
	switch sourceState {
	case "archived", "unavailable", "deleted", "missing", "unknown", "available":
		return sourceState
	default:
		return "available"
	}
}
