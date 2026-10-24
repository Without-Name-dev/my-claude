package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OAuthClient is a registered OAuth client. First-party apps are seeded at
// startup; the table exists so authorization codes can carry a foreign key.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// AllowsRedirect reports whether uri is one of the client's registered
// redirect targets.
func (c OAuthClient) AllowsRedirect(uri string) bool {
	for _, candidate := range c.RedirectURIs {
		if candidate == uri {
			return true
		}
	}
	return false
}

// firstPartyClients are the redirect schemes the shipped apps listen on.
var firstPartyClients = []OAuthClient{
	{
		ID:           "agents-anywhere-mobile",
		Name:         "Agents Anywhere Mobile",
		RedirectURIs: []string{"agents-anywhere://oauth/callback"},
	},
	{
		ID:           "agents-anywhere-desktop",
		Name:         "Agents Anywhere Desktop",
		RedirectURIs: []string{"agents-anywhere-desktop://oauth/callback"},
	},
}

// EnsureFirstPartyOAuthClients inserts the built-in clients if they are absent.
func (s *Store) EnsureFirstPartyOAuthClients(ctx context.Context) error {
	now := time.Now().UnixMilli()
	for _, c := range firstPartyClients {
		uris, err := json.Marshal(c.RedirectURIs)
		if err != nil {
			return err
		}
		if _, err := s.write.ExecContext(ctx, `
			INSERT INTO oauth_clients (id, name, redirect_uris_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				redirect_uris_json = excluded.redirect_uris_json,
				updated_at = excluded.updated_at`,
			c.ID, c.Name, string(uris), now, now); err != nil {
			return fmt.Errorf("seed oauth client %s: %w", c.ID, err)
		}
	}
	return nil
}

// OAuthClient loads a registered client by id.
func (s *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	var client OAuthClient
	var uris string
	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, redirect_uris_json FROM oauth_clients WHERE id = ?`, id).
		Scan(&client.ID, &client.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, err
	}
	if err := json.Unmarshal([]byte(uris), &client.RedirectURIs); err != nil {
		return OAuthClient{}, fmt.Errorf("decode redirect uris for %s: %w", id, err)
	}
	return client, nil
}

// OAuthAuthorizationCode describes a code about to be issued.
type OAuthAuthorizationCode struct {
	ClientID            string
	UserID              string
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// oauthCodeTTL is how long the app has to redeem the code it was redirected
// with. The redirect happens immediately, so this only guards against a stale
// browser tab.
const oauthCodeTTL = 10 * time.Minute

func hashOAuthCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CreateOAuthAuthorizationCode stores the hashed code and returns the plaintext
// one, which is the only copy that ever leaves the server.
func (s *Store) CreateOAuthAuthorizationCode(ctx context.Context, c OAuthAuthorizationCode) (string, error) {
	code := NewID("oac")
	now := time.Now().UnixMilli()
	if _, err := s.write.ExecContext(ctx, `
		INSERT INTO oauth_authorization_codes
			(code_hash, client_id, user_id, redirect_uri, scope, code_challenge,
			 code_challenge_method, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashOAuthCode(code), c.ClientID, c.UserID, c.RedirectURI, c.Scope,
		c.CodeChallenge, c.CodeChallengeMethod, now+oauthCodeTTL.Milliseconds(), now,
	); err != nil {
		return "", err
	}
	return code, nil
}

// ErrCodeAlreadyUsed and ErrCodeExpired tell the token endpoint why a code was
// rejected.
var (
	ErrCodeAlreadyUsed = errors.New("authorization code already used")
	ErrCodeExpired     = errors.New("authorization code expired")
)

// ConsumeOAuthAuthorizationCode redeems a code exactly once. It returns the user
// the code was issued for along with the PKCE challenge to verify.
func (s *Store) ConsumeOAuthAuthorizationCode(ctx context.Context, code, clientID, redirectURI string) (userID, scope, challenge, method string, err error) {
	hash := hashOAuthCode(code)
	now := time.Now().UnixMilli()

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", "", err
	}
	defer tx.Rollback()

	var expiresAt int64
	var consumedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, scope, code_challenge, code_challenge_method, expires_at, consumed_at
		FROM oauth_authorization_codes
		WHERE code_hash = ? AND client_id = ? AND redirect_uri = ?`,
		hash, clientID, redirectURI,
	).Scan(&userID, &scope, &challenge, &method, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", "", err
	}
	if consumedAt.Valid {
		return "", "", "", "", ErrCodeAlreadyUsed
	}
	if expiresAt <= now {
		return "", "", "", "", ErrCodeExpired
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_authorization_codes SET consumed_at = ? WHERE code_hash = ?`,
		now, hash); err != nil {
		return "", "", "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", "", "", err
	}
	return userID, scope, challenge, method, nil
}
