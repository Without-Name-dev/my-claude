package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SearchResult represents a single search result item
type SearchResult struct {
	SessionID   string          `json:"session_id"`
	SessionTitle string         `json:"session_title"`
	ItemID      string          `json:"item_id"`
	ItemType    string          `json:"item_type"`
	Role        string          `json:"role,omitempty"`
	Content     string          `json:"content"`
	Snippet     string          `json:"snippet"`
	ItemTime    *int64          `json:"item_time,omitempty"`
	Rank        float64         `json:"rank"`
}

// SearchParams contains search query parameters
type SearchParams struct {
	Query      string
	Limit      int
	Offset     int
	SessionIDs []string // Filter by specific sessions
}

// SearchMessages performs full-text search across all timeline messages
func (s *Store) SearchMessages(ctx context.Context, userID string, params SearchParams) ([]SearchResult, int, error) {
	if params.Query == "" {
		return []SearchResult{}, 0, nil
	}
	if params.Limit <= 0 {
		params.Limit = 20
	}
	if params.Limit > 100 {
		params.Limit = 100
	}

	// Escape FTS5 special characters and prepare query
	query := escapeFTS5Query(params.Query)

	// Build the WHERE clause for session filtering
	sessionFilter := ""
	args := []any{userID, query}

	if len(params.SessionIDs) > 0 {
		placeholders := make([]string, len(params.SessionIDs))
		for i, sid := range params.SessionIDs {
			placeholders[i] = "?"
			args = append(args, sid)
		}
		sessionFilter = fmt.Sprintf(" AND ti.session_id IN (%s)", strings.Join(placeholders, ","))
	}

	args = append(args, params.Limit, params.Offset)

	// First, get total count
	countQuery := fmt.Sprintf(`
		SELECT COUNT(DISTINCT fts.rowid)
		FROM timeline_search_fts fts
		INNER JOIN timeline_items ti ON ti.rowid = fts.rowid
		INNER JOIN sessions s ON s.id = ti.session_id
		INNER JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ?
		  AND timeline_search_fts MATCH ?
		  %s
	`, sessionFilter)

	var total int
	countArgs := args[:len(args)-2] // Remove limit and offset
	err := s.read.QueryRowContext(ctx, countQuery, countArgs...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	// Now get the actual results with snippets
	searchQuery := fmt.Sprintf(`
		SELECT
			ti.session_id,
			COALESCE(s.title, ''),
			ti.id,
			ti.type,
			COALESCE(ti.role, ''),
			json_extract(ti.payload_json, '$.text'),
			snippet(timeline_search_fts, 2, '<mark>', '</mark>', '...', 64) as snippet,
			ti.item_time,
			bm25(timeline_search_fts) as rank
		FROM timeline_search_fts fts
		INNER JOIN timeline_items ti ON ti.rowid = fts.rowid
		INNER JOIN sessions s ON s.id = ti.session_id
		INNER JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ?
		  AND timeline_search_fts MATCH ?
		  %s
		ORDER BY rank
		LIMIT ? OFFSET ?
	`, sessionFilter)

	rows, err := s.read.QueryContext(ctx, searchQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var content sql.NullString
		err := rows.Scan(
			&r.SessionID,
			&r.SessionTitle,
			&r.ItemID,
			&r.ItemType,
			&r.Role,
			&content,
			&r.Snippet,
			&r.ItemTime,
			&r.Rank,
		)
		if err != nil {
			return nil, 0, fmt.Errorf("scan search result: %w", err)
		}
		if content.Valid {
			r.Content = content.String
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate search results: %w", err)
	}

	return results, total, nil
}

// escapeFTS5Query escapes special FTS5 characters in user query
func escapeFTS5Query(query string) string {
	// FTS5 special characters: " * ( ) AND OR NOT
	query = strings.ReplaceAll(query, `"`, `""`)

	// Wrap in quotes for phrase search if it contains spaces
	if strings.Contains(query, " ") {
		return `"` + query + `"`
	}

	// For single words, use prefix matching
	return query + "*"
}

// SearchSuggestions returns autocomplete suggestions based on recent searches
func (s *Store) SearchSuggestions(ctx context.Context, userID string, prefix string, limit int) ([]string, error) {
	if prefix == "" || limit <= 0 {
		return []string{}, nil
	}
	if limit > 10 {
		limit = 10
	}

	// Return recent unique search terms from user's messages
	query := `
		SELECT DISTINCT json_extract(ti.payload_json, '$.text') as text
		FROM timeline_items ti
		INNER JOIN sessions s ON s.id = ti.session_id
		INNER JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ?
		  AND ti.type = 'message'
		  AND ti.role = 'user'
		  AND json_extract(ti.payload_json, '$.text') LIKE ?
		ORDER BY ti.item_time DESC
		LIMIT ?
	`

	rows, err := s.read.QueryContext(ctx, query, userID, prefix+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("get search suggestions: %w", err)
	}
	defer rows.Close()

	var suggestions []string
	for rows.Next() {
		var text sql.NullString
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("scan suggestion: %w", err)
		}
		if text.Valid && text.String != "" {
			suggestions = append(suggestions, text.String)
		}
	}

	return suggestions, rows.Err()
}
