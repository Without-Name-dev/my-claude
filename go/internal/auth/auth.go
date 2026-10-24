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

// Secret returns the HMAC key. A deployment that forgets to set it falls back
// to the development value, matching the Python behaviour, so tokens issued
// before the environment was configured stay valid.
func Secret() []byte {
	value := os.Getenv("AGENT_SERVER_SECRET")
	if value == "" {
		value = "agent-server-dev-secret"
	}
	return []byte(value)
}

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

// HashPassword returns "pbkdf2_sha256$<salt>$<digest>". An empty salt asks for
// a fresh random one; passing a salt reproduces a stored hash for comparison.
func HashPassword(password, salt string) (string, error) {
	if salt == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		salt = base64.RawURLEncoding.EncodeToString(buf)
	}
	digest := pbkdf2.Key([]byte(password), []byte(salt), iterations(), sha256.Size, sha256.New)
	return "pbkdf2_sha256$" + salt + "$" + b64(digest), nil
}

// PasswordSalt extracts the salt from a stored hash, or "" when the stored
// value is not in a format this package understands.
func PasswordSalt(stored string) string {
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 || parts[0] != "pbkdf2_sha256" {
		return ""
	}
	return parts[1]
}

// VerifyPassword compares a candidate password against a stored hash.
func VerifyPassword(password, stored string) bool {
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	actual, err := HashPassword(password, parts[1])
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(stored)) == 1
}

// PasswordVerifierHash wraps a client-computed verifier so the server never
// sees the password itself.
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
	parts := strings.SplitN(stored, "$", 3)
	if len(parts) != 3 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(verifier), []byte(parts[2])) == 1
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

// CreateUserAccessToken returns the bearer a signed-in client stores.
func CreateUserAccessToken(userID string, expiresIn int) (string, error) {
	header, err := jsonB64(map[string]any{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := jsonB64(map[string]any{
		"sub": userID,
		"exp": time.Now().Unix() + int64(expiresIn),
	})
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
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return "", ErrInvalidToken
	}
	mac := hmac.New(sha256.New, Secret())
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := b64(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return "", ErrInvalidToken
	}
	raw, err := b64Decode(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", ErrInvalidToken
	}
	if exp, ok := numberAsInt64(data["exp"]); !ok || exp < time.Now().Unix() {
		return "", ErrInvalidToken
	}
	subject, _ := data["sub"].(string)
	if subject == "" {
		return "", ErrInvalidToken
	}
	return subject, nil
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
