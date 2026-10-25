package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// avatarPattern is the only avatar shape the web client sends.
var avatarPattern = regexp.MustCompile(`^data:image/(png|jpeg|jpg|webp|gif);base64,[A-Za-z0-9+/=]+$`)

// Field caps shared by every endpoint that writes an account. They mirror the
// request models the reference server validates against.
const (
	maxEmailLength       = 320
	maxDisplayNameLength = 64
	maxCodeLength        = 6
)

// validateEmailAddress normalises an address and enforces its cap.
func validateEmailAddress(raw string) (string, error) {
	email := store.NormaliseEmail(raw)
	if email == "" || len(email) > maxEmailLength {
		return "", httpx.NewError(http.StatusUnprocessableEntity,
			"email must be 1..320 characters")
	}
	return email, nil
}

// validateDisplayName trims a name and enforces its cap.
func validateDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if len(name) < 1 || len(name) > maxDisplayNameLength {
		return "", httpx.NewError(http.StatusUnprocessableEntity,
			"displayName must be 1..64 characters")
	}
	return name, nil
}

// validateRole accepts only the two roles the database stores.
func validateRole(raw string) (string, error) {
	if raw != "admin" && raw != "member" {
		return "", httpx.NewError(http.StatusUnprocessableEntity,
			`role must be "admin" or "member"`)
	}
	return raw, nil
}

// authConfigResponse is what the client reads before showing a login form.
type authConfigResponse struct {
	EmailVerificationRequired bool    `json:"emailVerificationRequired"`
	NeedsBootstrap            bool    `json:"needsBootstrap"`
	RegistrationOpen          bool    `json:"registrationOpen"`
	OAuthRegistrationOpen     bool    `json:"oauthRegistrationOpen"`
	OAuthEnabled              bool    `json:"oauthEnabled"`
	OAuthProviderLabel        *string `json:"oauthProviderLabel"`
	PasswordResetEnabled      bool    `json:"passwordResetEnabled"`
	SetupTokenExpiresAt       *string `json:"setupTokenExpiresAt"`
	ServerTime                string  `json:"serverTime"`
}

// authResponse is returned by register, login and mobile exchange.
type authResponse struct {
	UserID        string  `json:"userId"`
	Email         *string `json:"email"`
	EmailVerified bool    `json:"emailVerified"`
	DisplayName   string  `json:"displayName"`
	Role          string  `json:"role"`
	AccessToken   string  `json:"accessToken"`
	TokenType     string  `json:"tokenType"`
	ServerTime    string  `json:"serverTime"`
}

// authMeResponse is the signed-in profile.
type authMeResponse struct {
	UserID        string  `json:"userId"`
	Email         *string `json:"email"`
	EmailVerified bool    `json:"emailVerified"`
	DisplayName   string  `json:"displayName"`
	Role          string  `json:"role"`
	Disabled      bool    `json:"disabled"`
	Avatar        *string `json:"avatar"`
	ServerTime    string  `json:"serverTime"`
}

// authRequest is the body register and login share.
type authRequest struct {
	Email            string  `json:"email"`
	DisplayName      *string `json:"displayName"`
	Code             *string `json:"code"`
	Password         *string `json:"password"`
	PasswordVerifier *string `json:"passwordVerifier"`
	PasswordSalt     *string `json:"passwordSalt"`
	SetupToken       *string `json:"setupToken"`
}

func (a *App) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	count, err := a.db.CountUsers(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	needsBootstrap := count == 0
	var expires *string
	if needsBootstrap {
		if exp, ok := a.setupTokenExpiry(r.Context()); ok {
			value := isoTime(exp)
			expires = &value
		}
	}
	stored := a.loadInstanceSettings(r.Context())
	// Same rule as the reference implementation: an enabled provider is only
	// usable once it names an authorize endpoint and a client id.
	oauthEnabled := stored.OAuth != nil && stored.OAuth.Enabled &&
		stored.OAuth.AuthorizeURL != "" && stored.OAuth.ClientID != ""
	var oauthLabel *string
	if oauthEnabled {
		label := "OAuth"
		if strings.TrimSpace(stored.OAuth.Label) != "" {
			label = stored.OAuth.Label
		}
		oauthLabel = &label
	}
	emailEnabled := stored.Email.Enabled
	httpx.OK(w, authConfigResponse{
		EmailVerificationRequired: emailEnabled,
		NeedsBootstrap:            needsBootstrap,
		// The first account must always be creatable, even if an administrator
		// closed registration before removing every user.
		RegistrationOpen:      stored.RegistrationOpen || needsBootstrap,
		OAuthRegistrationOpen: stored.OAuthRegistrationOpen,
		OAuthEnabled:          oauthEnabled,
		OAuthProviderLabel:    oauthLabel,
		PasswordResetEnabled:  emailEnabled && stored.PasswordResetEnabled,
		SetupTokenExpiresAt:   expires,
		ServerTime:            timestampNow(),
	})
}

// setupTokenExpiry reports when the bootstrap token stops being accepted.
func (a *App) setupTokenExpiry(ctx context.Context) (int64, bool) {
	raw, err := a.setting(ctx, "setup_token_expires_at")
	if err != nil || raw == "" {
		return 0, false
	}
	ms, ok := atoi(raw)
	if !ok {
		return 0, false
	}
	return ms, true
}

// handlePasswordSalt returns the salt login should hash with.
//
// An unknown address gets a deterministic salt derived from a server secret,
// so the response cannot be used to discover which addresses have accounts:
// repeated calls for the same address return the same value either way.
func (a *App) handlePasswordSalt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	salt := ""
	if user, err := a.db.UserByEmail(r.Context(), body.Email); err == nil {
		salt = auth.PasswordSalt(user.PasswordHash)
	}
	if salt == "" {
		salt = dummyPasswordSalt(body.Email)
	}
	httpx.OK(w, map[string]any{"salt": salt, "serverTime": timestampNow()})
}

// dummyPasswordSalt derives a stable stand-in salt. It is never used to store
// a credential; it only keeps an unknown address indistinguishable from a
// known one that happens to return a salt.
func dummyPasswordSalt(email string) string {
	mac := hmac.New(sha256.New, auth.Secret())
	mac.Write([]byte("password-salt:" + store.NormaliseEmail(email)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// handleRegister creates the first administrator or a later member.
func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body authRequest
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	email, err := validateEmailAddress(body.Email)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Code != nil && len(*body.Code) > maxCodeLength {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			"code must be at most 6 characters"))
		return
	}

	count, err := a.db.CountUsers(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if count > 0 && !a.loadInstanceSettings(r.Context()).RegistrationOpen {
		httpx.Fail(w, httpx.Forbidden("registration is closed"))
		return
	}
	role := "member"
	if count == 0 {
		role = "admin"
		ok, err := a.checkSetupToken(r.Context(), body.SetupToken)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		if !ok {
			httpx.Fail(w, httpx.Unauthorized("setup token is invalid or expired"))
			return
		}
	}

	hash, err := a.credentialHash(body)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	displayName := email
	if body.DisplayName != nil && strings.TrimSpace(*body.DisplayName) != "" {
		name, err := validateDisplayName(*body.DisplayName)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		displayName = name
	}

	user, err := a.db.CreateUser(r.Context(), store.User{
		Email:        email,
		DisplayName:  displayName,
		Role:         role,
		PasswordHash: hash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Same body as any other failure so the endpoint cannot be used to
			// ask whether an address already has an account.
			httpx.Fail(w, httpx.Conflict("registration could not be completed"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	if count == 0 {
		_ = a.clearSetting(r.Context(), "setup_token")
		_ = a.clearSetting(r.Context(), "setup_token_expires_at")
	}
	a.issueSession(w, user)
}

// handleLogin verifies the credential and returns a token.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body authRequest
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	// Each failed attempt spends 120k PBKDF2 rounds, so the endpoint is a
	// cheap way to pin a CPU. Reject known-bad sources before doing that work.
	// The account key stops a distributed guess against one address; the IP key
	// stops one source from spraying many addresses.
	accountKey := "login:account:" + store.NormaliseEmail(body.Email)
	ipKey := "login:ip:" + clientIP(r)
	if retry := loginAttempts.blocked(accountKey, loginAccountLimit); retry > 0 {
		respondTooManyAttempts(w, retry)
		return
	}
	if retry := loginAttempts.blocked(ipKey, loginIPLimit); retry > 0 {
		respondTooManyAttempts(w, retry)
		return
	}

	user, err := a.db.UserByEmail(r.Context(), body.Email)
	if err != nil {
		// An unknown account must cost the same PBKDF2 work as a wrong
		// password, or response time reveals which addresses exist.
		dummyCredentialCheck(body)
		loginAttempts.record(accountKey, loginAttemptWindow)
		loginAttempts.record(ipKey, loginAttemptWindow)
		httpx.Fail(w, httpx.Unauthorized("invalid credentials"))
		return
	}
	if !a.verifyCredential(user, body) {
		loginAttempts.record(accountKey, loginAttemptWindow)
		loginAttempts.record(ipKey, loginAttemptWindow)
		httpx.Fail(w, httpx.Unauthorized("invalid credentials"))
		return
	}
	if user.Disabled {
		httpx.Fail(w, httpx.Forbidden("account is disabled"))
		return
	}
	loginAttempts.reset(accountKey)

	// A hash from before the cost was stored verifies with the process default;
	// now that the plaintext is in hand, rewrite it with the current cost.
	if body.Password != nil && *body.Password != "" && auth.NeedsRehash(user.PasswordHash) {
		if rehashed, hashErr := auth.HashPassword(*body.Password, auth.PasswordSalt(user.PasswordHash)); hashErr == nil {
			if err := a.db.SetUserPassword(r.Context(), user.ID, rehashed); err == nil {
				user.PasswordHash = rehashed
			}
		}
	}
	a.issueSession(w, user)
}

// sessionTTL is the configured bearer lifetime, falling back to the built-in
// default when the environment leaves it unset.
func (a *App) sessionTTL() int {
	if a.cfg.SessionTTL > 0 {
		return int(a.cfg.SessionTTL.Seconds())
	}
	return auth.DefaultUserExpiresIn
}

// issueSession mints the bearer and writes the login response.
func (a *App) issueSession(w http.ResponseWriter, user store.User) {
	token, err := auth.CreateUserAccessTokenVersion(
		user.ID, auth.CredentialVersion(user.PasswordHash), a.sessionTTL())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var email *string
	if user.Email != "" {
		value := user.Email
		email = &value
	}
	httpx.OK(w, authResponse{
		UserID:        user.ID,
		Email:         email,
		EmailVerified: user.EmailVerifiedAt != nil,
		DisplayName:   user.DisplayName,
		Role:          user.Role,
		AccessToken:   token,
		TokenType:     "bearer",
		ServerTime:    timestampNow(),
	})
}

// handleMe returns the signed-in profile.
func (a *App) handleMe(w http.ResponseWriter, r *http.Request, user store.User) {
	httpx.OK(w, meView(user))
}

func meView(user store.User) authMeResponse {
	var email *string
	if user.Email != "" {
		value := user.Email
		email = &value
	}
	var avatar *string
	if user.Avatar != "" {
		value := user.Avatar
		avatar = &value
	}
	return authMeResponse{
		UserID:        user.ID,
		Email:         email,
		EmailVerified: user.EmailVerifiedAt != nil,
		DisplayName:   user.DisplayName,
		Role:          user.Role,
		Disabled:      user.Disabled,
		Avatar:        avatar,
		ServerTime:    timestampNow(),
	}
}

// handleUpdateProfile changes the display name.
func (a *App) handleUpdateProfile(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		DisplayName string `json:"displayName"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	name, err := validateDisplayName(body.DisplayName)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := a.db.UpdateUserProfile(r.Context(), user.ID, name, user.Avatar, user.Email); err != nil {
		httpx.Fail(w, err)
		return
	}
	user.DisplayName = name
	httpx.OK(w, meView(user))
}

// handleUpdateAvatar stores a data URL avatar.
func (a *App) handleUpdateAvatar(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Avatar string `json:"avatar"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if len(body.Avatar) > 262144 {
		httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "avatar is too large"))
		return
	}
	if !avatarPattern.MatchString(body.Avatar) {
		httpx.Fail(w, httpx.BadRequest("avatar must be a base64 image data URL"))
		return
	}
	if err := a.db.UpdateUserProfile(r.Context(), user.ID, user.DisplayName, body.Avatar, user.Email); err != nil {
		httpx.Fail(w, err)
		return
	}
	user.Avatar = body.Avatar
	httpx.OK(w, meView(user))
}

// handleDeleteAvatar clears it.
func (a *App) handleDeleteAvatar(w http.ResponseWriter, r *http.Request, user store.User) {
	if err := a.db.UpdateUserProfile(r.Context(), user.ID, user.DisplayName, "", user.Email); err != nil {
		httpx.Fail(w, err)
		return
	}
	user.Avatar = ""
	httpx.OK(w, meView(user))
}

// handleChangePassword replaces the credential and answers 204.
//
// The new hash changes the credential fingerprint the caller's token carries,
// so every token minted before this call stops working: a stolen bearer cannot
// outlive the password it was issued under.
func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		OldPassword         *string `json:"oldPassword"`
		OldPasswordVerifier *string `json:"oldPasswordVerifier"`
		NewPassword         *string `json:"newPassword"`
		NewPasswordVerifier *string `json:"newPasswordVerifier"`
		NewPasswordSalt     *string `json:"newPasswordSalt"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if !verifyCurrentCredential(user, body.OldPassword, body.OldPasswordVerifier) {
		httpx.Fail(w, httpx.Unauthorized("current password is incorrect"))
		return
	}
	hash, err := credentialHashFromParts(body.NewPassword, body.NewPasswordVerifier, body.NewPasswordSalt)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := a.db.SetUserPassword(r.Context(), user.ID, hash); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

// verifyCurrentCredential checks the current secret when the client sends one.
//
// The bearer has already authenticated the caller, and neither shipped client
// asks for the current password when changing it, so a request that omits both
// fields is accepted rather than rejected. A request that does send one must
// get it right.
func verifyCurrentCredential(user store.User, password, verifier *string) bool {
	if password != nil && *password != "" {
		return auth.VerifyPassword(*password, user.PasswordHash)
	}
	if verifier != nil && *verifier != "" {
		return auth.VerifyPasswordVerifier(*verifier, user.PasswordHash)
	}
	return true
}

// handleWSTicket mints a single-use websocket ticket.
func (a *App) handleWSTicket(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		ClientID string `json:"clientId"`
		Scope    *struct {
			SessionID *string `json:"sessionId"`
			Dashboard *bool   `json:"dashboard"`
		} `json:"scope"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Scope == nil {
		httpx.Fail(w, httpx.BadRequest("scope is required"))
		return
	}
	hasSession := body.Scope.SessionID != nil && *body.Scope.SessionID != ""
	hasDashboard := body.Scope.Dashboard != nil && *body.Scope.Dashboard
	if hasSession == hasDashboard {
		httpx.Fail(w, httpx.BadRequest("scope must name exactly one of sessionId or dashboard"))
		return
	}

	scope := TicketScope{Dashboard: hasDashboard}
	if hasSession {
		if _, err := a.db.SessionByID(r.Context(), a.db.Read(), *body.Scope.SessionID, user.ID); err != nil {
			httpx.Fail(w, httpx.NotFound("session not found"))
			return
		}
		scope.SessionID = *body.Scope.SessionID
	}

	ticket, expires := a.tickets.Issue(user.ID, body.ClientID, scope)
	httpx.OK(w, map[string]any{
		"ticket":     ticket,
		"expiresAt":  isoTime(expires.UnixMilli()),
		"serverTime": timestampNow(),
	})
}

// credentialHash builds the stored hash for a new password.
func (a *App) credentialHash(body authRequest) (string, error) {
	return credentialHashFromParts(body.Password, body.PasswordVerifier, body.PasswordSalt)
}

func credentialHashFromParts(password, verifier, salt *string) (string, error) {
	if verifier != nil && *verifier != "" {
		useSalt := ""
		if salt != nil {
			useSalt = *salt
		}
		if useSalt == "" {
			generated, err := randomSalt()
			if err != nil {
				return "", err
			}
			useSalt = generated
		}
		return auth.PasswordVerifierHash(*verifier, useSalt)
	}
	if password == nil || *password == "" {
		return "", httpx.BadRequest("a password is required")
	}
	return auth.HashPassword(*password, "")
}

// dummyLoginSalt is fixed so the unknown-account path performs the same work
// on every request without storing anything.
const dummyLoginSalt = "agents-anywhere-login-timing-dummy"

// dummyCredentialCheck spends the same effort as a real failed verification.
func dummyCredentialCheck(body authRequest) {
	if body.Password != nil && *body.Password != "" {
		_, _ = auth.HashPassword(*body.Password, dummyLoginSalt)
		return
	}
	if body.PasswordVerifier != nil && *body.PasswordVerifier != "" {
		// The verifier path is a constant-time compare; keep the compared
		// strings the same length as a stored digest.
		stored := "pbkdf2_sha256$" + dummyLoginSalt + "$" +
			strings.Repeat("0", len(*body.PasswordVerifier))
		_ = auth.VerifyPasswordVerifier(*body.PasswordVerifier, stored)
	}
}

// verifyCredential checks whichever form the client sent.
func (a *App) verifyCredential(user store.User, body authRequest) bool {
	if body.PasswordVerifier != nil && *body.PasswordVerifier != "" {
		return auth.VerifyPasswordVerifier(*body.PasswordVerifier, user.PasswordHash)
	}
	if body.Password != nil && *body.Password != "" {
		return auth.VerifyPassword(*body.Password, user.PasswordHash)
	}
	return false
}

// checkSetupToken validates the bootstrap token, minting one on first use. An
// error means the token could not be prepared (for example the entropy source
// failed) and belongs to the caller as a 500, not as a rejected token.
func (a *App) checkSetupToken(ctx context.Context, provided *string) (bool, error) {
	if fromEnv := strings.TrimSpace(envSetupToken()); fromEnv != "" {
		return provided != nil && subtleEqual(*provided, fromEnv), nil
	}
	expected, err := a.ensureSetupToken(ctx)
	if err != nil {
		a.log.Error("could not prepare the setup token", "error", err)
		return false, err
	}
	if exp, ok := a.setupTokenExpiry(ctx); ok && exp < time.Now().UnixMilli() {
		return false, nil
	}
	return provided != nil && subtleEqual(*provided, expected), nil
}

// ensureSetupToken returns the stored bootstrap token, generating a fresh one
// when the instance has none or the stored one has expired. The plaintext is
// written to the log once so the first administrator can complete setup.
func (a *App) ensureSetupToken(ctx context.Context) (string, error) {
	stored, err := a.setting(ctx, "setup_token")
	if err == nil && stored != "" {
		if exp, ok := a.setupTokenExpiry(ctx); !ok || exp >= time.Now().UnixMilli() {
			return stored, nil
		}
		// An expired token that is never replaced locks everyone out of the
		// bootstrap screen, so replace it on demand just like a missing one.
	}
	raw, err := randomBytes(18)
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	if err := a.setSetting(ctx, "setup_token", token); err != nil {
		return "", err
	}
	if err := a.setSetting(ctx, "setup_token_expires_at", itoa(expires)); err != nil {
		return "", err
	}
	a.log.Warn("bootstrap token generated",
		"setupToken", token,
		"expiresAt", isoTime(expires),
		"note", "use it to create the first account")
	return token, nil
}

func randomSalt() (string, error) {
	raw, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// randomBytes reports an entropy failure to the caller instead of deriving a
// predictable value from the clock.
func randomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// --- failed-attempt throttling ----------------------------------------------

const (
	// A short window bounds both the guesser's budget and the time a locked-out
	// owner has to wait once the counter trips.
	loginAttemptWindow = 5 * time.Minute
	// A person mistyping a password a handful of times recovers in seconds; a
	// guesser gets ten tries per address per window.
	loginAccountLimit = 10
	// One source may legitimately sign in many accounts (a shared office), so
	// its budget is larger.
	loginIPLimit = 60
	// The table is swept past this size and stops recording new keys rather
	// than growing without bound; an attacker who saturates it has already
	// paid for thousands of keyed requests.
	maxThrottleEntries = 8192
)

var loginAttempts = newAttemptLimiter()

type attemptEntry struct {
	count   int
	expires time.Time
}

// attemptLimiter counts recent failures per key over a fixed window.
type attemptLimiter struct {
	mu      sync.Mutex
	entries map[string]attemptEntry
	now     func() time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{entries: make(map[string]attemptEntry), now: time.Now}
}

// blocked reports how long the key must wait, or zero when it may proceed.
func (l *attemptLimiter) blocked(key string, limit int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok {
		return 0
	}
	now := l.now()
	if !now.Before(entry.expires) {
		delete(l.entries, key)
		return 0
	}
	if entry.count < limit {
		return 0
	}
	return entry.expires.Sub(now)
}

// record counts one failure.
func (l *attemptLimiter) record(key string, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.entries[key]
	if !ok || !now.Before(entry.expires) {
		if len(l.entries) >= maxThrottleEntries {
			l.sweepLocked(now)
			if len(l.entries) >= maxThrottleEntries {
				return
			}
		}
		entry = attemptEntry{expires: now.Add(window)}
	}
	entry.count++
	l.entries[key] = entry
}

// reset forgets a key after a successful sign-in.
func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *attemptLimiter) sweepLocked(now time.Time) {
	for key, entry := range l.entries {
		if !now.Before(entry.expires) {
			delete(l.entries, key)
		}
	}
}

// clientIP is the peer address without the port. It deliberately ignores
// X-Forwarded-For: the header is client-controlled unless a trusted proxy is
// configured, and a spoofable key would let a guesser reset its own budget.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func respondTooManyAttempts(w http.ResponseWriter, retry time.Duration) {
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	httpx.Fail(w, httpx.NewError(http.StatusTooManyRequests,
		"too many attempts; try again later"))
}
