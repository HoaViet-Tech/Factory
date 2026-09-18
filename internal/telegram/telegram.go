// Package telegram publishes the run checklist to a Telegram chat.
//
// It implements progress.Notifier with two Bot API calls: sendMessage for the
// first update of a run and editMessageText for every one after it, so a run
// occupies exactly one message that fills in as the work happens rather than a
// stream of near-identical notifications.
package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HoaViet-Tech/factory/internal/progress"
)

// The configuration lives in the environment, not in flags. The token is a
// credential that lets anyone post as the bot, and flag values are visible in
// `ps` output and shell history.
const (
	EnvToken = "FACTORY_TELEGRAM_BOT_TOKEN"
	EnvChat  = "FACTORY_TELEGRAM_CHAT_ID"
)

// DefaultBaseURL is the public Bot API endpoint. Tests point BaseURL at an
// httptest server instead.
const DefaultBaseURL = "https://api.telegram.org"

// Client talks to one bot and posts into one chat.
type Client struct {
	Token   string
	ChatID  string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the given bot token and chat.
func New(token, chatID string) *Client {
	return &Client{
		Token:   token,
		ChatID:  chatID,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// FromEnv builds a client from the environment.
//
// It returns (nil, nil) when neither variable is set: running the factory
// without chat updates is a normal way to run it. Setting only one of the two
// is an error, because it is always a mistake and silently ignoring it would
// leave an operator waiting for updates that can never arrive.
func FromEnv() (*Client, error) {
	token := strings.TrimSpace(os.Getenv(EnvToken))
	chat := strings.TrimSpace(os.Getenv(EnvChat))
	switch {
	case token == "" && chat == "":
		return nil, nil
	case token == "":
		return nil, fmt.Errorf("%s is set but %s is not", EnvChat, EnvToken)
	case chat == "":
		return nil, fmt.Errorf("%s is set but %s is not", EnvToken, EnvChat)
	}
	return New(token, chat), nil
}

// Channel identifies the destination chat. The token is not part of it: this
// string is persisted next to message ids, and a database row is the last place
// a credential should end up.
func (c *Client) Channel() string { return "telegram:" + c.ChatID }

// Send posts a new message and returns its id.
func (c *Client) Send(text string) (string, error) {
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	err := c.call("sendMessage", map[string]any{
		"chat_id":                  c.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}, &result)
	if err != nil {
		return "", err
	}
	if result.MessageID == 0 {
		return "", errors.New("telegram sendMessage: response carried no message id")
	}
	return strconv.FormatInt(result.MessageID, 10), nil
}

// Edit rewrites a message Send returned.
func (c *Client) Edit(externalID, text string) error {
	id, err := strconv.ParseInt(externalID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram message id %q is not a number: %w", externalID, err)
	}

	err = c.call("editMessageText", map[string]any{
		"chat_id":                  c.ChatID,
		"message_id":               id,
		"text":                     text,
		"disable_web_page_preview": true,
	}, nil)
	switch {
	case err == nil:
		return nil
	case mentions(err, "message is not modified"):
		// Telegram refuses an edit that would change nothing. The checklist in
		// the chat already says what we wanted it to say, so this is success.
		return nil
	case mentions(err, "message to edit not found", "MESSAGE_ID_INVALID", "message can't be edited"):
		return fmt.Errorf("%w: %v", progress.ErrMessageGone, err)
	default:
		return err
	}
}

// call performs one Bot API method.
func (c *Client) call(method string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL()+"/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return c.redact(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.redact(err)
	}

	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: unreadable response (%s)", method, resp.Status)
	}
	if !env.OK {
		description := env.Description
		if description == "" {
			description = resp.Status
		}
		return fmt.Errorf("telegram %s: %s", method, description)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// redact strips the bot token out of an error before it can be logged.
//
// The token sits in the request path, and net/http puts the whole URL into
// *url.Error, so one connection refused would otherwise print the credential
// into the server log. Wrapping is deliberately dropped: these errors are only
// logged, and keeping the chain would keep the token with it.
func (c *Client) redact(err error) error {
	if err == nil || c.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Token, "«token»"))
}

// mentions reports whether the Bot API description contains any of these
// phrases. Telegram has no machine-readable code for these cases, so matching
// the description is the only option available.
func mentions(err error, phrases ...string) bool {
	msg := strings.ToLower(err.Error())
	for _, p := range phrases {
		if strings.Contains(msg, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
