//go:build e2e

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	e2eAuthToken = "e2e-secret"
	e2eBotToken  = "e2e-bot-token"
	e2eBotID     = 42
	e2eBotName   = "support_bot"
	e2eChatID    = -1001234567890
	e2eOtherChat = -1009876543210
	e2eFilePath  = "documents/core.dump"
)

// e2eMessage mirrors the message view the tools hand out; the server-side type is unexported.
type e2eMessage struct {
	MessageID int64  `json:"message_id"`
	Customer  string `json:"customer"`
	Label     string `json:"label"`
	Sent      string `json:"sent"`
	Sender    string `json:"sender"`
	FromBot   bool   `json:"from_bot"`
	ReplyTo   int64  `json:"reply_to"`
	Text      string `json:"text"`
	Snippet   string `json:"snippet"`
	Mention   bool   `json:"mention"`
	EditedAt  string `json:"edited_at"`
	Media     string `json:"media"`
	FileName  string `json:"file_name"`
	FileSize  int64  `json:"file_size"`
}

type e2eMessages struct {
	Messages  []e2eMessage `json:"messages"`
	Truncated bool         `json:"truncated"`
}

type e2eCustomers struct {
	Customers []struct {
		Customer string `json:"customer"`
		Groups   []struct {
			Label  string `json:"label"`
			Unread int    `json:"unread"`
		} `json:"groups"`
		Unread int `json:"unread"`
	} `json:"customers"`
}

type e2eFile struct {
	MessageID int64  `json:"message_id"`
	Customer  string `json:"customer"`
	Media     string `json:"media"`
	FileName  string `json:"file_name"`
	FileSize  int64  `json:"file_size"`
	MimeType  string `json:"mime_type"`
	Inline    bool   `json:"inline"`
	URL       string `json:"url"`
}

type e2eSendReply struct {
	Message e2eMessage `json:"message"`
	Warning string     `json:"warning"`
}

type e2eMarkHandled struct {
	Customer   string `json:"customer"`
	MarkedUpTo int64  `json:"marked_up_to"`
}

// TestE2ESmoke runs the whole binary in-process against a scripted Bot API and drives every tool
// through a real MCP client over streamable HTTP. The subtests share state and run in order.
func TestE2ESmoke(t *testing.T) {
	api := startFakeAPI(t)

	opts := &options{
		AuthToken: e2eAuthToken,
		Listen:    freeAddr(t),
		Data:      t.TempDir(),
		Chats:     writeChats(t, fmt.Sprintf("chats:\n  %d:\n    customer: acme\n", e2eChatID)),
	}
	opts.Telegram.Token = e2eBotToken
	opts.Telegram.APIURL = api.URL

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	appCtx, stopApp := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- run(appCtx, opts) }()
	defer func() {
		stopApp()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Error("app did not shut down")
		}
	}()

	baseURL := "http://" + opts.Listen
	require.Eventually(t, func() bool {
		resp, err := http.Get(baseURL + "/ping")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond, "server never came up")

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: baseURL + "/mcp", HTTPClient: e2eBearerClient(e2eAuthToken)}, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	t.Run("all tools are exposed", func(t *testing.T) {
		res, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		assert.ElementsMatch(t, []string{"list_customers", "list_new", "get_thread", "get_history",
			"search", "get_file", "send_reply", "mark_handled"}, names)
	})

	// the ingest loop picks the scripted batch up on its first poll
	var listed e2eMessages
	require.Eventually(t, func() bool {
		listed = e2eMessages{}
		callTool(ctx, t, session, "list_new", nil, &listed)
		return len(listed.Messages) == 3
	}, 15*time.Second, 50*time.Millisecond, "ingest did not store the scripted batch: %+v", listed)

	t.Run("list_new shows the ingested messages", func(t *testing.T) {
		ids := make([]int64, 0, len(listed.Messages))
		for _, m := range listed.Messages {
			ids = append(ids, m.MessageID)
			assert.Equal(t, "acme", m.Customer)
			assert.Empty(t, m.Label, "single-group customer needs no label")
			assert.Empty(t, m.Text, "listings carry a snippet, not the full text")
			assert.NotEmpty(t, m.Snippet)
		}
		assert.Equal(t, []int64{101, 102, 103}, ids, "non-allowlisted chat and service message dropped")

		assert.Contains(t, listed.Messages[0].Snippet, "after the upgrade", "edit applied")
		assert.NotEmpty(t, listed.Messages[0].EditedAt)
		assert.True(t, listed.Messages[1].Mention, "@mention of the bot flagged")
		assert.Equal(t, "document", listed.Messages[2].Media)
		assert.Equal(t, "core.dump", listed.Messages[2].FileName)
	})

	t.Run("list_customers counts the group as unread", func(t *testing.T) {
		var res e2eCustomers
		callTool(ctx, t, session, "list_customers", nil, &res)
		require.Len(t, res.Customers, 1)
		assert.Equal(t, "acme", res.Customers[0].Customer)
		assert.Equal(t, 3, res.Customers[0].Unread)
		require.Len(t, res.Customers[0].Groups, 1)
		assert.Equal(t, 3, res.Customers[0].Groups[0].Unread)
	})

	t.Run("get_thread reconstructs the conversation", func(t *testing.T) {
		var res e2eMessages
		callTool(ctx, t, session, "get_thread",
			map[string]any{"customer": "acme", "message_id": 101}, &res)
		assert.False(t, res.Truncated)
		require.Len(t, res.Messages, 3)
		assert.Equal(t, int64(101), res.Messages[0].MessageID)
		assert.Contains(t, res.Messages[0].Text, "after the upgrade", "threads carry the full text")
		assert.Equal(t, int64(101), res.Messages[1].ReplyTo)
	})

	var reply e2eSendReply
	t.Run("send_reply posts and logs the answer", func(t *testing.T) {
		callTool(ctx, t, session, "send_reply", map[string]any{
			"customer": "acme", "text": "looking into it, please attach the agent log", "reply_to": 101,
		}, &reply)
		assert.Empty(t, reply.Warning)
		assert.True(t, reply.Message.FromBot)
		assert.Equal(t, int64(101), reply.Message.ReplyTo)

		sent := api.sentMessages()
		require.Len(t, sent, 1)
		chatID, ok := sent[0]["chat_id"].(float64)
		require.True(t, ok, "chat_id missing from the sendMessage payload: %+v", sent[0])
		assert.Equal(t, int64(e2eChatID), int64(chatID))
		assert.Equal(t, "looking into it, please attach the agent log", sent[0]["text"])
	})

	t.Run("the sent reply shows up in the thread", func(t *testing.T) {
		var res e2eMessages
		callTool(ctx, t, session, "get_thread",
			map[string]any{"customer": "acme", "message_id": 101}, &res)
		var found *e2eMessage
		for i, m := range res.Messages {
			if m.MessageID == reply.Message.MessageID {
				found = &res.Messages[i]
			}
		}
		require.NotNil(t, found, "the bot reply is missing from the thread: %+v", res.Messages)
		assert.True(t, found.FromBot)
		assert.Contains(t, found.Text, "please attach the agent log")
	})

	t.Run("send_reply rejects an oversized message", func(t *testing.T) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_reply", Arguments: map[string]any{
			"customer": "acme", "text": strings.Repeat("x", 5000),
		}})
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Contains(t, contentText(res), "split it")
		assert.Len(t, api.sentMessages(), 1, "nothing left the process")
	})

	t.Run("search finds the edited message", func(t *testing.T) {
		var res e2eMessages
		callTool(ctx, t, session, "search", map[string]any{"query": "upgrade"}, &res)
		require.Len(t, res.Messages, 1)
		assert.Equal(t, int64(101), res.Messages[0].MessageID)
		assert.NotEmpty(t, res.Messages[0].Snippet)

		var stale e2eMessages
		callTool(ctx, t, session, "search", map[string]any{"query": "connecting"}, &stale)
		assert.Empty(t, stale.Messages, "the pre-edit text is gone from the index")
	})

	t.Run("get_history returns the group chronologically", func(t *testing.T) {
		var res e2eMessages
		callTool(ctx, t, session, "get_history", map[string]any{"customer": "acme"}, &res)
		require.Len(t, res.Messages, 4, "three ingested messages plus our reply")
		for i := 1; i < len(res.Messages); i++ {
			assert.LessOrEqual(t, res.Messages[i-1].Sent, res.Messages[i].Sent)
		}
	})

	var file e2eFile
	t.Run("get_file hands out a download url", func(t *testing.T) {
		callTool(ctx, t, session, "get_file",
			map[string]any{"customer": "acme", "message_id": 103}, &file)
		assert.False(t, file.Inline, "a binary attachment is served, not inlined")
		assert.Equal(t, "document", file.Media)
		assert.Equal(t, "core.dump", file.FileName)
		assert.Equal(t, int64(len(api.file)), file.FileSize)
		require.NotEmpty(t, file.URL)
		assert.True(t, strings.HasPrefix(file.URL, baseURL), "url points back at this listener: %s", file.URL)
	})

	t.Run("the download url serves the cached file", func(t *testing.T) {
		body, status := fetch(ctx, t, file.URL, e2eAuthToken)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, api.file, body)
	})

	t.Run("the download url needs the token", func(t *testing.T) {
		_, status := fetch(ctx, t, file.URL, "")
		assert.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("mark_handled clears the triage cursor", func(t *testing.T) {
		var marked e2eMarkHandled
		callTool(ctx, t, session, "mark_handled",
			map[string]any{"customer": "acme", "message_id": 103}, &marked)
		assert.Equal(t, "acme", marked.Customer)
		assert.Equal(t, int64(103), marked.MarkedUpTo)

		var res e2eMessages
		callTool(ctx, t, session, "list_new", nil, &res)
		assert.Empty(t, res.Messages)

		var customers e2eCustomers
		callTool(ctx, t, session, "list_customers", nil, &customers)
		require.Len(t, customers.Customers, 1)
		assert.Equal(t, 0, customers.Customers[0].Unread)
	})

	t.Run("unknown customer is rejected", func(t *testing.T) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_history",
			Arguments: map[string]any{"customer": "globex"}})
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Contains(t, contentText(res), `unknown customer "globex"`)
	})

	t.Run("mcp rejects a client without the token", func(t *testing.T) {
		anon := mcp.NewClient(&mcp.Implementation{Name: "e2e-anon", Version: "v1"}, nil)
		_, err := anon.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: baseURL + "/mcp", MaxRetries: -1}, nil)
		require.Error(t, err)
	})
}

// callTool invokes a tool over the session and decodes its structured result.
func callTool(ctx context.Context, t *testing.T, session *mcp.ClientSession, name string,
	args map[string]any, out any) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "tool %q failed: %s", name, contentText(res))

	data, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, out))
}

func contentText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

func fetch(ctx context.Context, t *testing.T, url, token string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body, resp.StatusCode
}

func e2eBearerClient(token string) *http.Client {
	return &http.Client{Transport: e2eBearerTransport{token: token}}
}

type e2eBearerTransport struct{ token string }

func (t e2eBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone) //nolint:wrapcheck // transport errors pass through
}

// fakeAPI is a scripted Bot API server: getUpdates hands out one batch and then idles, getFile and
// the download route serve a single attachment, and sendMessage records what the app posted.
type fakeAPI struct {
	*httptest.Server
	updates []scriptedUpdate
	file    []byte

	mu     sync.Mutex
	sent   []map[string]any
	nextID int64
}

type scriptedUpdate struct {
	id   int64
	body string
}

func (a *fakeAPI) sentMessages() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.sent...)
}

func startFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{updates: scriptedBatch(), file: binaryAttachment(), nextID: 900}
	api.Server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.Close)
	return api
}

func (a *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/file/bot"+e2eBotToken+"/") {
		_, _ = w.Write(a.file)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch strings.TrimPrefix(r.URL.Path, "/bot"+e2eBotToken+"/") {
	case "getMe":
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"is_bot":true,"username":%q}}`, e2eBotID, e2eBotName)
	case "deleteWebhook":
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	case "getUpdates":
		a.serveUpdates(w, r)
	case "getFile":
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"doc-1","file_unique_id":"uniq-1",`+
			`"file_size":%d,"file_path":%q}}`, len(a.file), e2eFilePath)
	case "sendMessage":
		a.serveSend(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
	}
}

// serveUpdates replays the scripted updates the caller has not confirmed yet, honoring the offset
// the ingest loop advances only after a batch is stored.
func (a *fakeAPI) serveUpdates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Offset int64 `json:"offset"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	pending := make([]string, 0, len(a.updates))
	for _, u := range a.updates {
		if u.id >= req.Offset {
			pending = append(pending, u.body)
		}
	}
	if len(pending) == 0 { // a short poll: long enough not to spin, short enough never to block Close
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	_, _ = fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, strings.Join(pending, ","))
}

func (a *fakeAPI) serveSend(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request"}`)
		return
	}

	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.sent = append(a.sent, req)
	a.mu.Unlock()

	result := map[string]any{
		"message_id": id,
		"from":       map[string]any{"id": e2eBotID, "is_bot": true, "username": e2eBotName},
		"chat":       map[string]any{"id": e2eChatID, "type": "supergroup", "title": "Acme Support"},
		"date":       time.Now().Unix(),
		"text":       req["text"],
	}
	if params, ok := req["reply_parameters"].(map[string]any); ok {
		result["reply_to_message"] = map[string]any{"message_id": params["message_id"]}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// scriptedBatch is what the fake api delivers on the first poll: three messages worth keeping,
// one from a chat outside the allowlist, one service message and one edit.
func scriptedBatch() []scriptedUpdate {
	now := time.Now().Add(-time.Minute).Unix()
	jane := `{"id":501,"is_bot":false,"first_name":"Jane","last_name":"Doe"}`
	john := `{"id":502,"is_bot":false,"first_name":"John","username":"jroe"}`
	chat := fmt.Sprintf(`{"id":%d,"type":"supergroup","title":"Acme Support"}`, e2eChatID)

	return []scriptedUpdate{
		{id: 1, body: fmt.Sprintf(
			`{"update_id":1,"message":{"message_id":101,"from":%s,"chat":%s,"date":%d,
			  "text":"the agent stopped connecting to the server"}}`, jane, chat, now)},
		{id: 2, body: fmt.Sprintf(
			`{"update_id":2,"message":{"message_id":1,"from":%s,"chat":{"id":%d,"type":"group","title":"Randoms"},
			  "date":%d,"text":"who are you"}}`, john, e2eOtherChat, now)},
		{id: 3, body: fmt.Sprintf(
			`{"update_id":3,"message":{"message_id":102,"from":%s,"chat":%s,"date":%d,
			  "reply_to_message":{"message_id":101,"from":%s,"chat":%s,"date":%d},
			  "text":"@support_bot any idea?","entities":[{"type":"mention","offset":0,"length":12}]}}`,
			john, chat, now+1, jane, chat, now)},
		{id: 4, body: fmt.Sprintf(
			`{"update_id":4,"message":{"message_id":103,"from":%s,"chat":%s,"date":%d,
			  "caption":"here is the dump",
			  "document":{"file_id":"doc-1","file_unique_id":"uniq-1","file_name":"core.dump",
			    "mime_type":"application/octet-stream","file_size":%d}}}`,
			jane, chat, now+2, len(binaryAttachment()))},
		{id: 5, body: fmt.Sprintf(
			`{"update_id":5,"message":{"message_id":104,"from":%s,"chat":%s,"date":%d,
			  "new_chat_title":"Acme Support (EU)"}}`, jane, chat, now+3)},
		{id: 6, body: fmt.Sprintf(
			`{"update_id":6,"edited_message":{"message_id":101,"from":%s,"chat":%s,"date":%d,"edit_date":%d,
			  "text":"the agent stopped talking to the server after the upgrade"}}`,
			jane, chat, now, now+4)},
	}
}

// binaryAttachment is deliberately not valid utf-8, so get_file has to serve it over http instead
// of inlining it in the tool result.
func binaryAttachment() []byte {
	data := bytes.Repeat([]byte{0x00, 0xff, 0x10, 0x80}, 512)
	return append([]byte("\x7fELF"), data...)
}
