package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type approvalRequestBody struct {
	Action  string         `json:"action"`
	Summary string         `json:"summary"`
	Payload map[string]any `json:"payload"`
}

func createApprovalHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		var req approvalRequestBody
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		action := strings.TrimSpace(req.Action)
		summary := strings.TrimSpace(req.Summary)
		if action == "" || summary == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action and summary are required"})
			return
		}
		if len(summary) > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "summary too long"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		if err := agentCan(ctx, pool, agent.ID, permApprovals, false, true); err != nil {
			if errors.Is(err, errForbidden) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied", "code": "forbidden"})
				return
			}
			log.Printf("create approval permission check failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		payload, _ := json.Marshal(req.Payload)
		if payload == nil {
			payload = []byte("{}")
		}

		var id string
		err = pool.QueryRow(ctx, `
			insert into public.approval_requests (
				subnet_id, from_agent_id, action_key, summary, payload, status
			) values ($1::uuid, $2::uuid, $3, $4, $5::jsonb, 'pending')
			returning id::text
		`, agent.SubnetID, agent.ID, action, summary, string(payload)).Scan(&id)
		if err != nil {
			log.Printf("create approval failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		logAgentEvent(ctx, pool, agent.ID, agent.SubnetID, "approval_requested", summary)

		writeJSON(w, http.StatusOK, map[string]any{
			"id":     id,
			"status": "pending",
			"task": map[string]any{
				"id":    "task_" + id,
				"state": "input-required",
				"kind":  "approval",
				"status": map[string]any{
					"state": "input-required",
					"message": map[string]any{
						"role":  "agent",
						"parts": []map[string]any{{"kind": "text", "text": summary}},
					},
				},
			},
		})
	}
}

func listApprovalsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approvals failed"})
			return
		}

		if err := agentCan(ctx, pool, agent.ID, permApprovals, true, false); err != nil {
			if errors.Is(err, errForbidden) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied", "code": "forbidden"})
				return
			}
			log.Printf("list approvals permission check failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approvals failed"})
			return
		}

		status := strings.TrimSpace(r.URL.Query().Get("status"))
		if status == "" {
			status = "pending"
		}

		rows, err := pool.Query(ctx, `
			select id::text, action_key, summary, status, created_at, resolved_at
			from public.approval_requests
			where from_agent_id = $1::uuid
			  and ($2 = '' or status = $2)
			order by created_at desc
			limit 50
		`, agent.ID, status)
		if err != nil {
			log.Printf("list approvals failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approvals failed"})
			return
		}
		defer rows.Close()

		type item struct {
			ID         string  `json:"id"`
			Action     string  `json:"action"`
			Summary    string  `json:"summary"`
			Status     string  `json:"status"`
			CreatedAt  string  `json:"created_at"`
			ResolvedAt *string `json:"resolved_at,omitempty"`
		}
		out := make([]item, 0)
		for rows.Next() {
			var it item
			var created time.Time
			var resolved *time.Time
			if err := rows.Scan(&it.ID, &it.Action, &it.Summary, &it.Status, &created, &resolved); err != nil {
				continue
			}
			it.CreatedAt = created.UTC().Format(time.RFC3339)
			if resolved != nil {
				s := resolved.UTC().Format(time.RFC3339)
				it.ResolvedAt = &s
			}
			out = append(out, it)
		}

		writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
	}
}

func getApprovalHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent_key required"})
			return
		}
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		agent, err := resolveAgentKey(ctx, pool, key)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent_key"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		if err := agentCan(ctx, pool, agent.ID, permApprovals, true, false); err != nil {
			if errors.Is(err, errForbidden) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied", "code": "forbidden"})
				return
			}
			log.Printf("get approval permission check failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		var action, summary, status string
		var created time.Time
		var resolved *time.Time
		err = pool.QueryRow(ctx, `
			select action_key, summary, status, created_at, resolved_at
			from public.approval_requests
			where id = $1::uuid and from_agent_id = $2::uuid
		`, id, agent.ID).Scan(&action, &summary, &status, &created, &resolved)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "approval failed"})
			return
		}

		resp := map[string]any{
			"id":         id,
			"action":     action,
			"summary":    summary,
			"status":     status,
			"created_at": created.UTC().Format(time.RFC3339),
		}
		taskState := "input-required"
		if status == "approved" {
			taskState = "completed"
		} else if status == "rejected" {
			taskState = "canceled"
		}
		resp["task"] = map[string]any{
			"id":    "task_" + id,
			"state": taskState,
			"kind":  "approval",
		}
		if resolved != nil {
			resp["resolved_at"] = resolved.UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
