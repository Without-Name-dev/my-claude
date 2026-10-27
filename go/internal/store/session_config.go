package store

import (
	"context"
	"database/sql"
	"time"
)

// SessionConfig represents session configuration
type SessionConfig struct {
	SessionID   string    `json:"sessionId"`
	EffortLevel string    `json:"effortLevel"`
	ModelName   string    `json:"modelName"`
	FastMode    bool      `json:"fastMode"`
	AutoMode    bool      `json:"autoMode"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// AgentExecution represents an agent execution for workflow visualization
type AgentExecution struct {
	ID           string     `json:"id"`
	SessionID    string     `json:"sessionId"`
	ParentID     *string    `json:"parentId,omitempty"`
	Label        string     `json:"label"`
	Phase        *string    `json:"phase,omitempty"`
	Status       string     `json:"status"` // pending, running, completed, error, cancelled
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	TokenCount   int        `json:"tokenCount"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

// WorkflowRun represents a workflow execution
type WorkflowRun struct {
	ID           string     `json:"id"`
	SessionID    string     `json:"sessionId"`
	Name         string     `json:"name"`
	Status       string     `json:"status"` // running, completed, error, cancelled
	CurrentPhase *string    `json:"currentPhase,omitempty"`
	PhaseCount   int        `json:"phaseCount"`
	AgentCount   int        `json:"agentCount"`
	TotalTokens  int        `json:"totalTokens"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

// GetSessionConfig retrieves session configuration
func (s *Store) GetSessionConfig(ctx context.Context, sessionID string) (*SessionConfig, error) {
	var cfg SessionConfig
	var updatedAt int64

	err := s.Read().QueryRowContext(ctx, `
		SELECT session_id, effort_level, model_name, fast_mode, auto_mode, updated_at
		FROM session_configs
		WHERE session_id = ?`, sessionID).Scan(
		&cfg.SessionID, &cfg.EffortLevel, &cfg.ModelName,
		&cfg.FastMode, &cfg.AutoMode, &updatedAt)

	if err == sql.ErrNoRows {
		// Return default config
		return &SessionConfig{
			SessionID:   sessionID,
			EffortLevel: "medium",
			ModelName:   "claude-sonnet-5-5",
			FastMode:    false,
			AutoMode:    false,
			UpdatedAt:   time.Now(),
		}, nil
	}
	if err != nil {
		return nil, err
	}

	cfg.UpdatedAt = time.UnixMilli(updatedAt)
	return &cfg, nil
}

// UpsertSessionConfig creates or updates session configuration
func (s *Store) UpsertSessionConfig(ctx context.Context, cfg SessionConfig) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO session_configs (session_id, effort_level, model_name, fast_mode, auto_mode, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				effort_level = excluded.effort_level,
				model_name = excluded.model_name,
				fast_mode = excluded.fast_mode,
				auto_mode = excluded.auto_mode,
				updated_at = excluded.updated_at`,
			cfg.SessionID, cfg.EffortLevel, cfg.ModelName,
			cfg.FastMode, cfg.AutoMode, cfg.UpdatedAt.UnixMilli())
		return err
	})
}

// CreateAgentExecution creates a new agent execution record
func (s *Store) CreateAgentExecution(ctx context.Context, agent AgentExecution) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_executions (id, session_id, parent_id, label, phase, status,
				started_at, finished_at, token_count, error_message)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			agent.ID, agent.SessionID, agent.ParentID, agent.Label, agent.Phase, agent.Status,
			timeToMilli(agent.StartedAt), timeToMilli(agent.FinishedAt),
			agent.TokenCount, agent.ErrorMessage)
		return err
	})
}

// UpdateAgentExecution updates agent execution status
func (s *Store) UpdateAgentExecution(ctx context.Context, id, status string, finishedAt *time.Time, tokenCount int, errorMsg string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE agent_executions
			SET status = ?, finished_at = ?, token_count = ?, error_message = ?
			WHERE id = ?`,
			status, timeToMilli(finishedAt), tokenCount, errorMsg, id)
		return err
	})
}

// GetAgentExecutions retrieves all agents for a session
func (s *Store) GetAgentExecutions(ctx context.Context, sessionID string) ([]AgentExecution, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT id, session_id, parent_id, label, phase, status,
			started_at, finished_at, token_count, error_message
		FROM agent_executions
		WHERE session_id = ?
		ORDER BY started_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []AgentExecution
	for rows.Next() {
		var a AgentExecution
		var parentID, phase sql.NullString
		var startedAt, finishedAt sql.NullInt64

		err := rows.Scan(&a.ID, &a.SessionID, &parentID, &a.Label, &phase, &a.Status,
			&startedAt, &finishedAt, &a.TokenCount, &a.ErrorMessage)
		if err != nil {
			return nil, err
		}

		if parentID.Valid {
			a.ParentID = &parentID.String
		}
		if phase.Valid {
			a.Phase = &phase.String
		}
		if startedAt.Valid {
			t := time.UnixMilli(startedAt.Int64)
			a.StartedAt = &t
		}
		if finishedAt.Valid {
			t := time.UnixMilli(finishedAt.Int64)
			a.FinishedAt = &t
		}

		agents = append(agents, a)
	}

	return agents, rows.Err()
}

// CreateWorkflowRun creates a new workflow run
func (s *Store) CreateWorkflowRun(ctx context.Context, wf WorkflowRun) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_runs (id, session_id, name, status, current_phase,
				phase_count, agent_count, total_tokens, started_at, finished_at, error_message)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wf.ID, wf.SessionID, wf.Name, wf.Status, wf.CurrentPhase,
			wf.PhaseCount, wf.AgentCount, wf.TotalTokens,
			wf.StartedAt.UnixMilli(), timeToMilli(wf.FinishedAt), wf.ErrorMessage)
		return err
	})
}

// UpdateWorkflowRun updates workflow run status
func (s *Store) UpdateWorkflowRun(ctx context.Context, id, status string, currentPhase *string, agentCount, totalTokens int, finishedAt *time.Time, errorMsg string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE workflow_runs
			SET status = ?, current_phase = ?, agent_count = ?, total_tokens = ?,
				finished_at = ?, error_message = ?
			WHERE id = ?`,
			status, currentPhase, agentCount, totalTokens,
			timeToMilli(finishedAt), errorMsg, id)
		return err
	})
}

// GetWorkflowRuns retrieves all workflow runs for a session
func (s *Store) GetWorkflowRuns(ctx context.Context, sessionID string) ([]WorkflowRun, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT id, session_id, name, status, current_phase, phase_count,
			agent_count, total_tokens, started_at, finished_at, error_message
		FROM workflow_runs
		WHERE session_id = ?
		ORDER BY started_at DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []WorkflowRun
	for rows.Next() {
		var wf WorkflowRun
		var currentPhase sql.NullString
		var startedAt, finishedAt sql.NullInt64

		err := rows.Scan(&wf.ID, &wf.SessionID, &wf.Name, &wf.Status, &currentPhase,
			&wf.PhaseCount, &wf.AgentCount, &wf.TotalTokens,
			&startedAt, &finishedAt, &wf.ErrorMessage)
		if err != nil {
			return nil, err
		}

		if currentPhase.Valid {
			wf.CurrentPhase = &currentPhase.String
		}
		if startedAt.Valid {
			wf.StartedAt = time.UnixMilli(startedAt.Int64)
		}
		if finishedAt.Valid {
			t := time.UnixMilli(finishedAt.Int64)
			wf.FinishedAt = &t
		}

		workflows = append(workflows, wf)
	}

	return workflows, rows.Err()
}
