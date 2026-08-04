// Package server is the control plane: an HTTP API over the SQLite store,
// plus two background loops (lease reaping and optional GitHub polling).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/ingest"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// Config configures a Server.
type Config struct {
	Store *store.Store
	// GitHub may be nil, in which case /github/poll reports a clear error.
	GitHub ingest.IssueLister
	// DryRun is reported back to clients so they know GitHub writes are off.
	DryRun bool
	// DefaultLease is how long a claimed task stays leased to a worker.
	DefaultLease time.Duration
	// ReapInterval is how often expired leases are swept up.
	ReapInterval time.Duration
	// PollInterval, when > 0, polls GitHub automatically in the background.
	PollInterval time.Duration
	// PollLimit caps issues read per label per repository.
	PollLimit int
	Logger    *log.Logger
}

// Server holds the wired-up control plane.
type Server struct {
	cfg    Config
	store  *store.Store
	poller *ingest.Poller
	logger *log.Logger
	mux    *http.ServeMux
}

// New builds a Server and its routes.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stdout, "[server] ", log.LstdFlags)
	}
	if cfg.DefaultLease <= 0 {
		cfg.DefaultLease = 2 * time.Minute
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = 15 * time.Second
	}
	if cfg.PollLimit <= 0 {
		cfg.PollLimit = 30
	}

	s := &Server{
		cfg:    cfg,
		store:  cfg.Store,
		logger: cfg.Logger,
		poller: &ingest.Poller{
			Store:  cfg.Store,
			GitHub: cfg.GitHub,
			Limit:  cfg.PollLimit,
			DryRun: cfg.DryRun,
			Logger: cfg.Logger,
		},
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler exposes the router, which is all the tests need.
func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

func (s *Server) routes() {
	// Health
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// Tasks
	s.mux.HandleFunc("POST /tasks", s.handleCreateTask)
	s.mux.HandleFunc("GET /tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /tasks/claim", s.handleClaimTask)
	s.mux.HandleFunc("GET /tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("POST /tasks/{id}/cancel", s.handleCancelTask)
	s.mux.HandleFunc("GET /tasks/{id}/events", s.handleListEvents)
	s.mux.HandleFunc("POST /tasks/{id}/events", s.handleAppendEvent)
	s.mux.HandleFunc("POST /tasks/{id}/complete", s.handleCompleteTask)

	// Workers
	s.mux.HandleFunc("POST /workers/register", s.handleRegisterWorker)
	s.mux.HandleFunc("POST /workers/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("GET /workers", s.handleListWorkers)

	// Repositories
	s.mux.HandleFunc("POST /repositories", s.handleAddRepository)
	s.mux.HandleFunc("GET /repositories", s.handleListRepositories)

	// GitHub
	s.mux.HandleFunc("POST /github/poll", s.handlePoll)
}

// Run serves HTTP until ctx is cancelled, running the background loops
// alongside it.
func (s *Server) Run(ctx context.Context, listen string) error {
	srv := &http.Server{
		Addr:              listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go s.reapLoop(ctx)
	if s.cfg.PollInterval > 0 {
		go s.pollLoop(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Printf("listening on http://%s (dry-run=%v)", listen, s.cfg.DryRun)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.logger.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// reapLoop expires stale leases so a dead worker cannot hold a task forever.
func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reaped, err := s.store.ReapExpiredLeases()
			if err != nil {
				s.logger.Printf("reap error: %v", err)
				continue
			}
			for _, r := range reaped {
				s.logger.Printf("lease expired: task %s -> %s", r.TaskID, r.NewStatus)
			}
		}
	}
}

// pollLoop polls GitHub on an interval when one is configured.
func (s *Server) pollLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			resp, err := s.poller.PollOnce()
			if err != nil {
				s.logger.Printf("poll error: %v", err)
				continue
			}
			if resp.TasksCreated > 0 {
				s.logger.Printf("poll created %d task(s)", resp.TasksCreated)
			}
		}
	}
}

// logRequests is a tiny access log. Useful while learning: you can watch the
// worker talk to the server in real time.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Claim polling is noisy and boring; only log the interesting ones.
		if !(r.URL.Path == "/tasks/claim" && rec.status == http.StatusNoContent) {
			s.logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorResponse{Error: msg})
}

// writeStoreErr maps store sentinel errors onto HTTP status codes so every
// handler reports failures the same way.
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrInvalidLease):
		writeErr(w, http.StatusConflict, "invalid or expired lease token")
	case errors.Is(err, store.ErrNotRunning):
		writeErr(w, http.StatusConflict, "task is not running")
	case errors.Is(err, store.ErrTerminal):
		writeErr(w, http.StatusConflict, "task is already finished")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
