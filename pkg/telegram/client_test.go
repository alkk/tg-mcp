package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "123:secret-token"

// newTestClient points a client at a fake bot api server and shrinks retry waits.
func newTestClient(t *testing.T, local bool, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New(testToken, srv.URL, local)
	c.retryUnit = time.Millisecond
	return c
}

// decodePayload reads the json request body of a fake api call.
func decodePayload(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
	return payload
}

func writeOK(t *testing.T, w http.ResponseWriter, result string) {
	t.Helper()
	_, err := io.WriteString(w, `{"ok":true,"result":`+result+`}`)
	require.NoError(t, err)
}

func writeErr(t *testing.T, w http.ResponseWriter, code int, body string) {
	t.Helper()
	w.WriteHeader(code)
	_, err := io.WriteString(w, body)
	require.NoError(t, err)
}

func TestClientGetMe(t *testing.T) {
	c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/bot"+testToken+"/getMe", r.URL.Path)
		writeOK(t, w, `{"id":1,"is_bot":true,"first_name":"tg","username":"tgbot"}`)
	})

	me, err := c.GetMe(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tgbot", me.Username)
	assert.True(t, me.IsBot)
}

func TestClientDeleteWebhook(t *testing.T) {
	var seen map[string]any
	c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bot"+testToken+"/deleteWebhook", r.URL.Path)
		seen = decodePayload(t, r)
		writeOK(t, w, `true`)
	})

	require.NoError(t, c.DeleteWebhook(context.Background()))
	assert.Equal(t, map[string]any{"drop_pending_updates": false}, seen)
}

func TestClientGetUpdates(t *testing.T) {
	t.Run("offset and allowed updates", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			seen = decodePayload(t, r)
			writeOK(t, w, `[{"update_id":10,"message":{"message_id":1,"date":1700000000,"text":"hi",
				"chat":{"id":-1001,"type":"supergroup"}}},
				{"update_id":11,"edited_message":{"message_id":1,"date":1700000000,"edit_date":1700000100,"text":"hi!",
				"chat":{"id":-1001,"type":"supergroup"}}}]`)
		})

		updates, err := c.GetUpdates(context.Background(), 10, 2*time.Second)
		require.NoError(t, err)
		require.Len(t, updates, 2)
		assert.Equal(t, int64(10), updates[0].UpdateID)
		require.NotNil(t, updates[1].EditedMessage)
		assert.Equal(t, "hi!", updates[1].EditedMessage.Text)

		assert.InDelta(t, float64(10), seen["offset"], 0)
		assert.InDelta(t, float64(2), seen["timeout"], 0)
		assert.Equal(t, []any{"message", "edited_message"}, seen["allowed_updates"])
	})

	t.Run("zero offset is omitted", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			seen = decodePayload(t, r)
			writeOK(t, w, `[]`)
		})

		updates, err := c.GetUpdates(context.Background(), 0, time.Second)
		require.NoError(t, err)
		assert.Empty(t, updates)
		assert.NotContains(t, seen, "offset")
	})

	t.Run("409 conflict", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusConflict,
				`{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active"}`)
		})

		_, err := c.GetUpdates(context.Background(), 0, time.Second)
		require.Error(t, err)

		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.True(t, apiErr.IsConflict())
		assert.Contains(t, apiErr.Error(), "webhook is active")
	})

	t.Run("context canceled", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.GetUpdates(ctx, 0, time.Second)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestClientSendMessage(t *testing.T) {
	t.Run("with reply and topic", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/bot"+testToken+"/sendMessage", r.URL.Path)
			seen = decodePayload(t, r)
			writeOK(t, w, `{"message_id":99,"date":1700000000,"text":"on it",
				"chat":{"id":-1001,"type":"supergroup"},"from":{"id":1,"is_bot":true,"username":"tgbot"}}`)
		})

		msg, err := c.SendMessage(context.Background(), -1001, "on it", "", 12, 3)
		require.NoError(t, err)
		assert.Equal(t, int64(99), msg.MessageID)
		assert.True(t, msg.From.IsBot)

		assert.InDelta(t, float64(-1001), seen["chat_id"], 0)
		assert.Equal(t, "on it", seen["text"])
		assert.Equal(t, map[string]any{"message_id": float64(12)}, seen["reply_parameters"])
		assert.InDelta(t, float64(3), seen["message_thread_id"], 0)
	})

	t.Run("bare send", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			seen = decodePayload(t, r)
			writeOK(t, w, `{"message_id":100,"date":1700000000,"chat":{"id":-1001,"type":"supergroup"}}`)
		})

		_, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		require.NoError(t, err)
		assert.NotContains(t, seen, "reply_parameters")
		assert.NotContains(t, seen, "message_thread_id")
		assert.NotContains(t, seen, "parse_mode", "an empty parse mode leaves the key out entirely")
	})

	t.Run("html parse mode", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			seen = decodePayload(t, r)
			writeOK(t, w, `{"message_id":101,"date":1700000000,"text":"run jcmd",
				"chat":{"id":-1001,"type":"supergroup"}}`)
		})

		msg, err := c.SendMessage(context.Background(), -1001, "run <code>jcmd</code>", ParseModeHTML, 0, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(101), msg.MessageID)
		assert.Equal(t, "HTML", seen["parse_mode"])
		assert.Equal(t, "run <code>jcmd</code>", seen["text"])
	})

	t.Run("429 retried once", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				writeErr(t, w, http.StatusTooManyRequests,
					`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`)
				return
			}
			writeOK(t, w, `{"message_id":99,"date":1700000000,"chat":{"id":-1001,"type":"supergroup"}}`)
		})

		msg, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(99), msg.MessageID)
		assert.Equal(t, int32(2), calls.Load())
	})

	t.Run("429 twice surfaces", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			writeErr(t, w, http.StatusTooManyRequests,
				`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
		})

		_, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		require.Error(t, err)

		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, http.StatusTooManyRequests, apiErr.Code)
		assert.Equal(t, 1, apiErr.RetryAfter)
		assert.Equal(t, int32(2), calls.Load(), "one retry only")
	})

	t.Run("api error carries migrate_to_chat_id", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusBadRequest,
				`{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat",
				"parameters":{"migrate_to_chat_id":-1009}}`)
		})

		_, err := c.SendMessage(context.Background(), -100, "hi", "", 0, 0)
		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, int64(-1009), apiErr.MigrateToChatID)
	})

	t.Run("malformed response", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			_, err := io.WriteString(w, `not json at all`)
			assert.NoError(t, err)
		})

		_, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode sendMessage response")
	})

	t.Run("result type mismatch", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeOK(t, w, `"a string, not a message"`)
		})

		_, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode sendMessage result")
	})

	t.Run("error without error_code falls back to http status", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusBadGateway, `{"ok":false,"description":"upstream down"}`)
		})

		_, err := c.SendMessage(context.Background(), -1001, "hi", "", 0, 0)
		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, http.StatusBadGateway, apiErr.Code)
	})
}

func TestClientGetFile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var seen map[string]any
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/bot"+testToken+"/getFile", r.URL.Path)
			seen = decodePayload(t, r)
			writeOK(t, w, `{"file_id":"f1","file_unique_id":"u1","file_size":17,"file_path":"documents/file_1.log"}`)
		})

		f, err := c.GetFile(context.Background(), "f1")
		require.NoError(t, err)
		assert.Equal(t, "documents/file_1.log", f.FilePath)
		assert.Equal(t, int64(17), f.FileSize)
		assert.Equal(t, "f1", seen["file_id"])
	})

	t.Run("429 retried once", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				writeErr(t, w, http.StatusTooManyRequests,
					`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
				return
			}
			writeOK(t, w, `{"file_id":"f1","file_unique_id":"u1","file_path":"documents/file_1.log"}`)
		})

		f, err := c.GetFile(context.Background(), "f1")
		require.NoError(t, err)
		assert.Equal(t, "u1", f.FileUniqueID)
		assert.Equal(t, int32(2), calls.Load())
	})

	t.Run("cloud mode explains the size limit", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`)
		})

		_, err := c.GetFile(context.Background(), "f1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "20 MB")

		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	})

	t.Run("local mode does not add the cloud hint", func(t *testing.T) {
		c := newTestClient(t, true, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`)
		})

		_, err := c.GetFile(context.Background(), "f1")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "20 MB")
	})
}

func TestClientDownload(t *testing.T) {
	t.Run("cloud mode", func(t *testing.T) {
		var path string
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_, err := io.WriteString(w, "file body")
			assert.NoError(t, err)
		})

		var buf bytes.Buffer
		require.NoError(t, c.Download(context.Background(), "documents/file_1.log", &buf))
		assert.Equal(t, "file body", buf.String())
		assert.Equal(t, "/file/bot"+testToken+"/documents/file_1.log", path)
	})

	t.Run("cloud mode leading slash", func(t *testing.T) {
		var path string
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_, err := io.WriteString(w, "x")
			assert.NoError(t, err)
		})

		var buf bytes.Buffer
		require.NoError(t, c.Download(context.Background(), "/documents/file_1.log", &buf))
		assert.Equal(t, "/file/bot"+testToken+"/documents/file_1.log", path)
	})

	t.Run("cloud mode http error", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
			writeErr(t, w, http.StatusNotFound, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		})

		var buf bytes.Buffer
		err := c.Download(context.Background(), "documents/gone.log", &buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "http 404")
		assert.Contains(t, err.Error(), "Not Found")
	})

	t.Run("a slow body outlives the call timeout", func(t *testing.T) {
		c := newTestClient(t, false, func(w http.ResponseWriter, _ *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !assert.True(t, ok) {
				return
			}
			for range 4 {
				_, err := io.WriteString(w, "chunk")
				assert.NoError(t, err)
				flusher.Flush()
				time.Sleep(20 * time.Millisecond)
			}
		})
		// http.Client.Timeout covers reading the body too, so the method timeout must not apply
		c.client.Timeout = 10 * time.Millisecond

		var buf bytes.Buffer
		require.NoError(t, c.Download(context.Background(), "documents/big.bin", &buf))
		assert.Equal(t, "chunkchunkchunkchunk", buf.String())
	})

	t.Run("local mode copies from the shared volume", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "file_1.log")
		require.NoError(t, os.WriteFile(src, []byte("local body"), 0o600))

		c := newTestClient(t, true, func(w http.ResponseWriter, r *http.Request) {
			t.Error("local mode must not hit the api server")
		})

		var buf bytes.Buffer
		require.NoError(t, c.Download(context.Background(), src, &buf))
		assert.Equal(t, "local body", buf.String())
	})

	t.Run("local mode rejects a relative path", func(t *testing.T) {
		c := newTestClient(t, true, func(w http.ResponseWriter, r *http.Request) {})

		var buf bytes.Buffer
		err := c.Download(context.Background(), "documents/file_1.log", &buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--local")
	})

	t.Run("local mode missing file", func(t *testing.T) {
		c := newTestClient(t, true, func(w http.ResponseWriter, r *http.Request) {})

		var buf bytes.Buffer
		err := c.Download(context.Background(), filepath.Join(t.TempDir(), "nope.log"), &buf)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestClientTokenNotLeaked(t *testing.T) {
	c := New(testToken, "http://127.0.0.1:1", false)
	c.retryUnit = time.Millisecond

	_, err := c.GetMe(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	assert.Contains(t, err.Error(), "***")

	var buf bytes.Buffer
	err = c.Download(context.Background(), "documents/file_1.log", &buf)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}

func TestAPIErrorClasses(t *testing.T) {
	tests := []struct {
		code     int
		conflict bool
		auth     bool
	}{
		{code: http.StatusConflict, conflict: true},
		{code: http.StatusUnauthorized, auth: true},
		{code: http.StatusForbidden, auth: true},
		{code: http.StatusNotFound, auth: true},
		{code: http.StatusBadRequest},
		{code: http.StatusTooManyRequests},
		{code: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			err := &APIError{Method: "getUpdates", Code: tt.code}
			assert.Equal(t, tt.conflict, err.IsConflict())
			assert.Equal(t, tt.auth, err.IsAuthFailure())
		})
	}
}

func TestClientBaseURLTrailingSlash(t *testing.T) {
	c := New(testToken, "https://api.telegram.org/", false)
	assert.Equal(t, "https://api.telegram.org/bot"+testToken+"/getMe", c.methodURL("getMe"))
}

func TestClientRetryWaitCanceled(t *testing.T) {
	c := newTestClient(t, false, func(w http.ResponseWriter, r *http.Request) {
		writeErr(t, w, http.StatusTooManyRequests,
			`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":30}}`)
	})
	c.retryUnit = time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.SendMessage(ctx, -1001, "hi", "", 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry_after")
}
