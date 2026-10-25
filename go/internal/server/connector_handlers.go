package server

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleListConnectors returns the signed-in person's live connectors.
func (a *App) handleListConnectors(w http.ResponseWriter, r *http.Request, user store.User) {
	connectors, err := a.db.ListConnectors(r.Context(), user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views := make([]ConnectorView, 0, len(connectors))
	for _, c := range connectors {
		views = append(views, a.connectorView(c))
	}
	httpx.OK(w, map[string]any{"connectors": views, "serverTime": timestampNow()})
}

// handleCreateConnector mints a connector and its token.
func (a *App) handleCreateConnector(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Name           string  `json:"name"`
		ConnectorKind  string  `json:"connectorKind"`
		InstallationID *string `json:"installationId"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Agents Anywhere Connector"
	}
	kind := body.ConnectorKind
	if kind != "desktop" {
		kind = "cli"
	}

	token := NewToken("con")
	connector, err := a.db.CreateConnector(r.Context(), store.Connector{
		UserID:      user.ID,
		Name:        name,
		Kind:        kind,
		TokenHash:   HashToken(token),
		TokenPrefix: tokenPrefix(token),
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"connector":      a.connectorView(connector),
		"connectorToken": token,
		"tokenPrefix":    tokenPrefix(token),
	})
}

// handleGetConnector returns one connector belonging to the caller.
func (a *App) handleGetConnector(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{"connector": a.connectorView(connector), "serverTime": timestampNow()})
}

// handlePatchConnector renames a connector.
func (a *App) handlePatchConnector(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Name *string `json:"name"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			httpx.Fail(w, httpx.BadRequest("name must not be empty"))
			return
		}
		if err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE connectors SET name = ?, updated_at = ? WHERE id = ?`,
				name, time.Now().UnixMilli(), connector.ID)
			return err
		}); err != nil {
			httpx.Fail(w, err)
			return
		}
		connector.Name = name
	}
	httpx.OK(w, map[string]any{"connector": a.connectorView(connector), "serverTime": timestampNow()})
}

// handleRevokeConnector rotates the token and drops the live socket.
func (a *App) handleRevokeConnector(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	token := NewToken("con")
	if err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE connectors SET token_hash = ?, token_prefix = ?, updated_at = ?
			WHERE id = ?`, HashToken(token), tokenPrefix(token), time.Now().UnixMilli(), connector.ID)
		return err
	}); err != nil {
		httpx.Fail(w, err)
		return
	}
	a.disconnectConnector(connector.ID)
	connector.TokenHash = HashToken(token)
	httpx.OK(w, map[string]any{
		"connector":      a.connectorView(connector),
		"connectorToken": token,
		"tokenPrefix":    tokenPrefix(token),
		"serverTime":     timestampNow(),
	})
}

// handleDeleteConnector removes a connector and everything under it.
func (a *App) handleDeleteConnector(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var files []store.AttachmentFileRef
	if err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// Collect the attachment files before the sessions (and their index
		// rows) are gone. The blobs live outside SQLite, so they are removed
		// only after this transaction commits.
		var err error
		files, err = a.db.AttachmentFilesForConnector(ctx, tx, connector.ID)
		if err != nil {
			return err
		}
		// Sessions and projects reference the connector; delete them first so
		// the foreign keys stay satisfied without a cascade surprise.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE connector_id = ?`, connector.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM projects WHERE connector_id = ?`, connector.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM connectors WHERE id = ?`, connector.ID)
		return err
	}); err != nil {
		httpx.Fail(w, err)
		return
	}
	for _, file := range files {
		if err := store.DeleteAttachment(a.cfg.UploadDir, file.SessionID, file.FileID); err != nil {
			a.log.Warn("could not delete an attachment",
				"session", file.SessionID, "file", file.FileID, "error", err)
		}
	}
	a.disconnectConnector(connector.ID)
	httpx.NoContent(w)
}

// ownedConnector resolves the path connector and refuses one that belongs to
// somebody else, answering 404 rather than 403 so existence is not leaked.
func (a *App) ownedConnector(w http.ResponseWriter, r *http.Request, user store.User) (store.Connector, bool) {
	id := r.PathValue("connector_id")
	connector, err := a.db.ConnectorByID(r.Context(), id)
	if err != nil || connector.UserID != user.ID || connector.Revoked != 0 {
		httpx.Fail(w, httpx.NotFound("connector not found"))
		return store.Connector{}, false
	}
	return connector, true
}

// disconnectConnector closes a connector's socket after its token changed or it
// was deleted, so the desktop stops using a credential that no longer works.
func (a *App) disconnectConnector(connectorID string) {
	a.connectors.Send(connectorID, "close", map[string]any{"reason": "revoked"})
	conn := a.connectors.drop(connectorID)
	if conn == nil {
		return
	}
	// The socket's own cleanup runs after the hub already removed it, so it
	// publishes nothing. These views have to be republished here, or an open
	// page keeps showing the green dot of a connector that cannot deliver.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.db.SetConnectorPresence(ctx, connectorID, conn.connectionID, false); err != nil {
		a.log.Warn("could not clear connector presence", "connector", connectorID, "error", err)
	}
	a.publishConnectorPresence(ctx, conn.userID, connectorID, false)
}

func tokenPrefix(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}
