package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jhuancz/cronkite/internal/report"
)

// testToken and testTokenEnv are a synthetic token pair. No test may print the
// token, so every failure below names what was expected and never the error or
// the request it came from: a leak in a test log is still a leak in CI output.
const (
	testTokenEnv = "CRONKITE_TEST_TELEGRAM_TOKEN"
	testToken    = "123456789:AAH-test-token-value"
)

// telegramAgainst starts a Bot API stand-in, points a sender at it and returns
// the sender.
func telegramAgainst(t *testing.T, handler http.HandlerFunc) *Telegram {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv(testTokenEnv, testToken)
	return NewTelegram(TelegramOptions{ChatID: "42", TokenEnv: testTokenEnv, BaseURL: srv.URL})
}

func TestTelegramSendsTheMessage(t *testing.T) {
	var (
		method      string
		path        string
		contentType string
		payload     struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
	)
	sender := telegramAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})

	msg := Message{
		Status:      report.StatusPartial,
		RunID:       "run-1",
		Host:        "web-01",
		FailedTasks: []string{"logs"},
		ReportPath:  "/tmp/run-1.json",
	}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if !strings.HasSuffix(path, "/sendMessage") {
		t.Errorf("path = %q, want the sendMessage method", path)
	}
	if !strings.Contains(path, testToken) {
		t.Error("the token must travel in the Bot API path")
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("content type = %q, want application/json", contentType)
	}
	if payload.ChatID != "42" {
		t.Errorf("chat_id = %q, want the configured chat", payload.ChatID)
	}
	if payload.Text != msg.Text() {
		t.Error("the body must carry the rendered message")
	}
}

func TestTelegramClassifiesFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		mustSay string
	}{
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			body:    `{"ok":false,"description":"Internal Server Error"}`,
			mustSay: "unavailable",
		},
		{
			name:    "rate limited",
			status:  http.StatusTooManyRequests,
			body:    `{"ok":false,"description":"Too Many Requests: retry after 30"}`,
			mustSay: "rate limiting",
		},
		{
			name:    "rejected message",
			status:  http.StatusBadRequest,
			body:    `{"ok":false,"description":"Bad Request: chat not found"}`,
			mustSay: "rejected",
		},
		{
			name:    "truncated response",
			status:  http.StatusOK,
			body:    `{"ok":true`,
			mustSay: "decode",
		},
		{
			name:    "ok false with a description",
			status:  http.StatusOK,
			body:    `{"ok":false,"description":"Bad Request: chat not found"}`,
			mustSay: "refused the message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := telegramAgainst(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			err := sender.Send(context.Background(), Message{Status: report.StatusFailed, RunID: "run-1"})
			if err == nil {
				t.Fatalf("a %d response must be an error", tc.status)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("the error should mention %q", tc.mustSay)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Error("the error carries the token")
			}
		})
	}
}

// The client timeout is what keeps a messenger outage from holding a run open.
func TestTelegramTimeoutIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(testTokenEnv, testToken)

	sender := NewTelegram(TelegramOptions{
		ChatID:   "42",
		TokenEnv: testTokenEnv,
		BaseURL:  srv.URL,
		Client:   &http.Client{Timeout: 30 * time.Millisecond},
	})

	start := time.Now()
	err := sender.Send(context.Background(), Message{Status: report.StatusFailed, RunID: "run-1"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a request that outlives the client timeout must fail")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Send took %s, want it bounded by the client timeout", elapsed)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "deadline") {
		t.Error("the error should say the request timed out")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("the timeout error carries the token")
	}
}

// The token is environment-only, and a missing one is only a problem when a
// message would actually be sent.
func TestAbsentTokenIsAnErrorOnlyWhenSending(t *testing.T) {
	t.Setenv(testTokenEnv, "")
	sender := NewTelegram(TelegramOptions{ChatID: "42", TokenEnv: testTokenEnv})

	err := sender.Send(context.Background(), Message{Status: report.StatusFailed})
	if err == nil {
		t.Fatal("an empty token must fail the send")
	}
	if !strings.Contains(err.Error(), testTokenEnv) {
		t.Error("the error should name the environment variable an operator has to set")
	}
}

// The frozen decision: the token never reaches the report. The report embeds
// delivery failures as notes, so the assertion has to be on the encoded bytes
// and not on the error alone.
func TestTokenAbsentFromEncodedReport(t *testing.T) {
	sender := telegramAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"ok":false,"description":"Internal Server Error"}`)
	})
	sendErr := sender.Send(context.Background(), Message{Status: report.StatusFailed, RunID: "run-1"})
	if sendErr == nil {
		t.Fatal("a 500 must fail the send")
	}

	rep := report.New(report.ModeApply, "cronkite.conf")
	rep.Status = report.StatusFailed
	rep.Notes = append(rep.Notes, "notify: "+sendErr.Error())

	var encoded bytes.Buffer
	if err := rep.Write(&encoded); err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if !bytes.Contains(encoded.Bytes(), []byte("notify:")) {
		t.Error("the delivery failure should appear as a note")
	}
	if bytes.Contains(encoded.Bytes(), []byte(testToken)) {
		t.Error("the encoded report carries the token")
	}
}

// A body over the Bot API limit is rejected outright, so truncating is the
// difference between a shortened alert and no alert at all.
func TestClampTextStopsBeforeTheAPILimit(t *testing.T) {
	long := strings.Repeat("a failed task name; ", 400)
	if utf8.RuneCountInString(long) <= telegramMaxText {
		t.Fatalf("the fixture must exceed the limit, it is %d runes", utf8.RuneCountInString(long))
	}
	got := clampText(long)
	if utf8.RuneCountInString(got) > telegramMaxText {
		t.Fatalf("clamped text is %d runes, above the %d limit", utf8.RuneCountInString(got), telegramMaxText)
	}

	// The limit is characters, not bytes: a message of wide characters that fits
	// the API must not be truncated just because its UTF-8 form is long.
	cjk := strings.Repeat("任", 3000)
	if utf8.RuneCountInString(cjk) <= telegramMaxText/2 {
		t.Fatalf("the fixture should be under the character limit but wide: %d runes, %d bytes",
			utf8.RuneCountInString(cjk), len(cjk))
	}
	if got := clampText(cjk); got != cjk {
		t.Fatalf("a message of %d runes must not be truncated, got %d runes",
			utf8.RuneCountInString(cjk), utf8.RuneCountInString(got))
	}
	if got := clampText(strings.Repeat("任", telegramMaxText+100)); utf8.RuneCountInString(got) > telegramMaxText {
		t.Fatalf("a wide message over the limit must still be clamped, got %d runes", utf8.RuneCountInString(got))
	}
	if !strings.HasSuffix(got, "(truncated)") || !strings.HasPrefix(got, "a failed task name;") {
		t.Fatalf("the clamp must keep the beginning and say it cut: %q", got[len(got)-40:])
	}
	// A message inside the limit is untouched.
	if short := "cronkite: ok"; clampText(short) != short {
		t.Fatal("a short message must not be rewritten")
	}
}
