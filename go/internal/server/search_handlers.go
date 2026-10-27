package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleSearch performs global search across messages
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()

	// Parse query parameters
	query := r.URL.Query().Get("q")
	if query == "" {
		httpx.Fail(w, httpx.BadRequest("query parameter 'q' is required"))
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// Parse optional session_ids filter
	var sessionIDs []string
	if sessionIDsParam := r.URL.Query().Get("session_ids"); sessionIDsParam != "" {
		sessionIDs = strings.Split(sessionIDsParam, ",")
	}

	params := store.SearchParams{
		Query:      query,
		Limit:      limit,
		Offset:     offset,
		SessionIDs: sessionIDs,
	}

	results, total, err := a.db.SearchMessages(ctx, user.ID, params)
	if err != nil {
		a.log.ErrorContext(ctx, "search failed", "error", err, "user_id", user.ID, "query", query)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "search failed"))
		return
	}

	httpx.OK(w, map[string]any{
		"results": results,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// handleSearchSuggestions returns autocomplete suggestions
func (a *App) handleSearchSuggestions(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()

	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		httpx.OK(w, map[string]any{"suggestions": []string{}})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 5
	}

	suggestions, err := a.db.SearchSuggestions(ctx, user.ID, prefix, limit)
	if err != nil {
		a.log.ErrorContext(ctx, "search suggestions failed", "error", err, "user_id", user.ID, "prefix", prefix)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "suggestions failed"))
		return
	}

	httpx.OK(w, map[string]any{"suggestions": suggestions})
}
