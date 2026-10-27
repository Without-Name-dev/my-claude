package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleCreateBranch creates a new conversation branch from a message.
func (a *App) handleCreateBranch(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	var body struct {
		ItemID string `json:"item_id"`
		Name   string `json:"name"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	if body.ItemID == "" {
		httpx.Fail(w, httpx.BadRequest("item_id is required"))
		return
	}

	// Fetch the branch point item
	item, err := a.db.TimelineItemByID(r.Context(), a.db.Read(), session.ID, body.ItemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("timeline item not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}

	var branchID string
	var branch store.ConversationBranch

	err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// Get current active branch (if any) to set as parent
		parentBranchID := session.ActiveBranchID

		// Create the branch
		branch = store.ConversationBranch{
			SessionID:           session.ID,
			ParentBranchID:      parentBranchID,
			BranchPointItemID:   body.ItemID,
			BranchPointOrderSeq: item.OrderSeq,
			Name:                body.Name,
		}
		branch.BranchID = store.NewID("br")
		branchID = branch.BranchID

		if err := a.db.CreateBranch(ctx, tx, branch); err != nil {
			return err
		}

		// Copy timeline items up to and including the branch point
		if err := a.db.CopyTimelineItemsToBranch(ctx, tx, session.ID, branchID, item.OrderSeq); err != nil {
			return err
		}

		// Set as active branch
		if err := a.db.SetActiveBranch(ctx, tx, session.ID, branchID); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		httpx.Fail(w, err)
		return
	}

	a.publishBranchEvent(r.Context(), session.ID, "branch_created", branchID)

	httpx.OK(w, map[string]any{
		"branch_id": branchID,
		"branch":    serializeBranch(branch),
	})
}

// handleListBranches lists all branches for a session.
func (a *App) handleListBranches(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	branches, err := a.db.ListBranches(r.Context(), a.db.Read(), session.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	result := make([]map[string]any, len(branches))
	for i, b := range branches {
		result[i] = serializeBranch(b)
	}

	httpx.OK(w, map[string]any{
		"branches": result,
	})
}

// handleSwitchBranch switches the active branch.
func (a *App) handleSwitchBranch(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	branchID := r.PathValue("branch_id")

	// Validate branch exists
	if branchID != "" && branchID != "main" {
		_, err := a.db.GetBranch(r.Context(), a.db.Read(), session.ID, branchID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.Fail(w, httpx.NotFound("branch not found"))
				return
			}
			httpx.Fail(w, err)
			return
		}
	}

	err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		actualBranchID := branchID
		if branchID == "main" {
			actualBranchID = ""
		}
		return a.db.SetActiveBranch(ctx, tx, session.ID, actualBranchID)
	})

	if err != nil {
		httpx.Fail(w, err)
		return
	}

	a.publishBranchEvent(r.Context(), session.ID, "branch_switched", branchID)

	httpx.OK(w, map[string]any{
		"branch_id": branchID,
	})
}

// handleDeleteBranch deletes a branch.
func (a *App) handleDeleteBranch(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	branchID := r.PathValue("branch_id")
	if branchID == "" || branchID == "main" {
		httpx.Fail(w, httpx.BadRequest("cannot delete main branch"))
		return
	}

	err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// If deleting active branch, switch to main
		if session.ActiveBranchID == branchID {
			if err := a.db.SetActiveBranch(ctx, tx, session.ID, ""); err != nil {
				return err
			}
		}

		return a.db.DeleteBranch(ctx, tx, session.ID, branchID)
	})

	if err != nil {
		httpx.Fail(w, err)
		return
	}

	a.publishBranchEvent(r.Context(), session.ID, "branch_deleted", branchID)

	httpx.OK(w, map[string]any{"ok": true})
}

// handleGetBranchTimeline returns timeline items for a specific branch.
func (a *App) handleGetBranchTimeline(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	branchID := r.PathValue("branch_id")
	if branchID == "main" {
		branchID = ""
	}

	items, err := a.db.TimelineItemsForBranch(r.Context(), a.db.Read(), session.ID, branchID, 500)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	result := make([]map[string]any, len(items))
	for i, item := range items {
		var payload map[string]any
		json.Unmarshal(item.Payload, &payload)
		result[i] = map[string]any{
			"id":          item.ID,
			"type":        item.Type,
			"status":      item.Status,
			"role":        item.Role,
			"order_seq":   item.OrderSeq,
			"updated_seq": item.UpdatedSeq,
			"payload":     payload,
		}
	}

	httpx.OK(w, map[string]any{
		"items": result,
	})
}

func serializeBranch(b store.ConversationBranch) map[string]any {
	return map[string]any{
		"branch_id":             b.BranchID,
		"parent_branch_id":      b.ParentBranchID,
		"branch_point_item_id":  b.BranchPointItemID,
		"branch_point_order_seq": b.BranchPointOrderSeq,
		"name":                  b.Name,
		"created_at":            b.CreatedAt,
		"updated_at":            b.UpdatedAt,
		"last_activity_at":      b.LastActivityAt,
	}
}

func (a *App) publishBranchEvent(ctx context.Context, sessionID, eventType, branchID string) {
	frame, _ := json.Marshal(map[string]any{
		"type":       eventType,
		"session_id": sessionID,
		"branch_id":  branchID,
	})
	a.broker.Publish(sessionID, frame)
}
