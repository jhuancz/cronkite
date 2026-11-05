package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultBaseURL is the Bot API endpoint. Production traffic is HTTPS only,
// because the request path carries the token; tests point BaseURL at an
// httptest.Server instead of adding a knob to the configuration file.
const DefaultBaseURL = "https://api.telegram.org"

// DefaultTimeout bounds one delivery. It is short on purpose: an alert that
// arrives after the operator has already seen the next run is noise, and a run
// must never wait on a messenger.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes caps how much of an API response is read, so a broken or
// hostile endpoint cannot make a scheduled run allocate without bound.
const maxResponseBytes = 64 << 10

// TelegramOptions configures the Bot API sender.
type TelegramOptions struct {
	// ChatID is the destination chat or channel.
	ChatID string
	// TokenEnv names the environment variable holding the bot token. The token is
	// never a configuration value, so a configuration file can be pasted into a
	// ticket without leaking it.
	TokenEnv string
	// BaseURL overrides the API endpoint. Empty means DefaultBaseURL.
	BaseURL string
	// Client is the HTTP client. Nil means a client with DefaultTimeout.
	Client *http.Client
	// LookupEnv resolves the token. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Telegram delivers messages through the Bot API.
//
// No SDK and no new dependency: the Bot API call this needs is one HTTPS POST
// with a JSON body, and a client library would add a supply chain to a program
// whose whole job is to be boring at 03:00.
type Telegram struct {
	chatID    string
	tokenEnv  string
	baseURL   string
	client    *http.Client
	lookupEnv func(string) (string, bool)
}

// NewTelegram returns a sender for the configured chat. Nothing is read from the
// environment here: an absent token is an error only when a message would
// actually be sent.
func NewTelegram(opts TelegramOptions) *Telegram {
	return &Telegram{
		chatID:    opts.ChatID,
		tokenEnv:  opts.TokenEnv,
		baseURL:   opts.BaseURL,
		client:    opts.Client,
		lookupEnv: opts.LookupEnv,
	}
}

// sendMessage is the Bot API request body.
type sendMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
	// The message links to the report path; rendering it as a preview card would
	// show a local path to every member of the chat as a broken page.
	DisableWebPagePreview bool `json:"disable_web_page_preview"`
}

// apiResponse is the part of the Bot API answer cronkite uses: whether it
// worked, and why not.
type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Send delivers one message.
//
// Every error it returns has the token removed. The token travels in the request
// path and the HTTP client quotes the URL in transport errors, so an unredacted
// error would put the bot token into the report note and into anything that
// displays it.
func (t *Telegram) Send(ctx context.Context, m Message) error {
	if t.chatID == "" {
		return errors.New("notify: telegram.chat_id is not set")
	}
	if t.tokenEnv == "" {
		return errors.New("notify: telegram.token_env is not set")
	}
	lookup := t.lookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	token, ok := lookup(t.tokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		return fmt.Errorf("notify: telegram token is not set: the environment variable %s is empty", t.tokenEnv)
	}

	payload, err := json.Marshal(sendMessage{
		ChatID:                t.chatID,
		Text:                  clampText(m.Text()),
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("notify: encode telegram message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint(token), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: build telegram request: %w", redact(err, token))
	}
	req.Header.Set("Content-Type", "application/json")

	client := t.client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: send telegram message: %w", redact(err, token))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("notify: read telegram response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return redact(fmt.Errorf("notify: %s", describe(resp.StatusCode, body)), token)
	}

	var api apiResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return fmt.Errorf("notify: decode telegram response: %w", redact(err, token))
	}
	if !api.OK {
		return redact(fmt.Errorf("notify: telegram refused the message: %s", describeAPI(api)), token)
	}
	return nil
}

// endpoint builds the Bot API URL for one method.
func (t *Telegram) endpoint(token string) string {
	base := t.baseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimSuffix(base, "/") + "/bot" + token + "/sendMessage"
}

// describe turns a non-200 answer into a message that names the class of problem,
// because "failed" alone tells an operator nothing about whether to rotate the
// token, fix the chat id, or simply wait.
func describe(status int, body []byte) string {
	var api apiResponse
	_ = json.Unmarshal(body, &api)
	detail := ""
	if api.Description != "" {
		detail = ": " + api.Description
	}
	switch {
	case status == http.StatusTooManyRequests:
		return "telegram is rate limiting this bot (HTTP 429); the next run will try again" + detail
	case status >= 500:
		return fmt.Sprintf("telegram is unavailable (HTTP %d)%s; the maintenance run was not affected", status, detail)
	case status >= 400:
		return fmt.Sprintf("telegram rejected the message (HTTP %d)%s; check notify.telegram.chat_id and the bot token", status, detail)
	default:
		return fmt.Sprintf("telegram answered with an unexpected status (HTTP %d)%s", status, detail)
	}
}

// describeAPI turns an "ok":false answer into the same kind of message.
func describeAPI(api apiResponse) string {
	if api.Description != "" {
		return api.Description
	}
	return "the API answered ok=false without a description"
}

// telegramMaxText is the Bot API limit for one message body. A longer body is
// rejected outright, so a run with many failed tasks would silence the very alert
// it was trying to send: truncating keeps the beginning - the status and the first
// failures - which is what a reader acts on.
const telegramMaxText = 4096

// clampText shortens a message to the API limit and says that it did.
//
// The limit is counted in characters, not bytes: counting bytes would truncate a
// message that fits perfectly well - three thousand CJK characters are nine
// thousand bytes and one message the API accepts - so the cut is made on runes.
func clampText(s string) string {
	if utf8.RuneCountInString(s) <= telegramMaxText {
		return s
	}
	marker := "\n... (truncated)"
	budget := telegramMaxText - utf8.RuneCountInString(marker)

	var b strings.Builder
	b.Grow(len(s))
	written := 0
	for _, r := range s {
		if written >= budget {
			break
		}
		b.WriteRune(r)
		written++
	}
	b.WriteString(marker)
	return b.String()
}

// secretError hides a token inside the message of the error it wraps. Unwrap is
// kept so callers can still test for context cancellation or a timeout; only the
// rendered message is sanitized.
type secretError struct {
	err   error
	token string
}

func (e secretError) Error() string {
	return strings.ReplaceAll(e.err.Error(), e.token, "[redacted]")
}

func (e secretError) Unwrap() error { return e.err }

// redact returns err with every occurrence of token replaced in its message. It
// returns err unchanged when there is nothing to hide.
func redact(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return secretError{err: err, token: token}
}
