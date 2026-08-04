// Package client is the HTTP client for the control plane API. Both the
// worker and the CLI talk to the server exclusively through this package, so
// there is one place where the wire protocol lives.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// ErrNoTask is returned by Claim when the queue is empty.
var ErrNoTask = errors.New("no task available")

// Client talks to one control plane server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the given base URL (for example
// "http://127.0.0.1:7337").
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// do performs a request and decodes the JSON response into out (which may be
// nil). It returns the HTTP status so callers can distinguish 204 from 200.
func (c *Client) do(method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w (is the server running?)", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}

	if resp.StatusCode >= 400 {
		var e api.ErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return resp.StatusCode, fmt.Errorf("%s %s: %s (%s)", method, path, e.Error, resp.Status)
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}

	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response from %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// Health checks that the server is up.
func (c *Client) Health() (map[string]any, error) {
	var out map[string]any
	_, err := c.do(http.MethodGet, "/healthz", nil, &out)
	return out, err
}

// ---------- tasks ----------

// CreateTask enqueues a task.
func (c *Client) CreateTask(req api.CreateTaskRequest) (api.Task, error) {
	var out api.Task
	_, err := c.do(http.MethodPost, "/tasks", req, &out)
	return out, err
}

// ListTasks lists tasks, optionally filtered.
func (c *Client) ListTasks(status, kind, repo string, limit int) ([]api.Task, error) {
	q := make([]string, 0, 4)
	if status != "" {
		q = append(q, "status="+status)
	}
	if kind != "" {
		q = append(q, "kind="+kind)
	}
	if repo != "" {
		q = append(q, "repo="+repo)
	}
	if limit > 0 {
		q = append(q, "limit="+strconv.Itoa(limit))
	}
	path := "/tasks"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}

	var out []api.Task
	_, err := c.do(http.MethodGet, path, nil, &out)
	return out, err
}

// GetTask fetches one task.
func (c *Client) GetTask(id string) (api.Task, error) {
	var out api.Task
	_, err := c.do(http.MethodGet, "/tasks/"+id, nil, &out)
	return out, err
}

// CancelTask cancels a task.
func (c *Client) CancelTask(id string) (api.Task, error) {
	var out api.Task
	_, err := c.do(http.MethodPost, "/tasks/"+id+"/cancel", nil, &out)
	return out, err
}

// ListEvents fetches a task's log.
func (c *Client) ListEvents(id string) ([]api.TaskEvent, error) {
	var out []api.TaskEvent
	_, err := c.do(http.MethodGet, "/tasks/"+id+"/events", nil, &out)
	return out, err
}

// ---------- worker protocol ----------

// RegisterWorker registers (or re-registers) this worker.
func (c *Client) RegisterWorker(req api.RegisterWorkerRequest) (api.Worker, error) {
	var out api.Worker
	_, err := c.do(http.MethodPost, "/workers/register", req, &out)
	return out, err
}

// Heartbeat tells the server this worker is still alive.
func (c *Client) Heartbeat(workerID string) error {
	_, err := c.do(http.MethodPost, "/workers/heartbeat", api.HeartbeatRequest{WorkerID: workerID}, nil)
	return err
}

// Claim attempts to claim one task. It returns ErrNoTask when the queue is
// empty, which is the normal case and not a failure.
func (c *Client) Claim(workerID string, leaseSeconds int) (api.Task, string, error) {
	var out api.ClaimResponse
	code, err := c.do(http.MethodPost, "/tasks/claim",
		api.ClaimRequest{WorkerID: workerID, LeaseSeconds: leaseSeconds}, &out)
	if err != nil {
		return api.Task{}, "", err
	}
	if code == http.StatusNoContent {
		return api.Task{}, "", ErrNoTask
	}
	return out.Task, out.LeaseToken, nil
}

// AppendEvent streams one log line back to the server.
func (c *Client) AppendEvent(taskID, leaseToken, evType, message string) error {
	_, err := c.do(http.MethodPost, "/tasks/"+taskID+"/events",
		api.AppendEventRequest{LeaseToken: leaseToken, Type: evType, Message: message}, nil)
	return err
}

// Complete finishes a task.
func (c *Client) Complete(taskID, leaseToken, status, message string) (api.Task, error) {
	var out api.Task
	_, err := c.do(http.MethodPost, "/tasks/"+taskID+"/complete",
		api.CompleteTaskRequest{LeaseToken: leaseToken, Status: status, Message: message}, &out)
	return out, err
}

// ---------- repositories & github ----------

// AddRepository registers a repository with the control plane.
func (c *Client) AddRepository(req api.CreateRepositoryRequest) (api.Repository, error) {
	var out api.Repository
	_, err := c.do(http.MethodPost, "/repositories", req, &out)
	return out, err
}

// ListRepositories lists registered repositories.
func (c *Client) ListRepositories() ([]api.Repository, error) {
	var out []api.Repository
	_, err := c.do(http.MethodGet, "/repositories", nil, &out)
	return out, err
}

// Poll asks the server to run one GitHub polling pass now.
func (c *Client) Poll() (api.PollResponse, error) {
	var out api.PollResponse
	_, err := c.do(http.MethodPost, "/github/poll", nil, &out)
	return out, err
}
