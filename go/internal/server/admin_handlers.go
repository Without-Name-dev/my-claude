package server

// Administration endpoints: instance settings, user management, announcements,
// the service descriptor, and the usage report. They are mounted by
// registerAdminRoutes and every one of them requires an administrator.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const (
	adminSettingsKey     = "admin_settings"
	announcementKey      = "public_announcement"
	announcementMaxBytes = 20000
	maxReportDays        = 366
	defaultReportDays    = 30
	liveActiveWindow     = 15 * time.Minute

	// The server exposes exactly one runtime to every connector (see
	// handleListRuntimes). The report needs the same identity even when no
	// connector catalog rows exist, because connector_runtime_types is not
	// written by anything.
	primaryRuntimeType        = "claude"
	primaryRuntimeDisplayName = "Claude Code"
)

// adminProcessStartedAt is captured when the package loads. It is the closest
// honest answer this process can give to "when did the service start"; no
// supervisor tells us the real timestamp.
var (
	adminProcessStartedAt = time.Now()
	adminInstanceID       = store.NewID("inst")
	adminVersion          = buildAdminVersion()
)

// registerAdminRoutes installs the /admin endpoints.
func (a *App) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v2/admin/usage/report", a.requireUser(a.adminOnly(a.handleAdminUsageReport)))
	mux.HandleFunc("GET /api/v2/admin/usage/live", a.requireUser(a.adminOnly(a.handleAdminUsageLive)))
	mux.HandleFunc("GET /api/v2/admin/users", a.requireUser(a.adminOnly(a.handleAdminListUsers)))
	mux.HandleFunc("POST /api/v2/admin/users", a.requireUser(a.adminOnly(a.handleAdminCreateUser)))
	mux.HandleFunc("GET /api/v2/admin/announcement", a.requireUser(a.adminOnly(a.handleAdminGetAnnouncement)))
	mux.HandleFunc("PUT /api/v2/admin/announcement", a.requireUser(a.adminOnly(a.handleAdminUpdateAnnouncement)))
	mux.HandleFunc("GET /api/v2/admin/settings", a.requireUser(a.adminOnly(a.handleAdminGetSettings)))
	// The specification names PATCH; the audit that produced this task names
	// PUT. Accept both so either client works.
	mux.HandleFunc("PUT /api/v2/admin/settings", a.requireUser(a.adminOnly(a.handleAdminUpdateSettings)))
	mux.HandleFunc("PATCH /api/v2/admin/settings", a.requireUser(a.adminOnly(a.handleAdminUpdateSettings)))
	mux.HandleFunc("GET /api/v2/admin/service", a.requireUser(a.adminOnly(a.handleAdminServiceInfo)))
}

// adminOnly rejects members. requireUser has already resolved the account, so
// this check is a single field comparison.
func (a *App) adminOnly(next func(http.ResponseWriter, *http.Request, store.User)) func(http.ResponseWriter, *http.Request, store.User) {
	return func(w http.ResponseWriter, r *http.Request, user store.User) {
		if user.Role != "admin" {
			httpx.Fail(w, httpx.Forbidden("admin required"))
			return
		}
		next(w, r, user)
	}
}

// --- users -------------------------------------------------------------------

// adminUserView mirrors UserView in the REST specification.
type adminUserView struct {
	UserID        string  `json:"userId"`
	Email         *string `json:"email"`
	EmailVerified bool    `json:"emailVerified"`
	DisplayName   string  `json:"displayName"`
	Role          string  `json:"role"`
	Disabled      bool    `json:"disabled"`
	Avatar        *string `json:"avatar"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

func adminUserViewFrom(user store.User) adminUserView {
	var email, avatar *string
	if user.Email != "" {
		value := user.Email
		email = &value
	}
	if user.Avatar != "" {
		value := user.Avatar
		avatar = &value
	}
	return adminUserView{
		UserID:        user.ID,
		Email:         email,
		EmailVerified: user.EmailVerifiedAt != nil,
		DisplayName:   user.DisplayName,
		Role:          user.Role,
		Disabled:      user.Disabled,
		Avatar:        avatar,
		CreatedAt:     isoTime(user.CreatedAt),
		UpdatedAt:     isoTime(user.UpdatedAt),
	}
}

func (a *App) handleAdminListUsers(w http.ResponseWriter, r *http.Request, _ store.User) {
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "limit must be between 1 and 1000"))
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "offset must be zero or greater"))
			return
		}
		offset = parsed
	}
	role := ""
	switch strings.ToLower(strings.TrimSpace(query.Get("role"))) {
	case "", "all":
	case "admin", "admins":
		role = "admin"
	case "member", "members":
		role = "member"
	default:
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "role must be all, admins, or members"))
		return
	}
	search := strings.TrimSpace(query.Get("search"))
	if len(search) > maxEmailLength {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "search must be at most 320 characters"))
		return
	}

	users, err := a.db.ListUsersFiltered(r.Context(), limit, offset, role, search)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	total, err := a.db.CountUsersFiltered(r.Context(), role, search)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views := make([]adminUserView, 0, len(users))
	for _, user := range users {
		views = append(views, adminUserViewFrom(user))
	}
	httpx.OK(w, map[string]any{
		"users":      views,
		"total":      total,
		"serverTime": timestampNow(),
	})
}

func (a *App) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body struct {
		Email                 string  `json:"email"`
		DisplayName           string  `json:"displayName"`
		Code                  *string `json:"code"`
		Password              *string `json:"password"`
		PasswordVerifier      *string `json:"passwordVerifier"`
		PasswordSalt          *string `json:"passwordSalt"`
		Role                  *string `json:"role"`
		SkipEmailVerification bool    `json:"skipEmailVerification"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	email, err := validateEmailAddress(body.Email)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	displayName, err := validateDisplayName(body.DisplayName)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	role := "member"
	if body.Role != nil && *body.Role != "" {
		role = *body.Role
	}
	if role, err = validateRole(role); err != nil {
		httpx.Fail(w, err)
		return
	}

	hash, err := credentialHashFromParts(body.Password, body.PasswordVerifier, body.PasswordSalt)
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, err.Error()))
		return
	}

	created := store.User{
		Email:        email,
		DisplayName:  displayName,
		Role:         role,
		PasswordHash: hash,
	}
	if body.SkipEmailVerification {
		now := store.NowMS()
		created.EmailVerifiedAt = &now
	}
	user, err := a.db.CreateUser(r.Context(), created)
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Fail(w, httpx.Conflict("user already exists"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, adminUserViewFrom(user))
}

// handleAdminUpdateUser edits displayName, role and disabled.
//
// Password changes are intentionally not part of this endpoint: the audit only
// needs the three account fields, and a password path needs the verifier/salt
// pair the create endpoint already owns.
func (a *App) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request, actor store.User) {
	var body struct {
		DisplayName *string `json:"displayName"`
		Role        *string `json:"role"`
		Disabled    *bool   `json:"disabled"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.DisplayName == nil && body.Role == nil && body.Disabled == nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "nothing to update"))
		return
	}
	if body.DisplayName != nil {
		name, err := validateDisplayName(*body.DisplayName)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		body.DisplayName = &name
	}
	if body.Role != nil {
		role, err := validateRole(*body.Role)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		body.Role = &role
	}

	updated, err := a.db.UpdateUserAsAdmin(
		r.Context(), actor.ID, r.PathValue("user_id"),
		body.DisplayName, body.Role, body.Disabled,
	)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpx.Fail(w, httpx.NotFound("user not found"))
		case errors.Is(err, store.ErrLastAdmin):
			httpx.Fail(w, httpx.Conflict(store.ErrLastAdmin.Error()))
		case errors.Is(err, store.ErrSelfAdminEdit):
			httpx.Fail(w, httpx.Conflict(store.ErrSelfAdminEdit.Error()))
		default:
			httpx.Fail(w, err)
		}
		return
	}
	httpx.OK(w, adminUserViewFrom(updated))
}

// handleAdminDeleteUser removes an account and its devices and conversations.
//
// Refusing the last administrator is the difference between a clean delete and
// an instance nobody can administer afterwards.
func (a *App) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request, actor store.User) {
	files, err := a.db.DeleteUserAsAdmin(r.Context(), actor.ID, r.PathValue("user_id"))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpx.Fail(w, httpx.NotFound("user not found"))
		case errors.Is(err, store.ErrLastAdmin):
			httpx.Fail(w, httpx.Conflict(store.ErrLastAdmin.Error()))
		case errors.Is(err, store.ErrSelfDeleteUser):
			httpx.Fail(w, httpx.Conflict(store.ErrSelfDeleteUser.Error()))
		default:
			httpx.Fail(w, err)
		}
		return
	}
	// The rows are committed; the blobs are not part of the database and must
	// be removed separately.
	for _, file := range files {
		if err := store.DeleteAttachment(a.cfg.UploadDir, file.SessionID, file.FileID); err != nil {
			a.log.Warn("could not delete an attachment",
				"session", file.SessionID, "file", file.FileID, "error", err)
		}
	}
	httpx.NoContent(w)
}

// --- announcements -----------------------------------------------------------

// announcementSettings mirrors AnnouncementSettings in the specification.
type announcementSettings struct {
	Enabled     bool    `json:"enabled"`
	Markdown    string  `json:"markdown"`
	PublishedAt *string `json:"publishedAt"`
}

func (a *App) loadAnnouncementSettings(ctx context.Context) announcementSettings {
	var stored announcementSettings
	raw, err := a.setting(ctx, announcementKey)
	if err != nil || raw == "" {
		return stored
	}
	if err := jsonUnmarshal(raw, &stored); err != nil {
		return announcementSettings{}
	}
	return stored
}

func (a *App) handleAdminGetAnnouncement(w http.ResponseWriter, r *http.Request, _ store.User) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, a.loadAnnouncementSettings(r.Context()))
}

func (a *App) handleAdminUpdateAnnouncement(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body struct {
		Enabled  *bool   `json:"enabled"`
		Markdown *string `json:"markdown"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Enabled == nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "enabled is required"))
		return
	}
	if body.Markdown == nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "markdown is required"))
		return
	}
	markdown := *body.Markdown
	if len(markdown) > announcementMaxBytes {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "markdown must be at most 20000 characters"))
		return
	}
	if *body.Enabled && strings.TrimSpace(markdown) == "" {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "an enabled announcement must contain Markdown text"))
		return
	}

	current := a.loadAnnouncementSettings(r.Context())
	if current.Enabled == *body.Enabled && current.Markdown == markdown {
		w.Header().Set("Cache-Control", "no-store")
		httpx.OK(w, current)
		return
	}
	publishedAt := current.PublishedAt
	if *body.Enabled {
		next := nextPublicationTime(current.PublishedAt)
		publishedAt = &next
	}
	updated := announcementSettings{Enabled: *body.Enabled, Markdown: markdown, PublishedAt: publishedAt}
	raw, err := json.Marshal(updated)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := a.setSetting(r.Context(), announcementKey, string(raw)); err != nil {
		httpx.Fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, updated)
}

// nextPublicationTime keeps publishedAt strictly increasing even when two
// updates land inside the same millisecond.
func nextPublicationTime(previous *string) string {
	now := time.Now().UTC()
	if previous != nil && *previous != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, *previous); err == nil && !parsed.Before(now) {
			now = parsed.Add(time.Millisecond)
		}
	}
	return isoTime(now.UnixMilli())
}

// --- instance settings -------------------------------------------------------

// oAuthPublicConfig mirrors OAuthProviderPublicConfig. The secret is never
// returned, and this server does not implement OAuth, so the values only
// reflect what an administrator configured.
type oAuthPublicConfig struct {
	Enabled       bool   `json:"enabled"`
	Provider      string `json:"provider"`
	Label         string `json:"label"`
	AuthorizeURL  string `json:"authorizeUrl"`
	TokenURL      string `json:"tokenUrl"`
	UserInfoURL   string `json:"userInfoUrl"`
	ClientID      string `json:"clientId"`
	Scopes        string `json:"scopes"`
	UsernameClaim string `json:"usernameClaim"`
	SubjectClaim  string `json:"subjectClaim"`
	EmailClaim    string `json:"emailClaim"`
	NameClaim     string `json:"nameClaim"`
}

// emailSettingsView mirrors EmailSettingsView. The API key itself is never
// returned, only whether one was supplied.
type emailSettingsView struct {
	Enabled          bool   `json:"enabled"`
	FromAddress      string `json:"fromAddress"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

// instanceSettingsStored is the JSON document persisted in instance_settings.
type instanceSettingsStored struct {
	RegistrationOpen      bool               `json:"registrationOpen"`
	OAuthRegistrationOpen bool               `json:"oauthRegistrationOpen"`
	PasswordResetEnabled  bool               `json:"passwordResetEnabled"`
	OAuth                 *oAuthPublicConfig `json:"oauth"`
	Email                 emailSettingsView  `json:"email"`
}

// instanceSettingsResponse is InstanceSettingsView plus the instance facts the
// admin page displays next to the configuration. The extra fields are reported
// in the task notes because the specification has no place for them.
type instanceSettingsResponse struct {
	RegistrationOpen      bool               `json:"registrationOpen"`
	OAuthRegistrationOpen bool               `json:"oauthRegistrationOpen"`
	PasswordResetEnabled  bool               `json:"passwordResetEnabled"`
	OAuth                 *oAuthPublicConfig `json:"oauth"`
	Email                 emailSettingsView  `json:"email"`
	Version               string             `json:"version"`
	StartedAt             string             `json:"startedAt"`
	UptimeSeconds         int                `json:"uptimeSeconds"`
	Database              string             `json:"database"`
	DatabasePath          *string            `json:"databasePath"`
	UserCount             int                `json:"userCount"`
	DatabaseSizeBytes     int64              `json:"databaseSizeBytes"`
}

type oAuthSettingsUpdate struct {
	Enabled       *bool   `json:"enabled"`
	Provider      *string `json:"provider"`
	Label         *string `json:"label"`
	AuthorizeURL  *string `json:"authorizeUrl"`
	TokenURL      *string `json:"tokenUrl"`
	UserInfoURL   *string `json:"userInfoUrl"`
	ClientID      *string `json:"clientId"`
	Scopes        *string `json:"scopes"`
	UsernameClaim *string `json:"usernameClaim"`
	SubjectClaim  *string `json:"subjectClaim"`
	EmailClaim    *string `json:"emailClaim"`
	NameClaim     *string `json:"nameClaim"`
}

func (u *oAuthSettingsUpdate) apply(base *oAuthPublicConfig) *oAuthPublicConfig {
	if base == nil {
		base = &oAuthPublicConfig{
			Provider:      "oidc",
			Label:         "OAuth",
			Scopes:        "openid profile email",
			UsernameClaim: "preferred_username",
			SubjectClaim:  "sub",
			EmailClaim:    "email",
			NameClaim:     "name",
		}
	}
	if u.Enabled != nil {
		base.Enabled = *u.Enabled
	}
	if u.Provider != nil {
		base.Provider = *u.Provider
	}
	if u.Label != nil {
		base.Label = *u.Label
	}
	if u.AuthorizeURL != nil {
		base.AuthorizeURL = *u.AuthorizeURL
	}
	if u.TokenURL != nil {
		base.TokenURL = *u.TokenURL
	}
	if u.UserInfoURL != nil {
		base.UserInfoURL = *u.UserInfoURL
	}
	if u.ClientID != nil {
		base.ClientID = *u.ClientID
	}
	if u.Scopes != nil {
		base.Scopes = *u.Scopes
	}
	if u.UsernameClaim != nil {
		base.UsernameClaim = *u.UsernameClaim
	}
	if u.SubjectClaim != nil {
		base.SubjectClaim = *u.SubjectClaim
	}
	if u.EmailClaim != nil {
		base.EmailClaim = *u.EmailClaim
	}
	if u.NameClaim != nil {
		base.NameClaim = *u.NameClaim
	}
	return base
}

type emailSettingsUpdate struct {
	Enabled     *bool   `json:"enabled"`
	FromAddress *string `json:"fromAddress"`
	APIKey      *string `json:"apiKey"`
	ClearAPIKey *bool   `json:"clearApiKey"`
}

func (u *emailSettingsUpdate) apply(base *emailSettingsView) {
	if u.Enabled != nil {
		base.Enabled = *u.Enabled
	}
	if u.FromAddress != nil {
		base.FromAddress = *u.FromAddress
	}
	if u.ClearAPIKey != nil && *u.ClearAPIKey {
		base.APIKeyConfigured = false
	}
	if u.APIKey != nil && *u.APIKey != "" {
		base.APIKeyConfigured = true
	}
}

type instanceSettingsUpdate struct {
	RegistrationOpen      *bool                `json:"registrationOpen"`
	OAuthRegistrationOpen *bool                `json:"oauthRegistrationOpen"`
	PasswordResetEnabled  *bool                `json:"passwordResetEnabled"`
	OAuth                 *oAuthSettingsUpdate `json:"oauth"`
	Email                 *emailSettingsUpdate `json:"email"`
}

// instanceInfo is the subset of process facts shared by /admin/settings and
// /admin/service.
type instanceInfo struct {
	Version           string
	StartedAt         string
	UptimeSeconds     int
	Database          string
	DatabasePath      *string
	UserCount         int
	DatabaseSizeBytes int64
}

func (a *App) instanceInfo(ctx context.Context) (instanceInfo, error) {
	userCount, err := a.db.CountUsers(ctx)
	if err != nil {
		return instanceInfo{}, err
	}
	path := a.db.Path()
	absolute := path
	if value, err := filepath.Abs(path); err == nil {
		absolute = value
	}
	var databasePath *string
	if absolute != "" {
		value := absolute
		databasePath = &value
	}
	uptime := int(time.Since(adminProcessStartedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	return instanceInfo{
		Version:           adminVersion,
		StartedAt:         isoTime(adminProcessStartedAt.UnixMilli()),
		UptimeSeconds:     uptime,
		Database:          "SQLite",
		DatabasePath:      databasePath,
		UserCount:         userCount,
		DatabaseSizeBytes: databaseSize(path),
	}, nil
}

func (a *App) loadInstanceSettings(ctx context.Context) instanceSettingsStored {
	stored := instanceSettingsStored{RegistrationOpen: true}
	raw, err := a.setting(ctx, adminSettingsKey)
	if err != nil || raw == "" {
		return stored
	}
	if err := jsonUnmarshal(raw, &stored); err != nil {
		return instanceSettingsStored{RegistrationOpen: true}
	}
	return stored
}

func (a *App) adminSettingsView(ctx context.Context) (instanceSettingsResponse, error) {
	stored := a.loadInstanceSettings(ctx)
	info, err := a.instanceInfo(ctx)
	if err != nil {
		return instanceSettingsResponse{}, err
	}
	return instanceSettingsResponse{
		RegistrationOpen:      stored.RegistrationOpen,
		OAuthRegistrationOpen: stored.OAuthRegistrationOpen,
		PasswordResetEnabled:  stored.PasswordResetEnabled,
		OAuth:                 stored.OAuth,
		Email:                 stored.Email,
		Version:               info.Version,
		StartedAt:             info.StartedAt,
		UptimeSeconds:         info.UptimeSeconds,
		Database:              info.Database,
		DatabasePath:          info.DatabasePath,
		UserCount:             info.UserCount,
		DatabaseSizeBytes:     info.DatabaseSizeBytes,
	}, nil
}

func (a *App) handleAdminGetSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	view, err := a.adminSettingsView(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, view)
}

func (a *App) handleAdminUpdateSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body instanceSettingsUpdate
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	stored := a.loadInstanceSettings(r.Context())
	if body.RegistrationOpen != nil {
		stored.RegistrationOpen = *body.RegistrationOpen
	}
	if body.OAuthRegistrationOpen != nil {
		stored.OAuthRegistrationOpen = *body.OAuthRegistrationOpen
	}
	if body.PasswordResetEnabled != nil {
		stored.PasswordResetEnabled = *body.PasswordResetEnabled
	}
	if body.OAuth != nil {
		stored.OAuth = body.OAuth.apply(stored.OAuth)
	}
	if body.Email != nil {
		body.Email.apply(&stored.Email)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := a.setSetting(r.Context(), adminSettingsKey, string(raw)); err != nil {
		httpx.Fail(w, err)
		return
	}
	view, err := a.adminSettingsView(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, view)
}

// --- service -----------------------------------------------------------------

// serviceInfoResponse mirrors ServiceInfoResponse. userCount and
// databaseSizeBytes are additions: the specification has no inventory fields
// here, and the admin page displays them.
type serviceInfoResponse struct {
	Endpoint              string  `json:"endpoint"`
	Version               string  `json:"version"`
	Database              string  `json:"database"`
	DatabasePath          *string `json:"databasePath"`
	DatabaseSchemaVersion string  `json:"databaseSchemaVersion"`
	Coordination          string  `json:"coordination"`
	InstanceID            string  `json:"instanceId"`
	StartedAt             string  `json:"startedAt"`
	UptimeSeconds         int     `json:"uptimeSeconds"`
	ServerTime            string  `json:"serverTime"`
	UserCount             int     `json:"userCount"`
	DatabaseSizeBytes     int64   `json:"databaseSizeBytes"`
}

func (a *App) handleAdminServiceInfo(w http.ResponseWriter, r *http.Request, _ store.User) {
	info, err := a.instanceInfo(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	schemaVersion, err := a.db.SchemaVersion(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	endpoint := requestEndpoint(r)
	if endpoint == "" {
		endpoint = a.cfg.PublicURL
	}
	httpx.OK(w, serviceInfoResponse{
		Endpoint:              endpoint,
		Version:               info.Version,
		Database:              info.Database,
		DatabasePath:          info.DatabasePath,
		DatabaseSchemaVersion: schemaVersion,
		Coordination:          "memory",
		InstanceID:            adminInstanceID,
		StartedAt:             info.StartedAt,
		UptimeSeconds:         info.UptimeSeconds,
		ServerTime:            timestampNow(),
		UserCount:             info.UserCount,
		DatabaseSizeBytes:     info.DatabaseSizeBytes,
	})
}

// requestEndpoint reconstructs the public origin, honouring the reverse-proxy
// headers the deployment guide asks nginx to set.
func requestEndpoint(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func firstHeaderValue(value string) string {
	if index := strings.IndexByte(value, ','); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

// buildAdminVersion reports what the binary itself knows. Development builds
// carry no module version, so the VCS revision is the best real answer.
func buildAdminVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if version := info.Main.Version; version != "" && version != "(devel)" {
		return version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
			return "devel-" + setting.Value[:12]
		}
	}
	return "devel"
}

func databaseSize(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// --- live usage --------------------------------------------------------------

// usageLiveResponse mirrors UsageLiveResponse. totalUsers, totalSessions and
// activeSessions15m are additions requested by the audit: the specification
// only exposes activeUsers15m.
type usageLiveResponse struct {
	OnlineConnectors  int    `json:"onlineConnectors"`
	TotalConnectors   int    `json:"totalConnectors"`
	RunningTurns      int    `json:"runningTurns"`
	ActiveUsers15m    int    `json:"activeUsers15m"`
	TotalUsers        int    `json:"totalUsers"`
	TotalSessions     int    `json:"totalSessions"`
	ActiveSessions15m int    `json:"activeSessions15m"`
	ServerTime        string `json:"serverTime"`
}

func (a *App) handleAdminUsageLive(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()

	// OnlineIDs covers every account. The audit asked for the requesting
	// administrator's devices only, so intersect it with their connectors.
	connectors, err := a.db.ListConnectors(ctx, user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	owned := make(map[string]struct{}, len(connectors))
	for _, connector := range connectors {
		owned[connector.ID] = struct{}{}
	}
	online := 0
	for _, id := range a.connectors.OnlineIDs() {
		if _, ok := owned[id]; ok {
			online++
		}
	}

	totalConnectors, err := a.db.CountConnectors(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	runningTurns, err := a.db.CountRunningSessions(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	cutoff := store.NowMS() - liveActiveWindow.Milliseconds()
	activeUsers, err := a.db.CountActiveUsersSince(ctx, cutoff)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	activeSessions, err := a.db.CountActiveSessionsSince(ctx, cutoff)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	totalUsers, err := a.db.CountUsers(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	totalSessions, err := a.db.CountSessions(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	httpx.OK(w, usageLiveResponse{
		OnlineConnectors:  online,
		TotalConnectors:   totalConnectors,
		RunningTurns:      runningTurns,
		ActiveUsers15m:    activeUsers,
		TotalUsers:        totalUsers,
		TotalSessions:     totalSessions,
		ActiveSessions15m: activeSessions,
		ServerTime:        timestampNow(),
	})
}

// --- usage report ------------------------------------------------------------

type usageRange struct {
	FromDate    string  `json:"fromDate"`
	ToDate      string  `json:"toDate"`
	Timezone    string  `json:"timezone"`
	DataSince   *string `json:"dataSince"`
	GeneratedAt string  `json:"generatedAt"`
}

type usagePercentiles struct {
	P50     *int `json:"p50"`
	P90     *int `json:"p90"`
	P99     *int `json:"p99"`
	Samples int  `json:"samples"`
}

type usageSummary struct {
	ActiveUsers        int              `json:"activeUsers"`
	NewUsers           int              `json:"newUsers"`
	AvgDAU             float64          `json:"avgDau"`
	Wau                int              `json:"wau"`
	Mau                int              `json:"mau"`
	TotalUsers         int              `json:"totalUsers"`
	Messages           int              `json:"messages"`
	Sessions           int              `json:"sessions"`
	SessionViews       int              `json:"sessionViews"`
	SessionsViewed     int              `json:"sessionsViewed"`
	SessionsReadOnly   int              `json:"sessionsReadOnly"`
	Turns              int              `json:"turns"`
	TurnFailureRate    *float64         `json:"turnFailureRate"`
	TurnDurationMs     usagePercentiles `json:"turnDurationMs"`
	ApprovalsRequested int              `json:"approvalsRequested"`
	ApprovalsResolved  int              `json:"approvalsResolved"`
	ApprovalWaitMs     usagePercentiles `json:"approvalWaitMs"`
	RuntimeErrors      int              `json:"runtimeErrors"`
	ConnectorsOnline   int              `json:"connectorsOnline"`
	TotalConnectors    int              `json:"totalConnectors"`
}

type usageDailyPoint struct {
	Date               string `json:"date"`
	ActiveUsers        int    `json:"activeUsers"`
	NewUsers           int    `json:"newUsers"`
	Wau                int    `json:"wau"`
	Mau                int    `json:"mau"`
	Messages           int    `json:"messages"`
	Sessions           int    `json:"sessions"`
	SessionViews       int    `json:"sessionViews"`
	SessionsViewed     int    `json:"sessionsViewed"`
	SessionsReadOnly   int    `json:"sessionsReadOnly"`
	Turns              int    `json:"turns"`
	TurnsFailed        int    `json:"turnsFailed"`
	TurnDurationP50Ms  *int   `json:"turnDurationP50Ms"`
	TurnDurationP90Ms  *int   `json:"turnDurationP90Ms"`
	ApprovalsRequested int    `json:"approvalsRequested"`
	ApprovalsResolved  int    `json:"approvalsResolved"`
	RuntimeErrors      int    `json:"runtimeErrors"`
	ConnectorsOnline   int    `json:"connectorsOnline"`
	// Devices is the number of connectors created that day. The specification
	// only has connectorsOnline, but the audit asked for a per-day device
	// count sourced from connectors.created_at.
	Devices int `json:"devices"`
}

type usageHourPoint struct {
	Hour        int `json:"hour"`
	Messages    int `json:"messages"`
	ActiveUsers int `json:"activeUsers"`
}

type usageSegment struct {
	Segment string `json:"segment"`
	Min     int    `json:"min"`
	Max     *int   `json:"max"`
	Count   int    `json:"count"`
}

type usageBucket struct {
	Label string `json:"label"`
	Min   int    `json:"min"`
	Max   *int   `json:"max"`
	Count int    `json:"count"`
}

type usageRetentionCohort struct {
	CohortStart string   `json:"cohortStart"`
	CohortEnd   string   `json:"cohortEnd"`
	CohortSize  int      `json:"cohortSize"`
	D1          *float64 `json:"d1"`
	D7          *float64 `json:"d7"`
	D30         *float64 `json:"d30"`
}

type usageUsersSection struct {
	Segments         []usageSegment         `json:"segments"`
	MessageHistogram []usageBucket          `json:"messageHistogram"`
	Retention        []usageRetentionCohort `json:"retention"`
}

type usageRuntimeRow struct {
	RuntimeType          string `json:"runtimeType"`
	DisplayName          string `json:"displayName"`
	Messages             int    `json:"messages"`
	Sessions             int    `json:"sessions"`
	ActiveUsers          int    `json:"activeUsers"`
	Turns                int    `json:"turns"`
	TurnsFailed          int    `json:"turnsFailed"`
	TurnDurationP50Ms    *int   `json:"turnDurationP50Ms"`
	RuntimeErrors        int    `json:"runtimeErrors"`
	ConfiguredConnectors int    `json:"configuredConnectors"`
	DiscoveredConnectors int    `json:"discoveredConnectors"`
}

type usageOSRow struct {
	OS               string `json:"os"`
	Connectors       int    `json:"connectors"`
	ConnectorsOnline int    `json:"connectorsOnline"`
}

type usageInventoryPoint struct {
	Date       string `json:"date"`
	Users      int    `json:"users"`
	Connectors int    `json:"connectors"`
}

type usageDevicesSection struct {
	TotalConnectors   int                   `json:"totalConnectors"`
	ConnectorsSeen7d  int                   `json:"connectorsSeen7d"`
	ConnectorsSeen30d int                   `json:"connectorsSeen30d"`
	ByOS              []usageOSRow          `json:"byOs"`
	InventoryDaily    []usageInventoryPoint `json:"inventoryDaily"`
	InventoryAsOf     string                `json:"inventoryAsOf"`
}

type usageOutcomeRow struct {
	Outcome string `json:"outcome"`
	Count   int    `json:"count"`
}

type usageQualitySection struct {
	TurnOutcomes     []usageOutcomeRow `json:"turnOutcomes"`
	TurnDurationMs   usagePercentiles  `json:"turnDurationMs"`
	ApprovalOutcomes []usageOutcomeRow `json:"approvalOutcomes"`
	ApprovalWaitMs   usagePercentiles  `json:"approvalWaitMs"`
	RuntimeErrors    int               `json:"runtimeErrors"`
}

type usageReportResponse struct {
	Range      usageRange          `json:"range"`
	Summary    usageSummary        `json:"summary"`
	Daily      []usageDailyPoint   `json:"daily"`
	HourOfDay  []usageHourPoint    `json:"hourOfDay"`
	Users      usageUsersSection   `json:"users"`
	Runtimes   []usageRuntimeRow   `json:"runtimes"`
	Devices    usageDevicesSection `json:"devices"`
	Quality    usageQualitySection `json:"quality"`
	ServerTime string              `json:"serverTime"`
}

type usageWindow struct {
	from time.Time
	to   time.Time
	loc  *time.Location
	tz   string
}

func parseUsageWindow(r *http.Request) (usageWindow, error) {
	query := r.URL.Query()
	tz := query.Get("tz")
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return usageWindow{}, httpx.NewError(http.StatusUnprocessableEntity, "unsupported timezone: "+tz)
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	to := today
	if raw := query.Get("to"); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return usageWindow{}, httpx.NewError(http.StatusUnprocessableEntity, "invalid to date: "+raw)
		}
		to = parsed
	}
	from := to.AddDate(0, 0, -(defaultReportDays - 1))
	if raw := query.Get("from"); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			return usageWindow{}, httpx.NewError(http.StatusUnprocessableEntity, "invalid from date: "+raw)
		}
		from = parsed
	}
	if from.After(to) {
		return usageWindow{}, httpx.NewError(http.StatusUnprocessableEntity, "from date must be before or equal to to date")
	}
	if civilOrdinal(to)-civilOrdinal(from)+1 > maxReportDays {
		return usageWindow{}, httpx.NewError(http.StatusUnprocessableEntity, "range must not exceed 366 days")
	}
	return usageWindow{from: from, to: to, loc: loc, tz: tz}, nil
}

func civilOrdinal(t time.Time) int {
	year, month, day := t.Date()
	return int(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

func localDateOrdinal(ms int64, loc *time.Location) int {
	return civilOrdinal(time.UnixMilli(ms).In(loc))
}

func localDayKey(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02")
}

func localHourOf(ms int64, loc *time.Location) int {
	return time.UnixMilli(ms).In(loc).Hour()
}

func reportDays(from, to time.Time) []time.Time {
	days := make([]time.Time, 0, civilOrdinal(to)-civilOrdinal(from)+1)
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		days = append(days, day)
	}
	return days
}

// reportDayState carries the sets that become daily totals at the end.
type reportDayState struct {
	point usageDailyPoint
	users map[string]struct{}
	// sessions holds every session that showed activity that day, whether the
	// evidence was its activity clock or a message item.
	sessions   map[string]struct{}
	connectors map[string]struct{}
}

type reportRuntimeState struct {
	messages    int
	turns       int
	turnsFailed int
	sessions    map[string]struct{}
	users       map[string]struct{}
}

func (a *App) handleAdminUsageReport(w http.ResponseWriter, r *http.Request, _ store.User) {
	window, err := parseUsageWindow(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ctx := r.Context()
	fromMS := window.from.UTC().UnixMilli()
	toMS := window.to.AddDate(0, 0, 1).UTC().UnixMilli()

	// Rolling windows (wau/mau) need 29 days before the range; retention
	// needs up to 30 days after a signup, capped at today when the range ends
	// in the future.
	activityFromMS := window.from.AddDate(0, 0, -29).UTC().UnixMilli()
	wideEnd := window.to.AddDate(0, 0, 30)
	now := time.Now().In(window.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, window.loc)
	latest := today
	if window.to.After(latest) {
		latest = window.to
	}
	if wideEnd.After(latest) {
		wideEnd = latest
	}
	activityToMS := wideEnd.AddDate(0, 0, 1).UTC().UnixMilli()

	connectorCreations, err := a.db.ConnectorCreatedBetween(ctx, fromMS, toMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	userCreations, err := a.db.UsersCreatedBetween(ctx, fromMS, toMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	messages, err := a.db.MessageActivity(ctx, fromMS, toMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	activity, err := a.db.SessionActivity(ctx, activityFromMS, activityToMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	connectorActivity, err := a.db.ConnectorActivity(ctx, fromMS, toMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	totalUsers, err := a.db.CountUsers(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	totalConnectors, err := a.db.CountConnectors(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	usersBefore, err := a.db.CountUsersBefore(ctx, fromMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	connectorsBefore, err := a.db.CountConnectorsBefore(ctx, fromMS)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	seen7d, err := a.db.CountConnectorsSeenSince(ctx, store.NowMS()-7*24*60*60*1000)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	seen30d, err := a.db.CountConnectorsSeenSince(ctx, store.NowMS()-30*24*60*60*1000)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	osCounts, err := a.db.ConnectorCountsByOS(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	earliest, err := a.db.EarliestSessionTime(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	days := reportDays(window.from, window.to)
	dayState := make(map[string]*reportDayState, len(days))
	dayOrder := make([]string, 0, len(days))
	hourMessages := make([]int, 24)
	hourUsers := make([]map[string]struct{}, 24)
	for hour := range hourUsers {
		hourUsers[hour] = map[string]struct{}{}
	}
	userDays := map[string]map[int]struct{}{}
	runtimes := map[string]*reportRuntimeState{}
	runtimeFor := func(name string) *reportRuntimeState {
		if name == "" {
			name = "unknown"
		}
		state := runtimes[name]
		if state == nil {
			state = &reportRuntimeState{
				sessions: map[string]struct{}{},
				users:    map[string]struct{}{},
			}
			runtimes[name] = state
		}
		return state
	}
	userMessages := map[string]int{}
	// summarySessions is distinct across the whole range: a session active on
	// three days of a 30-day report still counts once in the summary.
	summarySessions := map[string]struct{}{}
	outcomeCounts := map[string]int{}
	turnStatuses := map[string]string{
		"done":        "completed",
		"failed":      "failed",
		"interrupted": "interrupted",
		"cancelled":   "cancelled",
	}

	for _, day := range days {
		key := day.Format("2006-01-02")
		dayOrder = append(dayOrder, key)
		dayState[key] = &reportDayState{
			point:      usageDailyPoint{Date: key},
			users:      map[string]struct{}{},
			sessions:   map[string]struct{}{},
			connectors: map[string]struct{}{},
		}
	}

	// Devices by the day they were created.
	for _, row := range connectorCreations {
		key := localDayKey(row.Hour, window.loc)
		if state := dayState[key]; state != nil {
			state.point.Devices++
		}
	}

	// Signups by the day they were created.
	for _, row := range userCreations {
		key := localDayKey(row.CreatedAt, window.loc)
		if state := dayState[key]; state != nil {
			state.point.NewUsers++
		}
	}

	// Message timeline items become both message and turn counts. They also
	// prove the session was active that day: last_activity_at only remembers
	// the most recent day, so relying on it alone would report zero sessions
	// for older days that clearly produced messages.
	for _, row := range messages {
		key := localDayKey(row.Hour, window.loc)
		state := dayState[key]
		if state == nil {
			continue
		}
		state.sessions[row.SessionID] = struct{}{}
		summarySessions[row.SessionID] = struct{}{}
		runtime := runtimeFor(row.Runtime)
		runtime.sessions[row.SessionID] = struct{}{}
		switch row.Role {
		case "user":
			state.point.Messages += row.Count
			hourMessages[localHourOf(row.Hour, window.loc)] += row.Count
			userMessages[row.UserID] += row.Count
			runtime.messages += row.Count
		case "assistant":
			state.point.Turns += row.Count
			runtime.turns += row.Count
			outcome := turnStatuses[row.Status]
			if outcome == "" {
				outcome = row.Status
			}
			if outcome == "" {
				outcome = "unknown"
			}
			outcomeCounts[outcome] += row.Count
			if row.Status == "failed" {
				state.point.TurnsFailed += row.Count
				runtime.turnsFailed += row.Count
			}
		}
	}

	// Session activity drives daily active users and the rolling windows.
	for _, row := range activity {
		ordinal := localDateOrdinal(row.Hour, window.loc)
		userSet := userDays[row.UserID]
		if userSet == nil {
			userSet = map[int]struct{}{}
			userDays[row.UserID] = userSet
		}
		userSet[ordinal] = struct{}{}

		if row.Hour < fromMS || row.Hour >= toMS {
			continue
		}
		key := localDayKey(row.Hour, window.loc)
		state := dayState[key]
		if state == nil {
			continue
		}
		state.users[row.UserID] = struct{}{}
		state.sessions[row.SessionID] = struct{}{}
		summarySessions[row.SessionID] = struct{}{}
		hourUsers[localHourOf(row.Hour, window.loc)][row.UserID] = struct{}{}
		runtime := runtimeFor(row.Runtime)
		runtime.users[row.UserID] = struct{}{}
		runtime.sessions[row.SessionID] = struct{}{}
	}

	// Connector presence drives daily online devices.
	for _, row := range connectorActivity {
		key := localDayKey(row.Hour, window.loc)
		state := dayState[key]
		if state == nil {
			continue
		}
		state.connectors[row.ConnectorID] = struct{}{}
	}

	daily := make([]usageDailyPoint, 0, len(dayOrder))
	summaryUsers := map[string]struct{}{}
	summaryConnectors := map[string]struct{}{}
	for _, key := range dayOrder {
		state := dayState[key]
		state.point.ActiveUsers = len(state.users)
		state.point.Sessions = len(state.sessions)
		state.point.ConnectorsOnline = len(state.connectors)
		for id := range state.users {
			summaryUsers[id] = struct{}{}
		}
		for id := range state.connectors {
			summaryConnectors[id] = struct{}{}
		}
		daily = append(daily, state.point)
	}

	startOrdinal := civilOrdinal(window.from)
	endOrdinal := civilOrdinal(window.to)
	wauByDay := rollingActiveCounts(userDays, 7, startOrdinal, endOrdinal)
	mauByDay := rollingActiveCounts(userDays, 30, startOrdinal, endOrdinal)
	totalDailyActive := 0
	for index, day := range days {
		daily[index].Wau = wauByDay[civilOrdinal(day)]
		daily[index].Mau = mauByDay[civilOrdinal(day)]
		totalDailyActive += daily[index].ActiveUsers
	}

	summary := usageSummary{
		ActiveUsers:      len(summaryUsers),
		NewUsers:         len(userCreations),
		TotalUsers:       totalUsers,
		Sessions:         len(summarySessions),
		ConnectorsOnline: len(summaryConnectors),
		TotalConnectors:  totalConnectors,
		TurnDurationMs:   usagePercentiles{},
		ApprovalWaitMs:   usagePercentiles{},
	}
	for _, point := range daily {
		summary.Messages += point.Messages
		summary.Turns += point.Turns
	}
	if len(daily) > 0 {
		summary.AvgDAU = roundTo(float64(totalDailyActive)/float64(len(daily)), 2)
		summary.Wau = daily[len(daily)-1].Wau
		summary.Mau = daily[len(daily)-1].Mau
	}
	if summary.Turns > 0 {
		failed := 0
		for _, point := range daily {
			failed += point.TurnsFailed
		}
		rate := roundTo(float64(failed)/float64(summary.Turns), 4)
		summary.TurnFailureRate = &rate
	}

	hourOfDay := make([]usageHourPoint, 24)
	for hour := 0; hour < 24; hour++ {
		hourOfDay[hour] = usageHourPoint{
			Hour:        hour,
			Messages:    hourMessages[hour],
			ActiveUsers: len(hourUsers[hour]),
		}
	}

	// Runtime device counts come from real presence, not from
	// connector_runtime_types: nothing in the server writes that table, so it
	// always reported 0 even while a connector was running claude. Every
	// connector is advertised as supporting and having claude configured
	// (handleListRuntimes returns exactly that for any paired device), so the
	// honest count for both fields is the non-revoked connectors that
	// heartbeated inside the window. summaryConnectors is built from the
	// connectors.last_seen_at query above and is already de-duplicated.
	seenConnectors := len(summaryConnectors)
	runtimeNames := map[string]string{primaryRuntimeType: primaryRuntimeDisplayName}
	configured := map[string]int{primaryRuntimeType: seenConnectors}
	discovered := map[string]int{primaryRuntimeType: seenConnectors}
	// Keep the runtime row visible even for a window with no activity: the
	// runtime exists independently of whether anyone used it in this range.
	runtimeFor(primaryRuntimeType)
	runtimeRows := make([]usageRuntimeRow, 0, len(runtimes))
	for runtimeType, state := range runtimes {
		name := runtimeNames[runtimeType]
		if name == "" {
			name = runtimeType
		}
		runtimeRows = append(runtimeRows, usageRuntimeRow{
			RuntimeType:          runtimeType,
			DisplayName:          name,
			Messages:             state.messages,
			Sessions:             len(state.sessions),
			ActiveUsers:          len(state.users),
			Turns:                state.turns,
			TurnsFailed:          state.turnsFailed,
			RuntimeErrors:        0,
			ConfiguredConnectors: configured[runtimeType],
			DiscoveredConnectors: discovered[runtimeType],
		})
	}
	sort.Slice(runtimeRows, func(i, j int) bool {
		if runtimeRows[i].Messages != runtimeRows[j].Messages {
			return runtimeRows[i].Messages > runtimeRows[j].Messages
		}
		return runtimeRows[i].RuntimeType < runtimeRows[j].RuntimeType
	})

	// Devices.
	onlineByOS := map[string]map[string]struct{}{}
	for _, row := range connectorActivity {
		osName := row.DeviceOS
		if osName == "" {
			osName = "unknown"
		}
		ids := onlineByOS[osName]
		if ids == nil {
			ids = map[string]struct{}{}
			onlineByOS[osName] = ids
		}
		ids[row.ConnectorID] = struct{}{}
	}
	totalByOS := map[string]int{}
	for _, row := range osCounts {
		totalByOS[row.OS] = row.Count
	}
	osNames := make([]string, 0, len(totalByOS)+len(onlineByOS))
	seenOS := map[string]struct{}{}
	for name := range totalByOS {
		seenOS[name] = struct{}{}
	}
	for name := range onlineByOS {
		seenOS[name] = struct{}{}
	}
	for name := range seenOS {
		osNames = append(osNames, name)
	}
	sort.Slice(osNames, func(i, j int) bool {
		if totalByOS[osNames[i]] != totalByOS[osNames[j]] {
			return totalByOS[osNames[i]] > totalByOS[osNames[j]]
		}
		return osNames[i] < osNames[j]
	})
	byOS := make([]usageOSRow, 0, len(osNames))
	for _, name := range osNames {
		byOS = append(byOS, usageOSRow{
			OS:               name,
			Connectors:       totalByOS[name],
			ConnectorsOnline: len(onlineByOS[name]),
		})
	}

	inventoryDaily := make([]usageInventoryPoint, 0, len(dayOrder))
	runningUsers, runningConnectors := usersBefore, connectorsBefore
	for index, key := range dayOrder {
		runningUsers += daily[index].NewUsers
		runningConnectors += daily[index].Devices
		inventoryDaily = append(inventoryDaily, usageInventoryPoint{
			Date:       key,
			Users:      runningUsers,
			Connectors: runningConnectors,
		})
	}

	var dataSince *string
	if earliest != nil {
		value := localDayKey(*earliest, window.loc)
		dataSince = &value
	}
	generatedAt := timestampNow()

	// User distribution and retention.
	segments, histogram := usageDistribution(userMessages)
	retention := usageRetention(userCreations, userDays, window.loc, today)

	report := usageReportResponse{
		Range: usageRange{
			FromDate:    window.from.Format("2006-01-02"),
			ToDate:      window.to.Format("2006-01-02"),
			Timezone:    window.tz,
			DataSince:   dataSince,
			GeneratedAt: generatedAt,
		},
		Summary:   summary,
		Daily:     daily,
		HourOfDay: hourOfDay,
		Users: usageUsersSection{
			Segments:         segments,
			MessageHistogram: histogram,
			Retention:        retention,
		},
		Runtimes: runtimeRows,
		Devices: usageDevicesSection{
			TotalConnectors:   totalConnectors,
			ConnectorsSeen7d:  seen7d,
			ConnectorsSeen30d: seen30d,
			ByOS:              byOS,
			InventoryDaily:    inventoryDaily,
			InventoryAsOf:     "live",
		},
		Quality: usageQualitySection{
			TurnOutcomes:     outcomeRows(outcomeCounts),
			TurnDurationMs:   usagePercentiles{},
			ApprovalOutcomes: []usageOutcomeRow{},
			ApprovalWaitMs:   usagePercentiles{},
			RuntimeErrors:    0,
		},
		ServerTime: generatedAt,
	}
	httpx.OK(w, report)
}

// rollingActiveCounts counts distinct users active in the trailing window
// ending at each ordinal. Coverage intervals are merged per user so one user
// contributes at most once per day.
func rollingActiveCounts(userDays map[string]map[int]struct{}, window, startOrdinal, endOrdinal int) map[int]int {
	diff := map[int]int{}
	for _, days := range userDays {
		ordered := make([]int, 0, len(days))
		for day := range days {
			ordered = append(ordered, day)
		}
		sort.Ints(ordered)
		hasRun := false
		var low, high int
		for _, day := range ordered {
			runLow, runHigh := day, day+window-1
			if !hasRun {
				low, high, hasRun = runLow, runHigh, true
				continue
			}
			if runLow <= high+1 {
				if runHigh > high {
					high = runHigh
				}
				continue
			}
			markCoverage(diff, low, high, startOrdinal, endOrdinal)
			low, high = runLow, runHigh
		}
		if hasRun {
			markCoverage(diff, low, high, startOrdinal, endOrdinal)
		}
	}
	result := make(map[int]int, endOrdinal-startOrdinal+1)
	running := 0
	for ordinal := startOrdinal; ordinal <= endOrdinal; ordinal++ {
		running += diff[ordinal]
		result[ordinal] = running
	}
	return result
}

func markCoverage(diff map[int]int, low, high, startOrdinal, endOrdinal int) {
	if low < startOrdinal {
		low = startOrdinal
	}
	if high > endOrdinal {
		high = endOrdinal
	}
	if low > high {
		return
	}
	diff[low]++
	diff[high+1]--
}

func usageDistribution(userMessages map[string]int) ([]usageSegment, []usageBucket) {
	values := make([]int, 0, len(userMessages))
	positive := make([]int, 0, len(userMessages))
	for _, count := range userMessages {
		values = append(values, count)
		if count > 0 {
			positive = append(positive, count)
		}
	}
	sort.Ints(values)
	sort.Ints(positive)

	lightMax := 1
	mediumMax := 1
	if len(positive) > 0 {
		lightMax = maxInt(1, percentileNearest(positive, 0.5))
		mediumMax = maxInt(lightMax, percentileNearest(positive, 0.8))
	}
	lightCount, mediumCount, heavyCount := 0, 0, 0
	for _, value := range positive {
		switch {
		case value <= lightMax:
			lightCount++
		case value <= mediumMax:
			mediumCount++
		default:
			heavyCount++
		}
	}
	heavyMin := mediumMax + 1
	segments := []usageSegment{
		{Segment: "light", Min: 1, Max: intPtr(lightMax), Count: lightCount},
		{Segment: "medium", Min: lightMax + 1, Max: intPtr(maxInt(mediumMax, lightMax+1)), Count: mediumCount},
		{Segment: "heavy", Min: heavyMin, Max: nil, Count: heavyCount},
	}

	edges := []int{1}
	if len(positive) > 0 {
		for _, fraction := range []float64{0.5, 0.8, 0.95} {
			edges = append(edges, percentileLower(positive, fraction))
		}
	}
	edgeSet := map[int]struct{}{}
	for _, edge := range edges {
		if edge >= 1 {
			edgeSet[edge] = struct{}{}
		}
	}
	uniqueEdges := make([]int, 0, len(edgeSet))
	for edge := range edgeSet {
		uniqueEdges = append(uniqueEdges, edge)
	}
	sort.Ints(uniqueEdges)

	histogram := make([]usageBucket, 0, len(uniqueEdges)+1)
	appendBucket := func(low int, high *int) {
		count := 0
		for _, value := range values {
			if value >= low && (high == nil || value <= *high) {
				count++
			}
		}
		label := strconv.Itoa(low)
		if high == nil {
			label = fmt.Sprintf("%d+", low)
		} else if *high != low {
			label = fmt.Sprintf("%d-%d", low, *high)
		}
		histogram = append(histogram, usageBucket{Label: label, Min: low, Max: high, Count: count})
	}
	zero := 0
	appendBucket(0, &zero)
	for index, edge := range uniqueEdges {
		var upper *int
		if index+1 < len(uniqueEdges) {
			value := uniqueEdges[index+1] - 1
			if value < edge {
				continue
			}
			upper = &value
		}
		appendBucket(edge, upper)
	}
	return segments, histogram
}

// retentionMember keeps the user identity, because retention is per signup.
type retentionMember struct {
	userID string
	signup int
}

func usageRetention(users []store.UserCreationRow, userDays map[string]map[int]struct{}, loc *time.Location, today time.Time) []usageRetentionCohort {
	cohorts := map[string][]retentionMember{}
	todayOrdinal := civilOrdinal(today)
	for _, signup := range users {
		local := time.UnixMilli(signup.CreatedAt).In(loc)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		mondayOffset := (int(day.Weekday()) + 6) % 7
		weekStart := day.AddDate(0, 0, -mondayOffset)
		key := weekStart.Format("2006-01-02")
		cohorts[key] = append(cohorts[key], retentionMember{
			userID: signup.UserID,
			signup: civilOrdinal(day),
		})
	}
	keys := make([]string, 0, len(cohorts))
	for key := range cohorts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]usageRetentionCohort, 0, len(keys))
	for _, key := range keys {
		start, _ := time.ParseInLocation("2006-01-02", key, loc)
		members := cohorts[key]
		out = append(out, usageRetentionCohort{
			CohortStart: key,
			CohortEnd:   start.AddDate(0, 0, 6).Format("2006-01-02"),
			CohortSize:  len(members),
			D1:          retainedShare(members, userDays, 1, todayOrdinal),
			D7:          retainedShare(members, userDays, 7, todayOrdinal),
			D30:         retainedShare(members, userDays, 30, todayOrdinal),
		})
	}
	return out
}

func retainedShare(members []retentionMember, userDays map[string]map[int]struct{}, horizon, todayOrdinal int) *float64 {
	eligible := 0
	retained := 0
	for _, member := range members {
		if member.signup+horizon > todayOrdinal {
			continue
		}
		eligible++
		days := userDays[member.userID]
		for day := member.signup + 1; day <= member.signup+horizon; day++ {
			if _, ok := days[day]; ok {
				retained++
				break
			}
		}
	}
	if eligible == 0 {
		return nil
	}
	value := roundTo(float64(retained)/float64(eligible), 4)
	return &value
}

func percentileNearest(ordered []int, fraction float64) int {
	if len(ordered) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(ordered))*fraction)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func percentileLower(ordered []int, fraction float64) int {
	if len(ordered) == 0 {
		return 0
	}
	index := int(float64(len(ordered)-1) * fraction)
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func outcomeRows(counts map[string]int) []usageOutcomeRow {
	order := []string{"completed", "interrupted", "cancelled", "failed"}
	seen := map[string]struct{}{}
	rows := make([]usageOutcomeRow, 0, len(order)+len(counts))
	for _, name := range order {
		rows = append(rows, usageOutcomeRow{Outcome: name, Count: counts[name]})
		seen[name] = struct{}{}
	}
	extra := make([]string, 0, len(counts))
	for name := range counts {
		if _, ok := seen[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		rows = append(rows, usageOutcomeRow{Outcome: name, Count: counts[name]})
	}
	return rows
}

func intPtr(value int) *int { return &value }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func roundTo(value float64, places int) float64 {
	factor := math.Pow(10, float64(places))
	return math.Round(value*factor) / factor
}
