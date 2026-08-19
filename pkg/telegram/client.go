// Package telegram is a minimal Bot API client covering the six endpoints tg-mcp needs.
// A bot framework is deliberately avoided: it would own the getUpdates offset internally and
// fight the commit-after-store semantics of the ingest loop.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultTimeout  = 30 * time.Second
	longPollGrace   = 10 * time.Second // added on top of the poll timeout so the server replies first
	maxRetryAfter   = 60 * time.Second
	maxResponseSize = 32 << 20
	cloudFileLimit  = 20 << 20 // getFile on the cloud api server refuses anything larger
)

// ParseModeHTML selects the telegram html subset for SendMessage.
const ParseModeHTML = "HTML"

// Client talks to a Bot API server, cloud or self-hosted.
type Client struct {
	token   string
	baseURL string
	local   bool

	client       *http.Client
	streamClient *http.Client
	retryUnit    time.Duration // scale of retry_after waits, overridden in tests
}

// New creates a client for the given bot token and api base url. Set local when the api server
// runs with --local, in which case getFile returns absolute filesystem paths.
func New(token, baseURL string, local bool) *Client {
	return &Client{
		token:   token,
		baseURL: strings.TrimRight(baseURL, "/"),
		local:   local,
		client:  &http.Client{Timeout: defaultTimeout},
		// no fixed timeout: a long poll carries its own deadline and a 2 GB download would
		// otherwise be cut mid-body, Timeout covering the read of the response too
		streamClient: &http.Client{},
		retryUnit:    time.Second,
	}
}

// APIError is a Bot API error envelope.
type APIError struct {
	Method          string
	Code            int
	Description     string
	RetryAfter      int
	MigrateToChatID int64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed: %d %s", e.Method, e.Code, e.Description)
}

// IsConflict reports a 409, meaning a webhook is set or another getUpdates poller is running.
func (e *APIError) IsConflict() bool { return e.Code == http.StatusConflict }

// IsAuthFailure reports the codes the Bot API returns for a token that will never work again:
// revoked or invalid (401), the bot blocked or removed (403), and a token the api server does not
// know at all (404, the method path itself is not found). Retrying any of them is pointless.
func (e *APIError) IsAuthFailure() bool {
	switch e.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// GetMe returns the bot account, most importantly its username for mention detection.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var res User
	if err := c.call(ctx, c.client, "getMe", nil, &res); err != nil {
		return User{}, err
	}
	return res, nil
}

// DeleteWebhook drops any configured webhook so getUpdates polling can start.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, c.client, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// GetUpdates long-polls for new updates starting at offset; offset 0 means "whatever is pending".
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	payload := map[string]any{
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "edited_message"},
	}
	if offset > 0 {
		payload["offset"] = offset
	}

	ctx, cancel := context.WithTimeout(ctx, timeout+longPollGrace)
	defer cancel()

	var res []Update
	if err := c.call(ctx, c.streamClient, "getUpdates", payload, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// SendMessage posts a message and returns the message telegram created. parseMode selects how
// text is parsed: empty for none, ParseModeHTML for the telegram html subset. Replies to
// a message inherit its forum topic automatically; threadID targets a topic explicitly.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text, parseMode string, replyTo, threadID int64) (Message, error) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]any{"message_id": replyTo}
	}
	if threadID > 0 {
		payload["message_thread_id"] = threadID
	}

	var res Message
	if err := c.call(ctx, c.client, "sendMessage", payload, &res); err != nil {
		return Message{}, err
	}
	return res, nil
}

// GetFile resolves a file id to a download path (or a filesystem path in local mode).
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var res File
	if err := c.call(ctx, c.client, "getFile", map[string]any{"file_id": fileID}, &res); err != nil {
		var apiErr *APIError
		if !c.local && errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Description), "too big") {
			return File{}, fmt.Errorf("%w: the cloud bot api caps downloads at %d MB, "+
				"a self-hosted api server lifts the limit", err, cloudFileLimit>>20)
		}
		return File{}, err
	}
	return res, nil
}

// Download writes the contents of a file resolved by GetFile into dst. In local mode file_path
// is an absolute path on a volume shared with the api server; otherwise it is fetched over http.
func (c *Client) Download(ctx context.Context, filePath string, dst io.Writer) error {
	if c.local {
		return c.copyLocal(filePath, dst)
	}

	url := c.baseURL + "/file/bot" + c.token + "/" + strings.TrimPrefix(filePath, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("create download request: %w", c.sanitize(err))
	}
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %q: %w", filePath, c.sanitize(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("download %q: http %d: %s", filePath, resp.StatusCode, describe(body))
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return fmt.Errorf("write %q: %w", filePath, err)
	}
	return nil
}

// copyLocal reads the file straight off the shared volume. A relative path means the api server
// is not actually running with --local, which must fail loudly rather than half-work.
func (c *Client) copyLocal(filePath string, dst io.Writer) error {
	if !filepath.IsAbs(filePath) {
		return fmt.Errorf("local mode expects an absolute file path, got %q: is the api server running with --local?", filePath)
	}
	fh, err := os.Open(filePath) //nolint:gosec // path comes from our own api server
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer fh.Close()

	if _, err := io.Copy(dst, fh); err != nil {
		return fmt.Errorf("copy local file %q: %w", filePath, err)
	}
	return nil
}

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
}

// call performs a bot api method, honoring a 429 retry_after once before giving up.
func (c *Client) call(ctx context.Context, cl *http.Client, method string, payload map[string]any, result any) error {
	body := []byte("{}")
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return fmt.Errorf("encode %s payload: %w", method, err)
		}
	}

	for attempt := 0; ; attempt++ {
		raw, err := c.do(ctx, cl, method, body)
		var apiErr *APIError
		if attempt == 0 && errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests {
			if waitErr := c.wait(ctx, apiErr.RetryAfter); waitErr != nil {
				return waitErr
			}
			continue
		}
		if err != nil {
			return err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
}

func (c *Client) do(ctx context.Context, cl *http.Client, method string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", method, c.sanitize(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", method, c.sanitize(err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", method, c.sanitize(err))
	}

	var env response
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode %s response (http %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		apiErr := &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
		if apiErr.Code == 0 {
			apiErr.Code = resp.StatusCode
		}
		if env.Parameters != nil {
			apiErr.RetryAfter, apiErr.MigrateToChatID = env.Parameters.RetryAfter, env.Parameters.MigrateToChatID
		}
		return nil, apiErr
	}
	return env.Result, nil
}

// wait sleeps for the retry_after telegram asked for, capped, and aborts on context cancel.
func (c *Client) wait(ctx context.Context, retryAfter int) error {
	delay := min(time.Duration(retryAfter)*c.retryUnit, maxRetryAfter)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for retry_after: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (c *Client) methodURL(method string) string { return c.baseURL + "/bot" + c.token + "/" + method }

// sanitize strips the bot token from transport errors, which embed the request url.
func (c *Client) sanitize(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), c.token, "***")
	if msg == err.Error() {
		return err
	}
	return sanitized{msg: msg, err: err}
}

type sanitized struct {
	msg string
	err error
}

func (e sanitized) Error() string { return e.msg }
func (e sanitized) Unwrap() error { return e.err }

// describe turns a non-json error body into something loggable.
func describe(body []byte) string {
	var env response
	if err := json.Unmarshal(body, &env); err == nil && env.Description != "" {
		return env.Description
	}
	return strings.TrimSpace(string(body))
}
