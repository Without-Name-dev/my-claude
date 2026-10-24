package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// The first-party apps sign in by opening the web login in a browser session
// and are then redirected back to their own URL scheme with a one-time code.
// The code is redeemed at the token endpoint with a PKCE verifier.

// handleOAuthAuthorizeRedirect finishes a browser sign-in by redirecting back to
// the app with a short-lived code.
func (a *App) handleOAuthAuthorizeRedirect(w http.ResponseWriter, r *http.Request, user store.User) {
	q := r.URL.Query()
	redirect, err := a.issueAuthorizationCode(r, user, authorizationRequest{
		ResponseType:        q.Get("response_type"),
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// handleOAuthAuthorize returns the redirect URL as JSON so the web client can
// hand it to the in-app browser without navigating itself.
func (a *App) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		ResponseType        string  `json:"response_type"`
		ClientID            string  `json:"client_id"`
		RedirectURI         string  `json:"redirect_uri"`
		CodeChallenge       string  `json:"code_challenge"`
		CodeChallengeMethod string  `json:"code_challenge_method"`
		Scope               string  `json:"scope"`
		State               *string `json:"state"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	state := ""
	if body.State != nil {
		state = *body.State
	}
	redirect, err := a.issueAuthorizationCode(r, user, authorizationRequest{
		ResponseType:        body.ResponseType,
		ClientID:            body.ClientID,
		RedirectURI:         body.RedirectURI,
		CodeChallenge:       body.CodeChallenge,
		CodeChallengeMethod: body.CodeChallengeMethod,
		Scope:               body.Scope,
		State:               state,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"redirectUrl": redirect, "serverTime": timestampNow()})
}

type authorizationRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	State               string
}

// issueAuthorizationCode validates the request and returns the URL to send the
// browser to.
func (a *App) issueAuthorizationCode(r *http.Request, user store.User, req authorizationRequest) (string, error) {
	if req.ResponseType != "code" {
		return "", httpx.NewError(http.StatusUnprocessableEntity, "response_type must be code")
	}
	method := req.CodeChallengeMethod
	if method == "" {
		method = "S256"
	}
	if method != "S256" {
		return "", httpx.NewError(http.StatusUnprocessableEntity, "code_challenge_method must be S256")
	}
	if strings.TrimSpace(req.CodeChallenge) == "" {
		return "", httpx.NewError(http.StatusUnprocessableEntity, "code_challenge is required")
	}

	client, err := a.db.OAuthClient(r.Context(), req.ClientID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", httpx.NotFound("oauth client not found")
		}
		return "", err
	}
	if !client.AllowsRedirect(req.RedirectURI) {
		return "", httpx.NewError(http.StatusUnprocessableEntity, "redirect uri is not allowed")
	}

	code, err := a.db.CreateOAuthAuthorizationCode(r.Context(), store.OAuthAuthorizationCode{
		ClientID:            req.ClientID,
		UserID:              user.ID,
		RedirectURI:         req.RedirectURI,
		Scope:               req.Scope,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: method,
	})
	if err != nil {
		return "", err
	}

	params := url.Values{"code": {code}}
	if req.State != "" {
		params.Set("state", req.State)
	}
	separator := "?"
	if strings.Contains(req.RedirectURI, "?") {
		separator = "&"
	}
	return req.RedirectURI + separator + params.Encode(), nil
}

// handleOAuthToken redeems an authorization code for a bearer token.
func (a *App) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.Fail(w, httpx.BadRequest("invalid form body"))
		return
	}
	form := r.PostForm
	if form.Get("grant_type") != "authorization_code" {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "grant_type must be authorization_code"))
		return
	}
	clientID := form.Get("client_id")
	redirectURI := form.Get("redirect_uri")
	code := form.Get("code")
	verifier := form.Get("code_verifier")

	client, err := a.db.OAuthClient(r.Context(), clientID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("oauth client not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	if !client.AllowsRedirect(redirectURI) {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "redirect uri is not allowed"))
		return
	}

	userID, scope, challenge, _, err := a.db.ConsumeOAuthAuthorizationCode(r.Context(), code, clientID, redirectURI)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpx.Fail(w, httpx.BadRequest("authorization code is not valid"))
		case errors.Is(err, store.ErrCodeAlreadyUsed):
			httpx.Fail(w, httpx.BadRequest("authorization code was already used"))
		case errors.Is(err, store.ErrCodeExpired):
			httpx.Fail(w, httpx.BadRequest("authorization code expired"))
		default:
			httpx.Fail(w, err)
		}
		return
	}
	if !pkceMatches(verifier, challenge) {
		httpx.Fail(w, httpx.BadRequest("code_verifier does not match the challenge"))
		return
	}

	token, err := auth.CreateUserAccessToken(userID, auth.DefaultUserExpiresIn)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   auth.DefaultUserExpiresIn,
		"scope":        scope,
	})
}

// pkceMatches recomputes the S256 challenge from the verifier.
func pkceMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}
