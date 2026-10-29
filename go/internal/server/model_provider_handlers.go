package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// ModelProviderView is a provider as iOS and Web render it. The credential is
// never included: only credentialConfigured crosses the wire, and no log or
// error message may carry the stored secret.
type ModelProviderView struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Protocol             string          `json:"protocol"`
	BaseURL              string          `json:"baseUrl"`
	AuthType             string          `json:"authType"`
	ModelID              string          `json:"modelId"`
	ModelDisplayName     string          `json:"modelDisplayName"`
	CredentialConfigured bool            `json:"credentialConfigured"`
	Enabled              bool            `json:"enabled"`
	Revision             int64           `json:"revision"`
	Capabilities         json.RawMessage `json:"capabilities"`
	CreatedAt            string          `json:"createdAt"`
	UpdatedAt            string          `json:"updatedAt"`
}

func modelProviderView(p store.ModelProvider) ModelProviderView {
	caps := json.RawMessage(p.Capabilities)
	if len(caps) == 0 {
		caps = json.RawMessage("{}")
	}
	return ModelProviderView{
		ID:                   p.ID,
		Name:                 p.Name,
		Protocol:             p.Protocol,
		BaseURL:              p.BaseURL,
		AuthType:             p.AuthType,
		ModelID:              p.ModelID,
		ModelDisplayName:     p.ModelName,
		CredentialConfigured: p.Credential != "",
		Enabled:              p.Enabled,
		Revision:             p.Revision,
		Capabilities:         caps,
		CreatedAt:            isoTime(p.CreatedAt),
		UpdatedAt:            isoTime(p.UpdatedAt),
	}
}

// handleListModelProviders lists the caller's providers. No credentials.
func (a *App) handleListModelProviders(w http.ResponseWriter, r *http.Request, user store.User) {
	providers, err := a.db.ListModelProviders(r.Context(), user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	views := make([]ModelProviderView, 0, len(providers))
	for _, p := range providers {
		views = append(views, modelProviderView(p))
	}
	httpx.OK(w, map[string]any{"providers": views, "serverTime": timestampNow()})
}

// providerBody is the create/PATCH payload. Credential handling is explicit:
// an absent credential field always means "keep the stored value"; clearing
// uses clearCredential, replacing uses credential.
type providerBody struct {
	Name             *string `json:"name"`
	Protocol         *string `json:"protocol"`
	BaseURL          *string `json:"baseUrl"`
	AuthType         *string `json:"authType"`
	ModelID          *string `json:"modelId"`
	ModelDisplayName *string `json:"modelDisplayName"`
	Enabled          *bool   `json:"enabled"`
	Capabilities     any     `json:"capabilities"`
	Credential       *string `json:"credential"`
	ClearCredential  *bool   `json:"clearCredential"`
	ExpectedRevision *int64  `json:"expectedRevision"`
}

func (b *providerBody) toProvider(userID string) store.ModelProvider {
	p := store.ModelProvider{UserID: userID}
	if b.Name != nil {
		p.Name = *b.Name
	}
	if b.Protocol != nil {
		p.Protocol = *b.Protocol
	}
	if b.BaseURL != nil {
		p.BaseURL = *b.BaseURL
	}
	if b.AuthType != nil {
		p.AuthType = *b.AuthType
	}
	if b.ModelID != nil {
		p.ModelID = *b.ModelID
	}
	if b.ModelDisplayName != nil {
		p.ModelName = *b.ModelDisplayName
	}
	if b.Enabled != nil {
		p.Enabled = *b.Enabled
	}
	if b.Credential != nil {
		p.Credential = *b.Credential
	}
	if b.Capabilities != nil {
		if raw, err := json.Marshal(b.Capabilities); err == nil {
			p.Capabilities = string(raw)
		}
	}
	return p
}

func (b *providerBody) toPatch() store.ProviderPatch {
	patch := store.ProviderPatch{
		Name:      b.Name,
		Protocol:  b.Protocol,
		BaseURL:   b.BaseURL,
		AuthType:  b.AuthType,
		ModelID:   b.ModelID,
		ModelName: b.ModelDisplayName,
		Enabled:   b.Enabled,
	}
	if b.Capabilities != nil {
		if raw, err := json.Marshal(b.Capabilities); err == nil {
			patch.Capabilities = stringPtr(string(raw))
		}
	}
	if b.ClearCredential != nil && *b.ClearCredential {
		patch.ClearCredential = true
	} else if b.Credential != nil {
		patch.ReplaceCredential = true
		patch.Credential = *b.Credential
	}
	return patch
}

func stringPtr(v string) *string { return &v }

// handleCreateModelProvider creates a provider plus its one model.
func (a *App) handleCreateModelProvider(w http.ResponseWriter, r *http.Request, user store.User) {
	var body providerBody
	if err := httpx.Decode(r, &body); err != nil {
		a.failModelGateway(w, err)
		return
	}
	// A create needs the defining fields present as values, not just pointers.
	in := body.toProvider(user.ID)
	if body.Name == nil || body.Protocol == nil || body.BaseURL == nil || body.ModelID == nil {
		httpx.Fail(w, httpx.BadRequest("name, protocol, baseUrl and modelId are required"))
		return
	}
	if !in.Enabled {
		// Default a create to enabled unless the client explicitly disabled it.
		if body.Enabled == nil {
			in.Enabled = true
		}
	}
	created, err := a.db.CreateModelProvider(r.Context(), in)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	a.gateway.invalidateUser(user.ID)
	a.publishCatalogsForUser(r.Context(), user.ID)
	httpx.OK(w, map[string]any{"provider": modelProviderView(created), "serverTime": timestampNow()})
}

// handleGetModelProvider returns one provider. No credentials.
func (a *App) handleGetModelProvider(w http.ResponseWriter, r *http.Request, user store.User) {
	p, err := a.db.ModelProviderOwned(r.Context(), r.PathValue("provider_id"), user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	httpx.OK(w, map[string]any{"provider": modelProviderView(p), "serverTime": timestampNow()})
}

// handlePatchModelProvider edits a provider. expectedRevision guards against
// two devices editing at once; a mismatch answers 409, never a silent merge.
func (a *App) handlePatchModelProvider(w http.ResponseWriter, r *http.Request, user store.User) {
	var body providerBody
	if err := httpx.DecodeOptional(r, &body); err != nil {
		a.failModelGateway(w, err)
		return
	}
	var expected int64
	if body.ExpectedRevision != nil {
		expected = *body.ExpectedRevision
	}
	updated, err := a.db.UpdateModelProvider(r.Context(), r.PathValue("provider_id"), user.ID, expected, body.toPatch())
	if err != nil {
		if errors.Is(err, store.ErrProviderRevision) {
			httpx.Fail(w, httpx.NewError(http.StatusConflict,
				"provider was modified by another client; refresh and edit again"))
			return
		}
		a.failModelGateway(w, err)
		return
	}
	// An edit changes every route that could resolve to this provider, and
	// requests already reading a cached decision must not keep stale
	// credentials. Clear the account's cache and refresh the live catalogs.
	a.gateway.invalidateUser(user.ID)
	a.publishCatalogsForUser(r.Context(), user.ID)
	httpx.OK(w, map[string]any{"provider": modelProviderView(updated), "serverTime": timestampNow()})
}

// handleDeleteModelProvider refuses a blind delete when routes still point at
// the provider: the response names the references so the person can decide.
// force=true (with the same reference report in hand) performs the delete;
// explicit routes then report "provider deleted" instead of silently
// inheriting the device default.
func (a *App) handleDeleteModelProvider(w http.ResponseWriter, r *http.Request, user store.User) {
	id := r.PathValue("provider_id")
	provider, err := a.db.ModelProviderOwned(r.Context(), id, user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	refs, err := a.db.CountProviderReferences(r.Context(), provider.ID, user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	force := strings.EqualFold(r.URL.Query().Get("force"), "true") || r.URL.Query().Get("force") == "1"
	if refs.Total() > 0 && !force {
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"detail": map[string]any{
				"code":              "provider_referenced",
				"message":           "provider is referenced by routes; pass force=true after reviewing them",
				"connectorDefaults": refs.ConnectorDefaults,
				"explicitRoutes":    refs.ExplicitRoutes,
			},
		})
		return
	}
	if err := a.db.DeleteModelProviderForce(r.Context(), provider.ID, user.ID); err != nil {
		a.failModelGateway(w, err)
		return
	}
	a.gateway.invalidateUser(user.ID)
	a.gateway.forgetProvider(provider.ID)
	a.publishCatalogsForUser(r.Context(), user.ID)
	httpx.OK(w, map[string]any{
		"ok":                true,
		"connectorDefaults": refs.ConnectorDefaults,
		"explicitRoutes":    refs.ExplicitRoutes,
		"serverTime":        timestampNow(),
	})
}

// handleCheckModelProvider runs a user-initiated connectivity check. With
// mode "request" it sends a minimal real model request to the provider, which
// may cost money; the default "connect" mode only probes reachability and
// authentication. Checks are never run implicitly on page load.
func (a *App) handleCheckModelProvider(w http.ResponseWriter, r *http.Request, user store.User) {
	provider, err := a.db.ModelProviderOwned(r.Context(), r.PathValue("provider_id"), user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	var body struct {
		Mode *string `json:"mode"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		a.failModelGateway(w, err)
		return
	}
	mode := "connect"
	if body.Mode != nil {
		mode = *body.Mode
	}
	if mode != "connect" && mode != "request" {
		httpx.Fail(w, httpx.BadRequest("mode must be connect or request"))
		return
	}
	result := a.gateway.checkProvider(r.Context(), provider, mode == "request")
	if err := a.db.SetProviderCheckResult(r.Context(), provider.ID, user.ID, result); err != nil {
		a.log.Warn("provider check result not stored", "provider", provider.ID, "err", err)
	}
	check := map[string]any{
		"mode":      result.Mode,
		"reachable": result.Reachable,
		"authOk":    result.AuthOK,
		"detail":    result.Detail,
		// Unix milliseconds, matching the stored capabilities.check shape.
		"at": result.At,
	}
	// A connect probe never gets a status; omitting it beats sending 0, which
	// clients rendered as "status code 0".
	if result.Status != 0 {
		check["status"] = result.Status
	}
	httpx.OK(w, map[string]any{
		"providerId": provider.ID,
		"check":      check,
		"serverTime": timestampNow(),
	})
}

// providerCheckTimeout bounds a user-initiated check.
const providerCheckTimeout = 20 * time.Second

// checkResult is the reduced outcome shape shared by the check path.
type providerCheckOutcome = store.ProviderCheckResult

// ioLimitReader mirrors the payload bound the gateway itself enforces on
// upstream responses during a check.
func limitedRead(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	return io.ReadAll(io.LimitReader(body, limit))
}
