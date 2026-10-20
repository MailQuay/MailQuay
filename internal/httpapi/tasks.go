package httpapi

import (
	"net/http"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListTasks(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	tasks := make([]oapi.Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, mapTask(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) DismissTask(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	taskID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid task id.")
		return
	}
	if _, err := s.store.GetTask(r.Context(), taskID); err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Task not found.")
		return
	}
	if err := s.store.DismissTask(r.Context(), taskID); err != nil {
		s.internal(w, r, err)
		return
	}
	s.hub.Publish("task.removed", map[string]any{"taskId": taskID.String()})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) RetryTask(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	taskID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid task id.")
		return
	}
	task, err := s.store.GetTask(r.Context(), taskID)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Task not found.")
		return
	}
	if task.Status != "failed" {
		s.error(w, r, http.StatusBadRequest, "not_failed", "Only failed tasks can be retried.")
		return
	}
	switch task.Type {
	case "outbox_send":
		rows, err := s.compose.ListOutbox(r.Context())
		if err != nil {
			s.internal(w, r, err)
			return
		}
		for _, row := range rows {
			if row.Status != "failed" && row.Status != "pending" {
				continue
			}
			if task.AccountID != nil && row.AccountID != *task.AccountID {
				continue
			}
			_ = s.compose.Retry(r.Context(), row.ID)
		}
	case "account_purge":
		if task.AccountID != nil {
			if _, err := s.accounts.Get(r.Context(), *task.AccountID); err == nil {
				_ = s.accounts.Delete(r.Context(), *task.AccountID)
			}
		}
	case "search_index":
		// Search jobs retry automatically; resetting the failed row is enough.
	default:
		if task.AccountID != nil {
			s.sync.TriggerAccount(*task.AccountID)
		}
	}
	_ = s.store.DismissTask(r.Context(), taskID)
	s.hub.Publish("task.removed", map[string]any{"taskId": taskID.String()})
	w.WriteHeader(http.StatusAccepted)
}
