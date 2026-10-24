package server

import (
	"net/http"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
)

// handleAnnouncement serves the operator's notice banner.
//
// The web client fetches this on every page load and tolerates null, so an
// instance that never configures one simply returns nothing.
func (a *App) handleAnnouncement(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	raw, err := a.setting(r.Context(), "public_announcement")
	if err != nil || raw == "" {
		httpx.OK(w, map[string]any{"announcement": nil})
		return
	}
	var stored struct {
		Enabled     bool   `json:"enabled"`
		Markdown    string `json:"markdown"`
		PublishedAt string `json:"publishedAt"`
	}
	if err := jsonUnmarshal(raw, &stored); err != nil || !stored.Enabled || stored.PublishedAt == "" {
		httpx.OK(w, map[string]any{"announcement": nil})
		return
	}
	httpx.OK(w, map[string]any{
		"announcement": map[string]any{
			"markdown":    stored.Markdown,
			"publishedAt": stored.PublishedAt,
		},
	})
}
