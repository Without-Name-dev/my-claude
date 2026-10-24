package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// errNotFound is the sentinel handlers compare against; the store's own value
// keeps a single definition.
var errNotFound = store.ErrNotFound

// connectorRecord is the identity behind a connector socket.
type connectorRecord struct {
	ID     string
	UserID string
	Name   string
}

// HashToken returns the stored form of a bearer token. Tokens are random and
// high-entropy, so a plain SHA-256 is enough; there is nothing to brute-force.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a bearer with a recognisable prefix.
func NewToken(prefix string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + hex.EncodeToString([]byte("fallback-token"))
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

// NewConnectionID identifies one socket, so a reconnect cannot be marked
// offline by the cleanup of the socket it replaced.
func NewConnectionID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "conn-fallback"
	}
	return hex.EncodeToString(buf)
}

// bearerToken reads the Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// userFromRequest resolves the signed-in user from the bearer token.
func (a *App) userFromRequest(r *http.Request) (store.User, bool) {
	token := bearerToken(r)
	if token == "" {
		return store.User{}, false
	}
	userID, err := VerifyUserToken(token)
	if err != nil {
		return store.User{}, false
	}
	user, err := a.db.UserByID(r.Context(), userID)
	if err != nil || user.Disabled {
		return store.User{}, false
	}
	return user, true
}

// requireUser wraps a handler with authentication.
func (a *App) requireUser(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.userFromRequest(r)
		if !ok {
			httpx.Fail(w, httpx.Unauthorized("sign in required"))
			return
		}
		next(w, r, user)
	}
}

// ctxWithUser stores the authenticated user for handlers that need it deeper
// than the signature allows.
type ctxKey struct{ name string }

var userKey = ctxKey{name: "user"}

func withUser(ctx context.Context, user store.User) context.Context {
	return context.WithValue(ctx, userKey, user)
}

func userFromCtx(ctx context.Context) (store.User, bool) {
	user, ok := ctx.Value(userKey).(store.User)
	return user, ok
}

// ErrUnavailable is returned when the target connector has no live socket.
var ErrUnavailable = errors.New("connector is offline")
