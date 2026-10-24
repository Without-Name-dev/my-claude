package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Project groups the sessions that share a working directory on one connector.
type Project struct {
	ID              string
	UserID          string
	ConnectorID     string
	Name            string
	WorkspacePath   string
	WorkspaceKey    string
	ManuallyCreated bool
	Pinned          bool
	PinnedAt        *int64
	CreatedAt       int64
	UpdatedAt       int64
}

const projectColumns = `id, user_id, connector_id, name, workspace_path, workspace_key,
	manually_created, pinned, pinned_at, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var manual, pinned int
	err := row.Scan(&p.ID, &p.UserID, &p.ConnectorID, &p.Name, &p.WorkspacePath,
		&p.WorkspaceKey, &manual, &pinned, &p.PinnedAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, err
	}
	p.ManuallyCreated = manual != 0
	p.Pinned = pinned != 0
	return p, nil
}

// BaseName is the last element of a path, whichever separator it uses.
//
// filepath.Base follows the host OS, and this server runs on Linux while the
// paths it receives are Windows paths — so "C:\Users\me" would come back
// whole. Both separators have to be honoured.
func BaseName(value string) string {
	trimmed := strings.TrimRight(value, "/\\")
	if trimmed == "" {
		return value
	}
	if index := strings.LastIndexAny(trimmed, "/\\"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// WorkspaceKey normalises a working directory so the same folder discovered by
// two different spellings maps to one project.
func WorkspaceKey(path string) string {
	key := strings.TrimSpace(path)
	if key == "" {
		return ""
	}
	key = filepath.ToSlash(filepath.Clean(key))
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

// ListProjects returns a user's projects, pinned first then most recent.
func (s *Store) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT `+projectColumns+` FROM projects
		WHERE user_id = ?
		ORDER BY pinned DESC, updated_at DESC, created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectByID returns one project owned by the given user.
func (s *Store) ProjectByID(ctx context.Context, id, userID string) (Project, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ? AND user_id = ?`, id, userID)
	return scanProject(row)
}

// CreateProject inserts a project, or returns the existing one when the same
// connector already has that workspace.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	now := time.Now().UnixMilli()
	if p.ID == "" {
		p.ID = NewID("prj")
	}
	if p.WorkspaceKey == "" {
		p.WorkspaceKey = WorkspaceKey(p.WorkspacePath)
	}
	p.CreatedAt, p.UpdatedAt = now, now

	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, user_id, connector_id, name, workspace_path,
			                      workspace_key, manually_created, pinned, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			p.ID, p.UserID, p.ConnectorID, p.Name, p.WorkspacePath, p.WorkspaceKey,
			boolInt(p.ManuallyCreated), now, now)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// EnsureProject returns the project for a workspace, creating it when missing.
// Discovery calls this for every directory it finds sessions in.
func (s *Store) EnsureProject(ctx context.Context, userID, connectorID, workspacePath, name string) (Project, error) {
	key := WorkspaceKey(workspacePath)
	var id string
	err := s.Read().QueryRowContext(ctx, `
		SELECT id FROM projects
		WHERE connector_id = ? AND workspace_key = ?`, connectorID, key).Scan(&id)
	if err == nil {
		return s.ProjectByID(ctx, id, userID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Project{}, err
	}
	if name == "" {
		name = BaseName(workspacePath)
	}
	project, err := s.CreateProject(ctx, Project{
		UserID:        userID,
		ConnectorID:   connectorID,
		Name:          name,
		WorkspacePath: workspacePath,
		WorkspaceKey:  key,
	})
	if err != nil {
		// A concurrent discovery may have won the race; read it back.
		var existing string
		if readErr := s.Read().QueryRowContext(ctx, `
			SELECT id FROM projects WHERE connector_id = ? AND workspace_key = ?`,
			connectorID, key).Scan(&existing); readErr == nil {
			return s.ProjectByID(ctx, existing, userID)
		}
		return Project{}, err
	}
	return project, nil
}

// UpdateProject changes the name or the pinned flag.
func (s *Store) UpdateProject(ctx context.Context, id string, name *string, pinned *bool) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if name != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`, *name, now, id); err != nil {
				return err
			}
		}
		if pinned != nil {
			var pinnedAt any
			if *pinned {
				pinnedAt = now
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects SET pinned = ?, pinned_at = ?, updated_at = ? WHERE id = ?`,
				boolInt(*pinned), pinnedAt, now, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteProject removes it and detaches its sessions.
func (s *Store) DeleteProject(ctx context.Context, id string) (int64, error) {
	var detached int64
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET project_id = NULL WHERE project_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
		if err != nil {
			return err
		}
		detached, _ = res.RowsAffected()
		return nil
	})
	return detached, err
}

// SessionCounts reports active and archived session counts per project.
func (s *Store) SessionCounts(ctx context.Context, userID string) (map[string][2]int, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT s.project_id, s.archived, count(*)
		FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ? AND s.project_id IS NOT NULL
		GROUP BY s.project_id, s.archived`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][2]int{}
	for rows.Next() {
		var projectID sql.NullString
		var archived int
		var count int
		if err := rows.Scan(&projectID, &archived, &count); err != nil {
			return nil, err
		}
		if !projectID.Valid {
			continue
		}
		counts := out[projectID.String]
		if archived != 0 {
			counts[1] = count
		} else {
			counts[0] = count
		}
		out[projectID.String] = counts
	}
	return out, rows.Err()
}

// ProjectLastActivity returns the newest activity time per project.
func (s *Store) ProjectLastActivity(ctx context.Context, userID string) (map[string]int64, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT s.project_id, max(coalesce(s.last_activity_at, s.updated_at))
		FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ? AND s.project_id IS NOT NULL
		GROUP BY s.project_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var projectID sql.NullString
		var at int64
		if err := rows.Scan(&projectID, &at); err != nil {
			return nil, err
		}
		if projectID.Valid {
			out[projectID.String] = at
		}
	}
	return out, rows.Err()
}
