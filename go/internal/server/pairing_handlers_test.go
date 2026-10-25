package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// postJSON exercises a handler through the real router so route wiring and the
// shared middleware are part of the result.
func postJSON(t *testing.T, handler http.Handler, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec, decoded
}

func TestPairingRoundTripAndFailedClaimLeavesNoConnector(t *testing.T) {
	app, st := newAdminTestApp(t)
	handler := app.Handler()

	rec, started := postJSON(t, handler, "/api/v2/pairing/start", "", map[string]any{"ttlSeconds": 5})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	code, ok := started["code"].(string)
	if !ok || code == "" {
		t.Fatalf("start returned no code: %v", started)
	}

	user := createTestUser(t, st, "owner@example.test", "admin")
	token := testToken(t, user)

	rec, claimed := postJSON(t, handler, "/api/v2/pairing/claim", token, map[string]any{
		"code": code,
		"name": "Box",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	connector, ok := claimed["connector"].(map[string]any)
	if !ok || connector["name"] != "Box" {
		t.Fatalf("claim returned %v", claimed)
	}

	// Losing the compare-and-swap must not leave a second connector behind: the
	// insert and the claim share a transaction and roll back together.
	before, err := st.ListConnectors(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if rec, _ := postJSON(t, handler, "/api/v2/pairing/claim", token, map[string]any{"code": code}); rec.Code == http.StatusOK {
		t.Fatal("the same code was claimed twice")
	}
	after, err := st.ListConnectors(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a failed claim added a connector: %d -> %d", len(before), len(after))
	}

	_, polled := postJSON(t, handler, "/api/v2/pairing/poll", "", map[string]any{
		"pairingId": started["pairingId"],
	})
	if polled["config"] == nil {
		t.Fatalf("poll did not hand out the configuration: %v", polled)
	}
}
