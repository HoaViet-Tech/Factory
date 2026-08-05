package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"time":    time.Now().UTC(),
		"dry_run": s.cfg.DryRun,
	})
}

// ---------- tasks ----------

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTaskRequest
	if !decode(w, r, &req) {
		return
	}
	task, err := s.store.CreateTask(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}

	tasks, err := s.store.ListTasks(store.TaskFilter{
		Status: q.Get("status"),
		Kind:   q.Get("kind"),
		Repo:   q.Get("repo"),
		Limit:  limit,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.GetTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.CancelTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	events, err := s.store.ListTaskEvents(r.PathValue("id"), limit)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// ---------- worker protocol ----------

func (s *Server) handleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterWorkerRequest
	if !decode(w, r, &req) {
		return
	}
	worker, err := s.store.RegisterWorker(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req api.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	worker, err := s.store.Heartbeat(req.WorkerID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.store.ListWorkers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workers)
}

// handleClaimTask hands out the oldest queued task, or 204 when the queue is
// empty. Workers poll this endpoint.
func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	var req api.ClaimRequest
	if !decode(w, r, &req) {
		return
	}
	if req.WorkerID == "" {
		writeErr(w, http.StatusBadRequest, "worker_id is required")
		return
	}
	// A worker must be registered before it can claim: this keeps the worker
	// registry honest and gives a clear error when someone forgets.
	if _, err := s.store.GetWorker(req.WorkerID); err != nil {
		writeStoreErr(w, err)
		return
	}

	lease := s.cfg.DefaultLease
	if req.LeaseSeconds > 0 {
		lease = time.Duration(req.LeaseSeconds) * time.Second
	}

	task, token, err := s.store.ClaimTask(req.WorkerID, lease)
	if err != nil {
		// An empty queue is normal, not an error.
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Refresh the heartbeat: claiming proves the worker is alive.
	_, _ = s.store.Heartbeat(req.WorkerID)

	writeJSON(w, http.StatusOK, api.ClaimResponse{Task: task, LeaseToken: token})
}

func (s *Server) handleAppendEvent(w http.ResponseWriter, r *http.Request) {
	var req api.AppendEventRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LeaseToken == "" {
		writeErr(w, http.StatusBadRequest, "lease_token is required")
		return
	}
	if err := s.store.AppendTaskEventWithLease(r.PathValue("id"), req.LeaseToken, req.Type, req.Message); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRenewLease extends the lease of a task a worker is still running.
func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	var req api.RenewLeaseRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LeaseToken == "" {
		writeErr(w, http.StatusBadRequest, "lease_token is required")
		return
	}

	lease := s.cfg.DefaultLease
	if req.LeaseSeconds > 0 {
		lease = time.Duration(req.LeaseSeconds) * time.Second
	}

	task, err := s.store.RenewLease(r.PathValue("id"), req.LeaseToken, lease)
	if err != nil {
		// A 409 here tells the worker its lease is gone and it should stop
		// working, rather than racing whoever picked the task up next.
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	var req api.CompleteTaskRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LeaseToken == "" {
		writeErr(w, http.StatusBadRequest, "lease_token is required")
		return
	}
	if !api.IsTerminal(req.Status) {
		writeErr(w, http.StatusBadRequest, "status must be one of succeeded, failed, cancelled, lost")
		return
	}

	task, err := s.store.CompleteTask(r.PathValue("id"), req.LeaseToken, req.Status, req.Message)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// ---------- repositories ----------

func (s *Server) handleAddRepository(w http.ResponseWriter, r *http.Request) {
	var req api.CreateRepositoryRequest
	if !decode(w, r, &req) {
		return
	}
	repo, err := s.store.AddRepository(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) handleListRepositories(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepositories(r.URL.Query().Get("enabled") == "true")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

// ---------- github ----------

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHub == nil {
		writeErr(w, http.StatusPreconditionFailed,
			"GitHub polling is disabled: start the server with a working `gh` CLI, or use --github-dry-run")
		return
	}
	resp, err := s.poller.PollOnce()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
