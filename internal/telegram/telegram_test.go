package telegram

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HoaViet-Tech/factory/internal/progress"
)

const testToken = "123456:super-secret-bot-token"

// newTestClient points a client at a stub Bot API.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	c := New(testToken, "-1001234")
	c.BaseURL = ts.URL
	return c
}

// respond writes one Bot API envelope.
func respond(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func TestSendPostsTheChecklistAndReturnsTheMessageID(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":4242}}`)
	})

	id, err := c.Send("Factory: owner/repo#1")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "4242" {
		t.Errorf("message id = %q, want 4242", id)
	}
	if want := "/bot" + testToken + "/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotBody["chat_id"] != "-1001234" {
		t.Errorf("chat_id = %v, want -1001234", gotBody["chat_id"])
	}
	if gotBody["text"] != "Factory: owner/repo#1" {
		t.Errorf("text = %v", gotBody["text"])
	}
}

func TestEditTargetsTheExistingMessage(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":4242}}`)
	})

	if err := c.Edit("4242", "Factory: owner/repo#1\n[x] ticket detected"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/editMessageText") {
		t.Errorf("path = %q, want an editMessageText call", gotPath)
	}
	if gotBody["message_id"] != float64(4242) {
		t.Errorf("message_id = %v, want 4242", gotBody["message_id"])
	}
}

// Telegram rejects an edit that would change nothing. The checklist in the chat
// already says the right thing, so that rejection is not a failure.
func TestEditTreatsAnUnmodifiedMessageAsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
	})

	if err := c.Edit("4242", "unchanged"); err != nil {
		t.Errorf("edit = %v, want nil", err)
	}
}

// A deleted message can never be edited again, so the caller has to be told to
// send a new one instead of retrying forever.
func TestEditReportsADeletedMessageAsGone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
	})

	err := c.Edit("4242", "text")
	if !errors.Is(err, progress.ErrMessageGone) {
		t.Errorf("edit = %v, want it to wrap ErrMessageGone", err)
	}
}

func TestAPIErrorsSurfaceTheDescription(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	})

	_, err := c.Send("text")
	if err == nil {
		t.Fatal("send succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error = %v, want it to mention the API description", err)
	}
	if errors.Is(err, progress.ErrMessageGone) {
		t.Errorf("error = %v, want it not to claim the message is gone", err)
	}
}

// The token sits in the request path, and net/http puts the whole URL into the
// error it returns. Logging that would print the credential.
func TestTransportErrorsDoNotLeakTheToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	ts.Close() // nothing is listening any more, so the request cannot connect

	c := New(testToken, "-1001234")
	c.BaseURL = addr

	_, err := c.Send("text")
	if err == nil {
		t.Fatal("send succeeded against a closed server, want an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaked the bot token: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Run("unset means progress updates are simply off", func(t *testing.T) {
		t.Setenv(EnvToken, "")
		t.Setenv(EnvChat, "")
		c, err := FromEnv()
		if err != nil || c != nil {
			t.Errorf("FromEnv() = %v, %v; want nil, nil", c, err)
		}
	})

	t.Run("half-configured is an error, not a silent no-op", func(t *testing.T) {
		t.Setenv(EnvToken, testToken)
		t.Setenv(EnvChat, "")
		if _, err := FromEnv(); err == nil {
			t.Error("FromEnv() succeeded with no chat id, want an error")
		}
	})

	t.Run("both set builds a client", func(t *testing.T) {
		t.Setenv(EnvToken, testToken)
		t.Setenv(EnvChat, "-1001234")
		c, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if c.Channel() != "telegram:-1001234" {
			t.Errorf("channel = %q, want telegram:-1001234", c.Channel())
		}
		if strings.Contains(c.Channel(), testToken) {
			t.Errorf("channel leaked the bot token: %q", c.Channel())
		}
	})
}
