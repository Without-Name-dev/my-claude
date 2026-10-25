package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginThrottleAnswers429(t *testing.T) {
	t.Setenv("AGENT_SERVER_PASSWORD_ITERATIONS", "1000")
	app, _ := newAdminTestApp(t)
	handler := app.Handler()

	for i := 0; i < loginAccountLimit; i++ {
		rec, _ := postJSON(t, handler, "/api/v2/auth/login", "", map[string]any{
			"email": "nobody@example.test", "password": "wrong",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec, _ := postJSON(t, handler, "/api/v2/auth/login", "", map[string]any{
		"email": "nobody@example.test", "password": "wrong",
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d failures, got %d", loginAccountLimit, rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

func TestPasswordChangeRevokesOlderTokens(t *testing.T) {
	t.Setenv("AGENT_SERVER_PASSWORD_ITERATIONS", "1000")
	app, st := newAdminTestApp(t)
	handler := app.Handler()

	user := createTestUser(t, st, "rotate@example.test", "member")

	// A token minted before the fingerprint claim existed still authenticates:
	// clients already hold those for up to the session lifetime.
	legacy := testToken(t, user)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+legacy)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a legacy token stopped working: %d %s", rec.Code, rec.Body.String())
	}

	rec, login := postJSON(t, handler, "/api/v2/auth/login", "", map[string]any{
		"email": user.Email, "password": "secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	sessionToken, _ := login["accessToken"].(string)

	rec, _ = postJSON(t, handler, "/api/v2/auth/change-password", sessionToken, map[string]any{
		"newPasswordVerifier": "verifier-value",
		"newPasswordSalt":     "salt-value",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v2/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token minted before the password change still works: %d %s",
			rec.Code, rec.Body.String())
	}
}
