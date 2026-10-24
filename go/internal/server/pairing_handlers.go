package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handlePairingStart issues a short code a connector prints for the person to
// enter in the app.
func (a *App) handlePairingStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerURL  *string `json:"serverUrl"`
		TTLSeconds int     `json:"ttlSeconds"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	ttl := body.TTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	expires := time.Now().Add(time.Duration(ttl) * time.Second).UnixMilli()

	var serverURL any
	if body.ServerURL != nil && *body.ServerURL != "" {
		serverURL = *body.ServerURL
	}

	pairingID := store.NewID("pair")
	code, err := a.uniquePairingCode(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pairing_codes (id, code, status, server_url, expires_at, created_at)
			VALUES (?, ?, 'pending', ?, ?, ?)`,
			pairingID, code, serverURL, expires, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"pairingId":  pairingID,
		"code":       code,
		"expiresAt":  isoTime(expires),
		"serverTime": timestampNow(),
	})
}

// handlePairingClaim binds a pending code to the signed-in person and creates
// the connector.
func (a *App) handlePairingClaim(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Code      string  `json:"code"`
		Name      string  `json:"name"`
		ServerURL *string `json:"serverUrl"`
		// Older clients send these when re-claiming an existing connector;
		// they are accepted and ignored, which is what the Python server did.
		ConnectorID    *string `json:"connectorId"`
		ConnectorToken *string `json:"connectorToken"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		httpx.Fail(w, httpx.BadRequest("code is required"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Agents Anywhere Connector"
	}

	now := time.Now().UnixMilli()
	var pairingID string
	var expires int64
	err := a.db.Read().QueryRowContext(r.Context(), `
		SELECT id, expires_at FROM pairing_codes
		WHERE code = ? AND status = 'pending'`, code).Scan(&pairingID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, httpx.NotFound("code not found"))
		return
	}
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if expires < now {
		httpx.Fail(w, httpx.Conflict("code has expired"))
		return
	}

	token := NewToken("con")
	connector, err := a.db.CreateConnector(r.Context(), store.Connector{
		UserID:      user.ID,
		Name:        name,
		Kind:        "cli",
		TokenHash:   HashToken(token),
		TokenPrefix: tokenPrefix(token),
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Claiming is a compare-and-swap: two people entering the same code must
	// not both get a connector, so the update only lands while it is pending.
	var affected int64
	err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes
			SET status = 'claimed', connector_id = ?, connector_token = ?, claimed_at = ?
			WHERE id = ? AND status = 'pending'`,
			connector.ID, token, now, pairingID)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if affected == 0 {
		httpx.Fail(w, httpx.Conflict("code was already claimed"))
		return
	}

	httpx.OK(w, map[string]any{
		"status":     "claimed",
		"connector":  a.connectorView(connector),
		"serverTime": timestampNow(),
	})
}

// handlePairingPoll is how the waiting connector learns it was claimed. The
// configuration is handed out once; afterwards the code reports "consumed" so a
// leaked pairing id cannot mint a second token.
func (a *App) handlePairingPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PairingID string `json:"pairingId"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	var status string
	var connectorID, connectorToken, serverURL sql.NullString
	var expires sql.NullInt64
	var consumed sql.NullInt64
	err := a.db.Read().QueryRowContext(r.Context(), `
		SELECT status, connector_id, connector_token, server_url, expires_at, consumed_at
		FROM pairing_codes WHERE id = ?`, body.PairingID).
		Scan(&status, &connectorID, &connectorToken, &serverURL, &expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, httpx.NotFound("pairing not found"))
		return
	}
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	if status == "pending" && expires.Valid && expires.Int64 < time.Now().UnixMilli() {
		status = "expired"
	}

	response := map[string]any{"status": status, "config": nil, "expiresAt": nil}
	if expires.Valid {
		response["expiresAt"] = isoTime(expires.Int64)
	}
	if status == "claimed" && !consumed.Valid {
		// Mark consumed first: if the response is lost the connector can simply
		// be paired again, which is safer than handing the same token out twice.
		err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				UPDATE pairing_codes SET status = 'consumed', consumed_at = ?
				WHERE id = ? AND consumed_at IS NULL`, time.Now().UnixMilli(), body.PairingID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errors.New("already consumed")
			}
			return nil
		})
		if err != nil {
			httpx.Fail(w, httpx.Conflict("pairing configuration already handed out"))
			return
		}
		url := a.cfg.PublicURL
		if serverURL.Valid && serverURL.String != "" {
			url = serverURL.String
		}
		// The caller sees the same "claimed" answer it would have seen before
		// the row moved on; only a *later* poll reports "consumed".
		response["config"] = map[string]any{
			"serverUrl":      url,
			"connectorId":    connectorID.String,
			"connectorToken": connectorToken.String,
		}
	}
	httpx.OK(w, response)
}

// uniquePairingCode finds a six-digit code no pending pairing holds.
func (a *App) uniquePairingCode(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 20; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			return "", err
		}
		code := leftPad6(n.Int64())
		var existing string
		err = a.db.Read().QueryRowContext(ctx,
			`SELECT id FROM pairing_codes WHERE code = ? AND status = 'pending'`, code).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return code, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a pairing code")
}

func leftPad6(value int64) string {
	digits := []byte("000000")
	for i := 5; i >= 0 && value > 0; i-- {
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits)
}
