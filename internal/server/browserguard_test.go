package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/store"
)

// newRawTestServer returns a bare httptest server, for tests that need to send
// requests the real client would never send.
func newRawTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ts := httptest.NewServer(New(Config{Store: st, DefaultLease: time.Minute}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

const taskBody = `{"repo_owner":"local","repo_name":"demo","title":"csrf","prompt":"x"}`

// post sends a raw request and returns the status code.
func post(t *testing.T, ts *httptest.Server, path, contentType, origin, body string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestFormPostFromAWebPageIsRefused covers the exact shape a malicious page can
// send without the browser asking permission first: a "simple request" with a
// non-JSON content type.
func TestFormPostFromAWebPageIsRefused(t *testing.T) {
	ts := newRawTestServer(t)

	for _, contentType := range []string{
		"text/plain",
		"text/plain;charset=UTF-8",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"", // no content type at all
	} {
		got := post(t, ts, "/tasks", contentType, "", taskBody)
		if got != http.StatusUnsupportedMediaType {
			t.Errorf("POST with Content-Type %q returned %d, want %d",
				contentType, got, http.StatusUnsupportedMediaType)
		}
	}

	// Nothing was created by any of them.
	tasks, err := listTaskCount(ts)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if tasks != 0 {
		t.Errorf("%d tasks were created by refused requests, want 0", tasks)
	}
}

func TestCrossOriginRequestIsRefused(t *testing.T) {
	ts := newRawTestServer(t)

	got := post(t, ts, "/tasks", "application/json", "https://evil.example", taskBody)
	if got != http.StatusForbidden {
		t.Errorf("cross-origin POST returned %d, want %d", got, http.StatusForbidden)
	}

	// A first-party origin (a future dashboard served from the same host) is
	// still allowed.
	sameOriginURL := strings.TrimSuffix(ts.URL, "/")
	if got := post(t, ts, "/tasks", "application/json", sameOriginURL, taskBody); got != http.StatusCreated {
		t.Errorf("same-origin POST returned %d, want %d", got, http.StatusCreated)
	}
}

// A legitimate JSON request with no Origin — what the CLI and worker send —
// must be unaffected.
func TestNormalJSONRequestIsAllowed(t *testing.T) {
	ts := newRawTestServer(t)

	for _, contentType := range []string{"application/json", "application/json; charset=utf-8"} {
		if got := post(t, ts, "/tasks", contentType, "", taskBody); got != http.StatusCreated {
			t.Errorf("POST with Content-Type %q returned %d, want %d",
				contentType, got, http.StatusCreated)
		}
	}
}

// Bodyless state-changing routes are guarded too: a page could otherwise
// cancel tasks or trigger polls.
func TestBodylessPostsAreGuarded(t *testing.T) {
	ts := newRawTestServer(t)

	if got := post(t, ts, "/github/poll", "", "", ""); got != http.StatusUnsupportedMediaType {
		t.Errorf("bodyless poll without a content type returned %d, want %d",
			got, http.StatusUnsupportedMediaType)
	}
	if got := post(t, ts, "/tasks/whatever/cancel", "text/plain", "", ""); got != http.StatusUnsupportedMediaType {
		t.Errorf("bodyless cancel with text/plain returned %d, want %d",
			got, http.StatusUnsupportedMediaType)
	}
}

// GET is left alone: it changes nothing, and keeping it browsable is useful.
func TestGetIsNotGuarded(t *testing.T) {
	ts := newRawTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/tasks", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET returned %d, want 200", resp.StatusCode)
	}
}

// listTaskCount counts tasks through the API.
func listTaskCount(ts *httptest.Server) (int, error) {
	resp, err := ts.Client().Get(ts.URL + "/tasks")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var tasks []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return 0, err
	}
	return len(tasks), nil
}
