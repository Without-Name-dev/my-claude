// Package auth holds the token and password primitives shared by the server.
//
// The formats mirror the Python implementation this replaces: connector access
// tokens are HMAC-signed "id.expiry.signature" strings, user and signed tokens
// are JWT-shaped (base64url body + detached signature), and passwords are
// PBKDF2-SHA256 in a self-describing "$"-delimited string. Keeping the shapes
// means an old client that stored a token or a password hash keeps working.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// Default lifetimes. The server issues shorter connector tokens because the
// connector refreshes them; user tokens are long-lived because phones keep them.
const (
	DefaultConnectorExpiresIn = 900
	DefaultUserExpiresIn      = 60 * 60 * 24 * 7
	defaultIterations         = 120_000
)

var ErrInvalidToken = errors.New("invalid token")

// Secret returns the HMAC key.
//
// A deployment with no key must not verify against a value anyone can read in
// the source, so the fallback is only the historical development constant when
// the process explicitly opted in with AA_ALLOW_DEV_SECRET=1. Otherwise the key
// is generated per process: tokens then stop verifying after a restart, which
// is the safe failure for an unconfigured instance (the server refuses to start
// in this state anyway).
func Secret() []byte {
	value := os.Getenv("AGENT_SERVER_SECRET")
	if value != "" {
		return []byte(value)
	}
	if os.Getenv("AA_ALLOW_DEV_SECRET") == "1" {
		return []byte("agent-server-dev-secret")
	}
	fallbackSecretOnce.Do(func() {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand.Read never fails on supported platforms; if it does,
			// there is no safe key to hand out.
			panic("auth: no entropy source for the signing key")
		}
		fallbackSecret = buf
	})
	return fallbackSecret
}

var (
	fallbackSecretOnce sync.Once
	fallbackSecret     []byte
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64Decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func iterations() int {
	if raw := os.Getenv("AGENT_SERVER_PASSWORD_ITERATIONS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultIterations
}

// HashPassword returns "pbkdf2_sha256$<iterations>$<salt>$<digest>". An empty
// salt asks for a fresh random one; passing a salt reproduces a stored hash for
// comparison. The cost is part of the stored value so raising the configured
// cost does not invalidate every existing password.
func HashPassword(password, salt string) (string, error) {
	if salt == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		salt = base64.RawURLEncoding.EncodeToString(buf)
	}
	return hashPasswordWithCost(password, salt, iterations()), nil
}

func hashPasswordWithCost(password, salt string, cost int) string {
	digest := pbkdf2.Key([]byte(password), []byte(salt), cost, sha256.Size, sha256.New)
	return "pbkdf2_sha256$" + strconv.Itoa(cost) + "$" + salt + "$" + b64(digest)
}

// parsePasswordHash splits a stored credential. Four parts carry the PBKDF2
// cost; three parts are the older shape, whose cost was whatever the process
// was configured with, so they verify with the current default.
func parsePasswordHash(stored string) (cost int, salt, digest string, ok bool) {
	parts := strings.Split(stored, "$")
	if len(parts) == 4 && parts[0] == "pbkdf2_sha256" {
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 {
			return 0, "", "", false
		}
		return n, parts[2], parts[3], true
	}
	if len(parts) == 3 && parts[0] == "pbkdf2_sha256" {
		return iterations(), parts[1], parts[2], true
	}
	return 0, "", "", false
}

// PasswordSalt extracts the salt from a stored hash, or "" when the stored
// value is not in a format this package understands.
func PasswordSalt(stored string) string {
	_, salt, _, ok := parsePasswordHash(stored)
	if !ok {
		return ""
	}
	return salt
}

// NeedsRehash reports whether a stored password should be rewritten at the
// current cost. Legacy three-part hashes cannot prove which cost they used, so
// they are rewritten once the plaintext is available at login.
func NeedsRehash(stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) == 3 && parts[0] == "pbkdf2_sha256" {
		return true
	}
	if len(parts) == 4 && parts[0] == "pbkdf2_sha256" {
		n, err := strconv.Atoi(parts[1])
		return err == nil && n < iterations()
	}
	return false
}

// VerifyPassword compares a candidate password against a stored hash.
func VerifyPassword(password, stored string) bool {
	cost, salt, digest, ok := parsePasswordHash(stored)
	if !ok {
		return false
	}
	actual := b64(pbkdf2.Key([]byte(password), []byte(salt), cost, sha256.Size, sha256.New))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(digest)) == 1
}

// PasswordVerifierHash wraps a client-computed verifier so the server never
// sees the password itself.
//
// The verifier was derived with the client's own cost, which the server cannot
// know, so this keeps the three-part shape and stores the digest as sent.
func PasswordVerifierHash(verifier, salt string) (string, error) {
	if verifier == "" {
		return "", errors.New("password verifier is required")
	}
	if salt == "" {
		return "", errors.New("password salt is required")
	}
	return "pbkdf2_sha256$" + salt + "$" + verifier, nil
}

// VerifyPasswordVerifier checks a client-computed verifier against storage.
func VerifyPasswordVerifier(verifier, stored string) bool {
	_, _, expected, ok := parsePasswordHash(stored)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(verifier), []byte(expected)) == 1
}

// jsonB64 encodes a JSON object with sorted keys and no spaces, so the signed
// bytes are identical no matter which field a caller set first.
func jsonB64(value map[string]any) (string, error) {
	keys := make([]string, 0, len(value))
	for k := range value {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(value[k])
		if err != nil {
			return "", err
		}
		sb.Write(key)
		sb.WriteByte(':')
		sb.Write(raw)
	}
	sb.WriteByte('}')
	return b64([]byte(sb.String())), nil
}

// CredentialVersion fingerprints a stored credential. A token carries the
// fingerprint it was minted against, so changing the password (or resetting it)
// changes the fingerprint and invalidates every older token.
func CredentialVersion(storedHash string) string {
	sum := sha256.Sum256([]byte(storedHash))
	return b64(sum[:16])
}

// UserToken is the verified content of a user bearer.
type UserToken struct {
	Subject string
	// CredentialVersion is empty for tokens minted before the claim existed.
	CredentialVersion string
}

// CreateUserAccessToken returns the bearer a signed-in client stores.
func CreateUserAccessToken(userID string, expiresIn int) (string, error) {
	return CreateUserAccessTokenVersion(userID, "", expiresIn)
}

// CreateUserAccessTokenVersion mints a bearer bound to a stored credential.
func CreateUserAccessTokenVersion(userID, credentialVersion string, expiresIn int) (string, error) {
	header, err := jsonB64(map[string]any{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims := map[string]any{
		"sub": userID,
		"exp": time.Now().Unix() + int64(expiresIn),
	}
	if credentialVersion != "" {
		claims["cv"] = credentialVersion
	}
	payload, err := jsonB64(claims)
	if err != nil {
		return "", err
	}
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, Secret())
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64(mac.Sum(nil)), nil
}

// VerifyUserAccessToken returns the subject of a valid, unexpired token.
func VerifyUserAccessToken(token string) (string, error) {
	info, err := VerifyUserAccessTokenInfo(token)
	if err != nil {
		return "", err
	}
	return info.Subject, nil
}

// VerifyUserAccessTokenInfo returns the subject and credential fingerprint of
// a valid, unexpired token.
func VerifyUserAccessTokenInfo(token string) (UserToken, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return UserToken{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, Secret())
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := b64(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return UserToken{}, ErrInvalidToken
	}
	raw, err := b64Decode(parts[1])
	if err != nil {
		return UserToken{}, ErrInvalidToken
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return UserToken{}, ErrInvalidToken
	}
	if exp, ok := numberAsInt64(data["exp"]); !ok || exp < time.Now().Unix() {
		return UserToken{}, ErrInvalidToken
	}
	subject, _ := data["sub"].(string)
	if subject == "" {
		return UserToken{}, ErrInvalidToken
	}
	version, _ := data["cv"].(string)
	return UserToken{Subject: subject, CredentialVersion: version}, nil
}

// CreateSignedToken signs a short-lived single-purpose token (download links,
// websocket tickets, email codes). kind keeps them from being interchangeable.
func CreateSignedToken(kind string, payload map[string]any, expiresIn int) (string, error) {
	body := map[string]any{"kind": kind, "exp": time.Now().Unix() + int64(expiresIn)}
	for k, v := range payload {
		body[k] = v
	}
	signingInput, err := jsonB64(body)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, Secret())
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64(mac.Sum(nil)), nil
}

// VerifySignedToken returns the payload of a valid token of the given kind.
func VerifySignedToken(kind, token string) (map[string]any, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, Secret())
	mac.Write([]byte(parts[0]))
	expected := b64(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[1])) != 1 {
		return nil, ErrInvalidToken
	}
	raw, err := b64Decode(parts[0])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, ErrInvalidToken
	}
	if got, _ := data["kind"].(string); got != kind {
		return nil, ErrInvalidToken
	}
	if exp, ok := numberAsInt64(data["exp"]); !ok || exp < time.Now().Unix() {
		return nil, ErrInvalidToken
	}
	return data, nil
}

func numberAsInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
