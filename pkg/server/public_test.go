package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/server/mocks"
	"github.com/alkk/tg-mcp/pkg/store"
)

const (
	publicChatID  = int64(-1001234567890)
	publicChatMap = `
chats:
  -1001234567890:
    customer: community
    label: en
    public: netxms-en
    username: netxms_en
  -1001234567891:
    customer: community
    label: ru
    public: netxms-ru
  -1001234567892:
    customer: acme
    label: support
`
)

func publicServer(t *testing.T, st *mocks.MessageStore) *Server {
	t.Helper()
	s, err := New(Params{Store: st, Chats: testConfig(t, publicChatMap), AuthToken: testToken})
	require.NoError(t, err)
	return s
}

func getPublic(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func latestReturning(msg *store.Message) *mocks.MessageStore {
	return &mocks.MessageStore{
		LatestFunc: func(int64) (store.Message, bool) {
			if msg == nil {
				return store.Message{}, false
			}
			return *msg, true
		},
	}
}

func TestServePublic(t *testing.T) {
	sent := time.Date(2026, 9, 25, 13, 4, 12, 0, time.FixedZone("EEST", 3*60*60))

	tests := []struct {
		name       string
		path       string
		wantChatID int64
		msg        *store.Message
		want       string
	}{
		{
			name:       "message with link",
			path:       "/public/netxms-en",
			wantChatID: publicChatID,
			msg: &store.Message{ChatID: publicChatID, MessageID: 48213, Sent: sent, SenderID: 77,
				SenderName: "John D.", Text: "has anyone tried 5.2 with ...", ReplyTo: 48200},
			want: `{"message":{"sent":"2026-09-25T10:04:12Z","sender":"John D.",` +
				`"text":"has anyone tried 5.2 with ...","link":"https://t.me/netxms_en/48213"}}`,
		},
		{
			name:       "no link without username",
			path:       "/public/netxms-ru",
			wantChatID: -1001234567891,
			msg: &store.Message{ChatID: -1001234567891, MessageID: 48213, Sent: sent,
				SenderName: "Иван", Text: "привет"},
			want: `{"message":{"sent":"2026-09-25T10:04:12Z","sender":"Иван","text":"привет"}}`,
		},
		{
			name:       "chat without messages",
			path:       "/public/netxms-en",
			wantChatID: publicChatID,
			want:       `{"message":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := latestReturning(tt.msg)
			rec := getPublic(t, publicServer(t, st), tt.path)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.JSONEq(t, tt.want, rec.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			assert.NotContains(t, rec.Body.String(), "100123456789", "chat id leaked")

			calls := st.LatestCalls()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.wantChatID, calls[0].ChatID)
			assert.Empty(t, st.HistoryCalls(), "the endpoint never queries the database")
		})
	}
}

func TestPublicView(t *testing.T) {
	sent := time.Date(2026, 9, 25, 13, 4, 12, 0, time.FixedZone("EEST", 3*60*60))

	tests := []struct {
		name     string
		msg      store.Message
		username string
		want     *publicMessage
	}{
		{
			name: "text message with link",
			msg: store.Message{ChatID: publicChatID, MessageID: 48213, Sent: sent, SenderID: 77,
				SenderName: "John D.", Text: "has anyone tried 5.2 with ...", ReplyTo: 48200},
			username: "netxms_en",
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "John D.",
				Text: "has anyone tried 5.2 with ...", Link: "https://t.me/netxms_en/48213"},
		},
		{
			name: "no link without username",
			msg:  store.Message{ChatID: publicChatID, MessageID: 48213, Sent: sent, SenderName: "Иван", Text: "привет"},
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "Иван", Text: "привет"},
		},
		{
			name: "document with a name the sender gave",
			msg: store.Message{ChatID: publicChatID, MessageID: 7, Sent: sent, SenderName: "alice",
				Text: "log attached", MediaType: "document", FileID: "file-id-1",
				FileUniqueID: "AgADuQ", FileName: "server.log", FileSize: 98765},
			username: "netxms_en",
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "alice", Text: "log attached",
				Media: &publicMedia{Type: "document", FileName: "server.log"}, Link: "https://t.me/netxms_en/7"},
		},
		{
			name: "photo with a synthesized name",
			msg: store.Message{ChatID: publicChatID, MessageID: 8, Sent: sent, SenderName: "alice",
				MediaType: "photo", FileID: "file-id-2", FileUniqueID: "AgADvA", FileName: "AgADvA.jpg"},
			username: "netxms_en",
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "alice",
				Media: &publicMedia{Type: "photo"}, Link: "https://t.me/netxms_en/8"},
		},
		{
			name: "nameless document falls back to the file unique id",
			msg: store.Message{ChatID: publicChatID, MessageID: 9, Sent: sent, SenderName: "alice",
				Text: "dump", MediaType: "document", FileID: "file-id-3", FileUniqueID: "AgADwB", FileName: "AgADwB"},
			username: "netxms_en",
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "alice", Text: "dump",
				Media: &publicMedia{Type: "document"}, Link: "https://t.me/netxms_en/9"},
		},
		{
			name: "text is passed through without html processing",
			msg: store.Message{ChatID: publicChatID, MessageID: 10, Sent: sent, SenderName: "<b>eve</b>",
				Text: "<script>alert(1)</script> & <b>"},
			username: "netxms_en",
			want: &publicMessage{Sent: "2026-09-25T10:04:12Z", Sender: "<b>eve</b>",
				Text: "<script>alert(1)</script> & <b>", Link: "https://t.me/netxms_en/10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, publicView(tt.msg, tt.username))
		})
	}
}

func TestServePublicErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "unknown alias", path: "/public/nope"},
		{name: "alias is case sensitive", path: "/public/NETXMS-EN"},
		{name: "chat without alias by customer slug", path: "/public/acme"},
		{name: "chat without alias by label", path: "/public/support"},
		{name: "customer slug of a public chat", path: "/public/community"},
		{name: "bare prefix is the router's 404", path: "/public/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := latestReturning(&store.Message{ChatID: publicChatID, MessageID: 1, SenderName: "x"})
			rec := getPublic(t, publicServer(t, st), tt.path)

			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.NotContains(t, rec.Body.String(), "100123456789", "chat id leaked")
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Empty(t, rec.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), "net/http sets it on every error")
			assert.Empty(t, st.LatestCalls())
			assert.Empty(t, st.HistoryCalls())
		})
	}
}
