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

// Pairing codes are globally unique and the start endpoint is anonymous, so
// the TTL is bounded and each source gets a small budget: an unbounded TTL or
// unlimited starts would let one caller occupy the code space.
const (
	pairingDefaultTTL = 600
	pairingMinTTL     = 60
	pairingMaxTTL     = 1800

	pairingStartWindow = 10 * time.Minute
	pairingStartLimit  = 30
	// How long an expired code keeps answering "expired" before its row is
	// removed and the code becomes allocatable again.
	pairingExpiredGrace = time.Hour
	pairingStaleGrace   = 24 * time.Hour
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
	startKey := "pairing:ip:" + clientIP(r)
	if retry := loginAttempts.blocked(startKey, pairingStartLimit); retry > 0 {
		respondTooManyAttempts(w, retry)
		return
	}

	ttl := body.TTLSeconds
	if ttl <= 0 {
		ttl = pairingDefaultTTL
	}
	if ttl < pairingMinTTL {
		ttl = pairingMinTTL
	}
	if ttl > pairingMaxTTL {
		ttl = pairingMaxTTL
	}
	expires := time.Now().Add(time.Duration(ttl) * time.Second).UnixMilli()

	var serverURL any
	if body.ServerURL != nil && *body.ServerURL != "" {
		serverURL = *body.ServerURL
	}

	a.reapPairingCodes(r.Context())

	// The code column is unique across every status, so a pre-flight lookup of
	// pending rows is not enough: a previously claimed or expired row can hold
	// the same digits. Insert and retry on the constraint instead.
	var pairingID, code string
	for attempt := 0; attempt < 20; attempt++ {
		generated, err := randomPairingCode()
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		candidateID := store.NewID("pair")
		err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO pairing_codes (id, code, status, server_url, expires_at, created_at)
				VALUES (?, ?, 'pending', ?, ?, ?)`,
				candidateID, generated, serverURL, expires, time.Now().UnixMilli())
			return err
		})
		if err == nil {
			pairingID, code = candidateID, generated
			break
		}
		if !isUniqueViolation(err) {
			httpx.Fail(w, err)
			return
		}
	}
	if code == "" {
		httpx.Fail(w, errors.New("could not allocate a pairing code"))
		return
	}
	loginAttempts.record(startKey, pairingStartWindow)

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

	token, err := newToken("con")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	connector := store.Connector{
		ID:          store.NewID("con"),
		UserID:      user.ID,
		Name:        name,
		Kind:        "cli",
		Status:      "offline",
		TokenHash:   HashToken(token),
		TokenPrefix: tokenPrefix(token),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// The app creates a connector before the person chooses how to pair. When
	// they pick the CLI code, that placeholder is reused here: creating a
	// second one left an offline "New device" orphan in the list. Only a
	// connector that has never connected and is not revoked is eligible.
	reuseID := ""
	if body.ConnectorID != nil {
		if candidate, err := a.db.ConnectorByID(r.Context(), strings.TrimSpace(*body.ConnectorID)); err == nil &&
			candidate.UserID == user.ID && candidate.Revoked == 0 && candidate.LastSeenAt == nil {
			reuseID = candidate.ID
		}
	}
	if reuseID != "" {
		connector.ID = reuseID
		connector.CreatedAt = now
	}

	// The connector insert and the compare-and-swap share one transaction.
	// The insert has to come first because pairing_codes.connector_id is a
	// foreign key, but a lost race still leaves nothing behind: returning
	// errPairingRace rolls the insert back with the rest of the transaction.
	err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if reuseID != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE connectors
				SET name = ?, connector_kind = ?, status = 'offline',
				    token_hash = ?, token_prefix = ?, revoked = 0, updated_at = ?
				WHERE id = ? AND user_id = ? AND revoked = 0 AND last_seen_at IS NULL`,
				connector.Name, connector.Kind, connector.TokenHash, connector.TokenPrefix,
				now, reuseID, user.ID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `
			INSERT INTO connectors (id, user_id, name, connector_kind, device_os, status,
			                        token_hash, token_prefix, revoked, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULL, 'offline', ?, ?, 0, ?, ?)`,
			connector.ID, connector.UserID, connector.Name, connector.Kind,
			connector.TokenHash, connector.TokenPrefix, now, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE pairing_codes
			SET status = 'claimed', connector_id = ?, connector_token = ?, claimed_at = ?
			WHERE id = ? AND status = 'pending' AND expires_at >= ?`,
			connector.ID, token, now, pairingID, now)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return errPairingRace
		}
		return nil
	})
	if errors.Is(err, errPairingRace) {
		if expires < time.Now().UnixMilli() {
			httpx.Fail(w, httpx.Conflict("code has expired"))
			return
		}
		httpx.Fail(w, httpx.Conflict("code was already claimed"))
		return
	}
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	httpx.OK(w, map[string]any{
		"status":     "claimed",
		"connector":  a.connectorView(connector),
		"serverTime": timestampNow(),
	})
}

// errPairingRace means the compare-and-swap lost: the code was expired or
// claimed between the lookup and the update.
var errPairingRace = errors.New("pairing code is no longer available")

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

// randomPairingCode draws a six-digit code. Uniqueness is settled by the
// database constraint at insert time, not by a lookup that another request can
// invalidate before the row is written.
func randomPairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return leftPad6(n.Int64()), nil
}

// reapPairingCodes frees codes that can no longer be claimed. Expired codes
// stay readable for a grace period so a waiting connector still sees "expired"
// rather than "not found", then their rows (and therefore their code values)
// are released.
func (a *App) reapPairingCodes(ctx context.Context) {
	now := time.Now().UnixMilli()
	expiredBefore := now - int64(pairingExpiredGrace/time.Millisecond)
	staleBefore := now - int64(pairingStaleGrace/time.Millisecond)
	err := a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM pairing_codes
			WHERE (status IN ('pending', 'expired') AND expires_at < ?)
			   OR (status = 'claimed' AND claimed_at < ?)
			   OR (status = 'consumed' AND consumed_at < ?)`,
			expiredBefore, staleBefore, staleBefore)
		return err
	})
	if err != nil {
		// Reaping is housekeeping; a failure must not stop a valid start.
		a.log.Error("could not reap pairing codes", "error", err)
	}
}

func leftPad6(value int64) string {
	digits := []byte("000000")
	for i := 5; i >= 0 && value > 0; i-- {
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits)
}
