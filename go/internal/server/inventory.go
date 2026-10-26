package server

import (
	"context"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// applyInventory turns the connector's transcript scan into projects and
// sessions the person can open.
//
// Each distinct working directory becomes a project, because that is how a
// person thinks about their Claude work: "the repo I was in". Sessions keep the
// connector's own id so --resume still works.
func (a *App) applyInventory(ctx context.Context, conn *connectorConn, inv protocol.Inventory) {
	created := 0
	updated := 0
	existingSessions, err := a.db.SessionsForConnector(ctx, conn.id)
	if err != nil {
		a.log.Warn("could not read connector sessions", "connector", conn.id, "error", err)
	}
	existing := make(map[string]store.Session, len(existingSessions))
	for _, session := range existingSessions {
		existing[session.ExternalID] = session
	}
	for _, record := range inv.Sessions {
		if record.ExternalID == "" {
			continue
		}
		_, existed := existing[record.ExternalID]
		projectID := ""
		if record.CWD != "" {
			project, err := a.db.EnsureProject(ctx, conn.userID, conn.id, record.CWD, "", false)
			if err != nil {
				a.log.Debug("could not ensure project", "cwd", record.CWD, "error", err)
			} else {
				projectID = project.ID
			}
		}
		session, err := a.db.UpsertDiscoveredSession(ctx, conn.id, projectID,
			record.ExternalID, record.Title, record.CWD, record.UpdatedAt)
		if err != nil {
			a.log.Warn("could not record discovered session", "error", err)
			continue
		}
		updated++
		if !existed {
			created++
		}
		// A full inventory can follow a disconnect while the server missed
		// transcript changes. The connector deduplicates unchanged versions.
		if existed {
			a.requestChangedHistory(session, maxHistoryRequest)
		}
	}
	for _, externalID := range inv.Removed {
		if err := a.db.MarkSessionMissing(ctx, conn.id, externalID); err != nil {
			a.log.Debug("could not mark a session missing", "external", externalID, "error", err)
			continue
		}
		updated++
	}

	if updated > 0 {
		a.log.Info("inventory applied", "sessions", updated, "created", created, "full", inv.Full)
		a.dashboards.notify(conn.userID)
	}
}
