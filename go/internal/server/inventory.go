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
	// Transcripts retired by a /clear still sit on disk. The conversation has
	// already moved to the new id; discovering the old one again would add a
	// duplicate session for the same conversation.
	superseded, err := a.db.SupersededExternals(ctx, conn.id)
	if err != nil {
		a.log.Debug("could not read rebind history", "connector", conn.id, "error", err)
	}
	for _, record := range inv.Sessions {
		if record.ExternalID == "" || superseded[record.ExternalID] {
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
		// Changed records refresh the recent tail. A full transcript is only
		// requested when a person opens the session: asking for it here
		// re-uploaded hundreds of items every minute for an active
		// conversation and published a timeline reset with every full pass,
		// which is what made the phone's sync crawl.
		if existed {
			a.requestChangedHistory(ctx, session, historyRefreshLimit, false)
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
