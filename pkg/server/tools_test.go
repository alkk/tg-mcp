package server

import (
	"context"
	"encoding/base64"
	"errors"
	stdhtml "html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/server/mocks"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

// base is the timestamp seeded messages are laid out around, recent enough for the default
// 24h window of get_history to cover them.
func seedBase() time.Time { return time.Now().Add(-time.Hour).Truncate(time.Second).UTC() }

// seedPayloads are the attachment bytes the seeded conversation refers to. get_thread inlines
// images, so every fake bot api a seeded server gets has to answer for them: moq panics on an
// unscripted call, and threadImage swallows that panic into a missing image block.
func seedPayloads() map[string][]byte { return map[string][]byte{"f4": pngData} }

// seededServer returns a server backed by a real store holding a small support conversation in
// acme's only group plus one message in each globex group.
func seededServer(t *testing.T) (*Server, time.Time) {
	t.Helper()
	return seededServerWith(t, fakeAPI(seedPayloads()))
}

// seededServerWith is seededServer with a fake bot api the test can script.
func seededServerWith(t *testing.T, tg *mocks.TelegramAPI) (*Server, time.Time) {
	t.Helper()

	st, err := store.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	base := seedBase()
	msgs := []store.Message{
		{ChatID: -1001, MessageID: 1, Sent: base, SenderID: 11, SenderName: "alice", Text: "server is down"},
		{ChatID: -1001, MessageID: 2, Sent: base.Add(time.Minute), SenderID: 12, SenderName: "bob",
			ReplyTo: 1, Text: "which server?"},
		{ChatID: -1001, MessageID: 3, Sent: base.Add(2 * time.Minute), SenderID: 11, SenderName: "alice",
			Text: "nxagentd on host7"},
		{ChatID: -1001, MessageID: 4, Sent: base.Add(3 * time.Minute), SenderID: 11, SenderName: "alice",
			Text: "screenshot", IsMention: true, MediaType: "photo", FileID: "f4", FileUniqueID: "u4",
			FileName: "shot.png", FileSize: 1024},
		{ChatID: -1001, MessageID: 5, Sent: base.Add(4 * time.Minute), SenderName: "tg-mcp bot",
			FromBot: true, ReplyTo: 1, Text: "we are on it"},
		{ChatID: -1001, MessageID: 6, Sent: base.Add(5 * time.Minute), SenderID: 12, SenderName: "bob",
			Text: strings.Repeat("a", 300), EditedAt: base.Add(6 * time.Minute)},
		{ChatID: -1002, MessageID: 10, Sent: base.Add(7 * time.Minute), SenderID: 21, SenderName: "carol",
			Text: "license question"},
		{ChatID: -1003, MessageID: 20, Sent: base.Add(8 * time.Minute), SenderID: 22, SenderName: "dave",
			Text: "escalating the outage"},
	}
	require.NoError(t, st.UpsertBatch(context.Background(), msgs))

	srv, err := New(Params{Store: st, Telegram: tg, Chats: testConfig(t, chatMap),
		AuthToken: testToken, Listen: "127.0.0.1:0"})
	require.NoError(t, err)
	return srv, base
}

func messageIDs(views []messageView) []int64 {
	if len(views) == 0 {
		return nil
	}
	res := make([]int64, 0, len(views))
	for _, v := range views {
		res = append(res, v.MessageID)
	}
	return res
}

func TestToolsListCustomers(t *testing.T) {
	s, _ := seededServer(t)
	ctx := context.Background()

	_, res, err := s.listCustomers(ctx, nil, struct{}{})
	require.NoError(t, err)
	require.Len(t, res.Customers, 2)

	assert.Equal(t, customerView{Customer: "acme", Unread: 5, Groups: []groupView{{Unread: 5}}}, res.Customers[0])
	assert.Equal(t, customerView{Customer: "globex", Unread: 2, Groups: []groupView{
		{Label: "escalations", Unread: 1}, {Label: "main", Unread: 1},
	}}, res.Customers[1], "own replies never count as unread")

	t.Run("cursor lowers the count", func(t *testing.T) {
		_, err := s.store.SetCursor(ctx, -1001, 3)
		require.NoError(t, err)
		_, res, err := s.listCustomers(ctx, nil, struct{}{})
		require.NoError(t, err)
		assert.Equal(t, 2, res.Customers[0].Unread)
	})

	t.Run("store error", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{
			UnreadCountsFunc: func(context.Context, []int64) (map[int64]int, error) {
				return nil, errors.New("db is gone")
			},
		})
		_, _, err := s.listCustomers(ctx, nil, struct{}{})
		require.ErrorContains(t, err, "read unread counts: db is gone")
	})
}

func TestToolsListNew(t *testing.T) {
	s, _ := seededServer(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		params  listNewParams
		want    []int64
		wantErr string
	}{
		{name: "every group", want: []int64{1, 2, 3, 4, 6, 10, 20}},
		{name: "one customer", params: listNewParams{Customer: "acme"}, want: []int64{1, 2, 3, 4, 6}},
		{name: "both groups of a customer", params: listNewParams{Customer: "globex"}, want: []int64{10, 20}},
		{name: "limit", params: listNewParams{Customer: "acme", Limit: 2}, want: []int64{1, 2}},
		{name: "unknown customer", params: listNewParams{Customer: "initech"}, wantErr: `unknown customer "initech"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res, err := s.listNew(ctx, nil, tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, messageIDs(res.Messages), "own replies are not new")
		})
	}

	t.Run("result shape", func(t *testing.T) {
		_, res, err := s.listNew(ctx, nil, listNewParams{Customer: "acme"})
		require.NoError(t, err)
		require.Len(t, res.Messages, 5)

		media := res.Messages[3]
		assert.Equal(t, "acme", media.Customer)
		assert.Empty(t, media.Label, "a single group customer needs no label")
		assert.Equal(t, "alice", media.Sender)
		assert.Equal(t, "photo", media.Media)
		assert.Equal(t, "shot.png", media.FileName)
		assert.Equal(t, int64(1024), media.FileSize)
		assert.True(t, media.Mention)
		assert.Equal(t, "screenshot", media.Snippet)
		assert.Empty(t, media.Text, "listings carry an excerpt, not the full text")

		long := res.Messages[4]
		assert.Equal(t, strings.Repeat("a", snippetRunes)+ellipsis, long.Snippet)
		assert.NotEmpty(t, long.EditedAt)
	})

	t.Run("multi group customer shows labels", func(t *testing.T) {
		_, res, err := s.listNew(ctx, nil, listNewParams{Customer: "globex"})
		require.NoError(t, err)
		require.Len(t, res.Messages, 2)
		assert.Equal(t, "main", res.Messages[0].Label)
		assert.Equal(t, "escalations", res.Messages[1].Label)
	})

	t.Run("store error", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{
			ListNewFunc: func(context.Context, []int64, int) ([]store.Message, error) {
				return nil, errors.New("db is gone")
			},
		})
		_, _, err := s.listNew(ctx, nil, listNewParams{})
		require.ErrorContains(t, err, "list new messages: db is gone")
	})
}

func TestToolsListNewDefaultLimit(t *testing.T) {
	var gotLimit int
	s := failingServer(t, &mocks.MessageStore{
		ListNewFunc: func(_ context.Context, _ []int64, limit int) ([]store.Message, error) {
			gotLimit = limit
			return nil, nil
		},
	})

	_, _, err := s.listNew(context.Background(), nil, listNewParams{})
	require.NoError(t, err)
	assert.Equal(t, listNewDefaultLimit, gotLimit)
}

func TestLimitOr(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		def   int
		want  int
	}{
		{name: "zero takes the default", limit: 0, def: 100, want: 100},
		{name: "negative takes the default", limit: -5, def: 100, want: 100},
		{name: "within range passes through", limit: 42, def: 100, want: 42},
		{name: "at the cap passes through", limit: maxLimit, def: 100, want: maxLimit},
		{name: "above the cap is clamped", limit: 100_000_000, def: 100, want: maxLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, limitOr(tt.limit, tt.def))
		})
	}
}

// a caller that guesses a huge limit must not be able to pull the whole store into memory
func TestToolsLimitsAreCapped(t *testing.T) {
	var listNewLimit, historyLimit, searchLimit int
	s := failingServer(t, &mocks.MessageStore{
		ListNewFunc: func(_ context.Context, _ []int64, limit int) ([]store.Message, error) {
			listNewLimit = limit
			return nil, nil
		},
		HistoryFunc: func(_ context.Context, _ []int64, _, _ time.Time, _ *store.HistoryCursor,
			limit int) ([]store.Message, error) {
			historyLimit = limit
			return nil, nil
		},
		SearchFunc: func(_ context.Context, _ string, _ []int64, _, _ time.Time, limit int) ([]store.SearchHit, error) {
			searchLimit = limit
			return nil, nil
		},
	})
	ctx := context.Background()

	_, _, err := s.listNew(ctx, nil, listNewParams{Limit: 100_000_000})
	require.NoError(t, err)
	_, _, err = s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Limit: 100_000_000})
	require.NoError(t, err)
	_, _, err = s.search(ctx, nil, searchParams{Query: "x", Limit: 100_000_000})
	require.NoError(t, err)

	assert.Equal(t, maxLimit, listNewLimit)
	assert.Equal(t, maxLimit, historyLimit)
	assert.Equal(t, maxLimit, searchLimit)
}

func TestToolsGetThread(t *testing.T) {
	s, _ := seededServer(t)
	ctx := context.Background()

	t.Run("chain, replies and span fill", func(t *testing.T) {
		_, res, err := s.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 2})
		require.NoError(t, err)
		assert.False(t, res.Truncated)
		assert.Equal(t, []int64{1, 2, 3, 4, 5, 6}, messageIDs(res.Messages),
			"unlinked answers come in through the span fill")
		assert.Equal(t, "server is down", res.Messages[0].Text, "threads carry the full text")
		assert.Empty(t, res.Messages[0].Snippet)
		assert.True(t, res.Messages[4].FromBot)
	})

	t.Run("labeled group", func(t *testing.T) {
		_, res, err := s.getThread(ctx, nil, getThreadParams{Customer: "globex", Label: "main", MessageID: 10})
		require.NoError(t, err)
		assert.Equal(t, []int64{10}, messageIDs(res.Messages))
		assert.Equal(t, "main", res.Messages[0].Label)
	})

	t.Run("ambiguous customer", func(t *testing.T) {
		var ambiguous *AmbiguousChatError
		_, _, err := s.getThread(ctx, nil, getThreadParams{Customer: "globex", MessageID: 10})
		require.ErrorAs(t, err, &ambiguous)
		assert.Contains(t, err.Error(), "escalations, main")
	})

	t.Run("unknown customer", func(t *testing.T) {
		var unknown *UnknownCustomerError
		_, _, err := s.getThread(ctx, nil, getThreadParams{Customer: "initech", MessageID: 1})
		require.ErrorAs(t, err, &unknown)
	})

	t.Run("unknown message", func(t *testing.T) {
		_, _, err := s.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 999})
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("message of another customer", func(t *testing.T) {
		_, _, err := s.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 10})
		require.ErrorIs(t, err, store.ErrNotFound, "message ids never cross the customer boundary")
	})

	t.Run("images ride along with the conversation", func(t *testing.T) {
		srv, _ := seededServer(t)

		res, out, err := srv.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 2})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Content, 1)
		img, ok := res.Content[0].(*mcp.ImageContent)
		require.True(t, ok, "unexpected content type %T", res.Content[0])
		assert.Equal(t, pngData, img.Data)

		var flagged []int64
		for _, v := range out.Messages {
			if v.Inlined {
				flagged = append(flagged, v.MessageID)
			}
		}
		assert.Equal(t, []int64{4}, flagged, "the flags pair with the blocks by order")
	})

	t.Run("a thread without images returns no content", func(t *testing.T) {
		res, out, err := s.getThread(ctx, nil, getThreadParams{Customer: "globex", Label: "main", MessageID: 10})
		require.NoError(t, err)
		assert.Nil(t, res, "an empty result would strip the conversation out of the content block")
		assert.Equal(t, []int64{10}, messageIDs(out.Messages))
	})

	t.Run("no telegram client leaves the thread untouched", func(t *testing.T) {
		srv := fileServer(t, nil)

		res, out, err := srv.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 1})
		require.NoError(t, err)
		assert.Nil(t, res)
		assert.Contains(t, messageIDs(out.Messages), int64(1))
		for _, v := range out.Messages {
			assert.False(t, v.Inlined)
		}
	})
}

func TestToolsGetHistory(t *testing.T) {
	s, base := seededServer(t)
	ctx := context.Background()
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }

	tests := []struct {
		name    string
		params  getHistoryParams
		want    []int64
		wantErr string
	}{
		{name: "defaults to the last day", params: getHistoryParams{Customer: "acme"},
			want: []int64{1, 2, 3, 4, 5, 6}},
		{name: "range", params: getHistoryParams{Customer: "acme",
			From: rfc(base.Add(time.Minute)), To: rfc(base.Add(3 * time.Minute))}, want: []int64{2, 3, 4}},
		{name: "limit keeps the newest", params: getHistoryParams{Customer: "acme", Limit: 2},
			want: []int64{5, 6}},
		{name: "labeled group", params: getHistoryParams{Customer: "globex", Label: "escalations"},
			want: []int64{20}},
		{name: "empty range", params: getHistoryParams{Customer: "acme",
			From: rfc(base.Add(time.Hour)), To: rfc(base.Add(2 * time.Hour))}},
		{name: "both groups of a customer", params: getHistoryParams{Customer: "globex"}, want: []int64{10, 20}},
		{name: "ambiguous label", params: getHistoryParams{Customer: "globex", Label: "billing"},
			wantErr: "available labels"},
		{name: "empty customer never widens to every group", params: getHistoryParams{},
			wantErr: `unknown customer ""`},
		{name: "bad from", params: getHistoryParams{Customer: "acme", From: "yesterday"},
			wantErr: `parse from "yesterday" as RFC3339`},
		{name: "bad to", params: getHistoryParams{Customer: "acme", To: "soon"},
			wantErr: `parse to "soon" as RFC3339`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res, err := s.getHistory(ctx, nil, tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, messageIDs(res.Messages))
		})
	}

	t.Run("pagination by cursor", func(t *testing.T) {
		_, first, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Limit: 2})
		require.NoError(t, err)
		require.Len(t, first.Messages, 2)
		require.NotEmpty(t, first.NextCursor, "a full page offers the next one")

		_, second, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Limit: 3,
			Cursor: first.NextCursor})
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 3, 4}, messageIDs(second.Messages), "nothing repeats, nothing is skipped")

		_, third, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Limit: 3,
			Cursor: second.NextCursor})
		require.NoError(t, err)
		assert.Equal(t, []int64{1}, messageIDs(third.Messages))
		assert.Empty(t, third.NextCursor, "a page that is not full ends the walk")
	})

	t.Run("bad cursor", func(t *testing.T) {
		for _, token := range []string{"not base64 at all!", base64.RawURLEncoding.EncodeToString([]byte("nope")),
			base64.RawURLEncoding.EncodeToString([]byte("yesterday|7")),
			base64.RawURLEncoding.EncodeToString([]byte("2026-07-30T10:00:00Z|x"))} {
			_, _, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Cursor: token})
			require.Error(t, err, token)
			assert.Contains(t, err.Error(), "cursor")
		}
	})

	t.Run("full text, no snippet", func(t *testing.T) {
		_, res, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme", Limit: 1})
		require.NoError(t, err)
		require.Len(t, res.Messages, 1)
		assert.Equal(t, strings.Repeat("a", 300), res.Messages[0].Text)
		assert.Empty(t, res.Messages[0].Snippet)
	})

	t.Run("store error", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{
			HistoryFunc: func(context.Context, []int64, time.Time, time.Time, *store.HistoryCursor,
				int) ([]store.Message, error) {
				return nil, errors.New("db is gone")
			},
		})
		_, _, err := s.getHistory(ctx, nil, getHistoryParams{Customer: "acme"})
		require.ErrorContains(t, err, "read history: db is gone")
	})
}

func TestToolsGetHistoryDefaults(t *testing.T) {
	var (
		gotFrom, gotTo time.Time
		gotBefore      *store.HistoryCursor
		gotLimit       int
	)
	s := failingServer(t, &mocks.MessageStore{
		HistoryFunc: func(_ context.Context, _ []int64, from, to time.Time, before *store.HistoryCursor,
			limit int) ([]store.Message, error) {
			gotFrom, gotTo, gotBefore, gotLimit = from, to, before, limit
			return nil, nil
		},
	})

	_, _, err := s.getHistory(context.Background(), nil, getHistoryParams{Customer: "acme"})
	require.NoError(t, err)
	assert.Equal(t, historyDefaultLimit, gotLimit)
	assert.True(t, gotTo.IsZero())
	assert.WithinDuration(t, time.Now().Add(-historyWindow), gotFrom, time.Minute)

	t.Run("an explicit bound suppresses the window", func(t *testing.T) {
		to := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
		_, _, err := s.getHistory(context.Background(), nil,
			getHistoryParams{Customer: "acme", To: to.Format(time.RFC3339)})
		require.NoError(t, err)
		assert.True(t, gotFrom.IsZero(), "paginating backwards must not be capped at 24h")
		assert.Equal(t, to, gotTo.UTC())
	})

	t.Run("a cursor suppresses the window too", func(t *testing.T) {
		at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
		_, _, err := s.getHistory(context.Background(), nil, getHistoryParams{Customer: "acme",
			Cursor: encodeCursor(store.Message{Sent: at, ID: 42})})
		require.NoError(t, err)
		assert.True(t, gotFrom.IsZero())
		assert.True(t, gotTo.IsZero())
		require.NotNil(t, gotBefore)
		assert.Equal(t, store.HistoryCursor{Sent: at, ID: 42}, *gotBefore)
	})
}

func TestToolsSearch(t *testing.T) {
	s, base := seededServer(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		params  searchParams
		want    []int64
		wantErr string
	}{
		{name: "term", params: searchParams{Query: "nxagentd"}, want: []int64{3}},
		{name: "across customers", params: searchParams{Query: "outage"}, want: []int64{20}},
		{name: "newest first within a customer", params: searchParams{Query: "server", Customer: "acme"},
			want: []int64{2, 1}},
		{name: "scoped away", params: searchParams{Query: "server", Customer: "globex"}},
		{name: "limit", params: searchParams{Query: "server", Customer: "acme", Limit: 1}, want: []int64{2}},
		{name: "no match", params: searchParams{Query: "kubernetes"}},
		{name: "range drops the older hit", params: searchParams{Query: "server",
			From: base.Add(time.Minute).Format(time.RFC3339)}, want: []int64{2}},
		{name: "unknown customer", params: searchParams{Query: "server", Customer: "initech"},
			wantErr: `unknown customer "initech"`},
		{name: "bad from", params: searchParams{Query: "server", From: "recently"},
			wantErr: `parse from "recently" as RFC3339`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res, err := s.search(ctx, nil, tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, messageIDs(res.Messages))
		})
	}

	t.Run("hits carry a snippet instead of the text", func(t *testing.T) {
		_, res, err := s.search(ctx, nil, searchParams{Query: "nxagentd"})
		require.NoError(t, err)
		require.Len(t, res.Messages, 1)
		assert.Contains(t, res.Messages[0].Snippet, "nxagentd")
		assert.Empty(t, res.Messages[0].Text)
		assert.Equal(t, "acme", res.Messages[0].Customer)
	})

	t.Run("store error", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{
			SearchFunc: func(context.Context, string, []int64, time.Time, time.Time, int) ([]store.SearchHit, error) {
				return nil, errors.New("db is gone")
			},
		})
		_, _, err := s.search(ctx, nil, searchParams{Query: "boom"})
		require.ErrorContains(t, err, "search messages: db is gone")
	})
}

func TestToolsSearchDefaultLimit(t *testing.T) {
	var gotLimit int
	s := failingServer(t, &mocks.MessageStore{
		SearchFunc: func(_ context.Context, _ string, _ []int64, _, _ time.Time, limit int) ([]store.SearchHit, error) {
			gotLimit = limit
			return nil, nil
		},
	})

	_, _, err := s.search(context.Background(), nil, searchParams{Query: "boom"})
	require.NoError(t, err)
	assert.Equal(t, searchDefaultLimit, gotLimit)
}

// echoAPI answers sendMessage the way telegram does, assigning ids from 100 up. The returned
// message carries clean text: telegram answers with the parsed result, tags gone and entities
// listed separately, never with the html it was handed.
func echoAPI() *mocks.TelegramAPI {
	var next int64 = 100
	tg := fakeAPI(seedPayloads())
	tg.SendMessageFunc = func(_ context.Context, chatID int64, text, parseMode string, replyTo, threadID int64) (telegram.Message, error) {
		next++
		if parseMode == telegram.ParseModeHTML {
			text = plainOf(text)
		}
		m := telegram.Message{
			MessageID: next, MessageThreadID: threadID, Chat: telegram.Chat{ID: chatID},
			From: &telegram.User{ID: 42, IsBot: true, Username: "tg_mcp_bot", FirstName: "tg-mcp"},
			Date: time.Now().Unix(), Text: text,
		}
		if replyTo > 0 {
			m.ReplyToMessage = &telegram.Message{MessageID: replyTo}
		}
		return m, nil
	}
	return tg
}

var htmlTag = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// plainOf is what telegram's html parser leaves of a message: tags stripped, entities unescaped.
func plainOf(html string) string {
	return stdhtml.UnescapeString(htmlTag.ReplaceAllString(html, ""))
}

// badRequest is the rejection telegram returns for html it cannot parse.
func badRequest() *telegram.APIError {
	return &telegram.APIError{Method: "sendMessage", Code: http.StatusBadRequest,
		Description: "Bad Request: can't parse entities"}
}

func TestToolsSendReply(t *testing.T) {
	ctx := context.Background()

	t.Run("reply lands in the thread it answers", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", ReplyTo: 3,
			Text: "  restart the agent  "})
		require.NoError(t, err)
		assert.Empty(t, res.Warning)
		assert.Equal(t, int64(101), res.Message.MessageID)
		assert.Equal(t, "restart the agent", res.Message.Text, "surrounding whitespace is dropped")
		assert.True(t, res.Message.FromBot)
		assert.Equal(t, int64(3), res.Message.ReplyTo)
		assert.Equal(t, "acme", res.Message.Customer)

		calls := tg.SendMessageCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, int64(-1001), calls[0].ChatID)
		assert.Equal(t, "restart the agent", calls[0].Text)
		assert.Equal(t, int64(3), calls[0].ReplyTo)

		_, thread, err := s.getThread(ctx, nil, getThreadParams{Customer: "acme", MessageID: 3})
		require.NoError(t, err)
		assert.Contains(t, messageIDs(thread.Messages), int64(101),
			"the sent reply is logged, telegram never echoes it back")
	})

	t.Run("forum topic inherited from the answered message", func(t *testing.T) {
		tg := echoAPI()
		s, base := seededServerWith(t, tg)
		require.NoError(t, s.store.UpsertMessage(ctx, store.Message{ChatID: -1001, MessageID: 50,
			ThreadID: 7, Sent: base, SenderID: 11, SenderName: "alice", Text: "topic question"}))

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", ReplyTo: 50, Text: "answer"})
		require.NoError(t, err)
		assert.Equal(t, int64(7), tg.SendMessageCalls()[0].ThreadID)
		assert.Equal(t, int64(7), res.Message.ThreadID)
	})

	t.Run("bare send to a single group customer", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "we are looking into it"})
		require.NoError(t, err)
		assert.Zero(t, res.Message.ReplyTo)
		assert.Equal(t, int64(-1001), tg.SendMessageCalls()[0].ChatID)
	})

	t.Run("reply_to pins the group of a multi group customer", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "globex", ReplyTo: 20, Text: "on it"})
		require.NoError(t, err)
		assert.Equal(t, int64(-1003), tg.SendMessageCalls()[0].ChatID)
		assert.Equal(t, "escalations", res.Message.Label)
	})

	t.Run("label picks the group", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "globex", Label: "main", Text: "hi"})
		require.NoError(t, err)
		assert.Equal(t, int64(-1002), tg.SendMessageCalls()[0].ChatID)
	})

	t.Run("same message id in both groups stays ambiguous", func(t *testing.T) {
		tg := echoAPI()
		s, base := seededServerWith(t, tg)
		require.NoError(t, s.store.UpsertMessage(ctx, store.Message{ChatID: -1002, MessageID: 20,
			Sent: base, SenderID: 21, SenderName: "carol", Text: "same id, other group"}))

		var ambiguous *AmbiguousChatError
		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "globex", ReplyTo: 20, Text: "on it"})
		require.ErrorAs(t, err, &ambiguous)
		assert.Contains(t, err.Error(), "escalations, main")
		assert.Empty(t, tg.SendMessageCalls(), "nothing is sent while the group is unclear")
	})

	t.Run("errors", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)
		tests := []struct {
			name    string
			params  sendReplyParams
			wantErr string
		}{
			{name: "empty text", params: sendReplyParams{Customer: "acme", Text: "   "},
				wantErr: "reply text is empty"},
			{name: "too long", params: sendReplyParams{Customer: "acme",
				Text: strings.Repeat("ы", maxReplyRunes+1)},
				wantErr: "reply is 4097 characters, telegram caps a message at 4096: split it into several replies"},
			{name: "unknown customer", params: sendReplyParams{Customer: "initech", Text: "hi"},
				wantErr: `unknown customer "initech"`},
			{name: "ambiguous customer", params: sendReplyParams{Customer: "globex", Text: "hi"},
				wantErr: "pass label"},
			{name: "unknown label", params: sendReplyParams{Customer: "globex", Label: "billing", Text: "hi"},
				wantErr: "available labels"},
			{name: "unknown message", params: sendReplyParams{Customer: "acme", ReplyTo: 999, Text: "hi"},
				wantErr: "message not found"},
			{name: "message of another customer", params: sendReplyParams{Customer: "acme", ReplyTo: 10, Text: "hi"},
				wantErr: "message not found"},
			{name: "message in no group of the customer", params: sendReplyParams{Customer: "globex",
				ReplyTo: 1, Text: "hi"}, wantErr: "message not found"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, _, err := s.sendReply(ctx, nil, tt.params)
				require.ErrorContains(t, err, tt.wantErr)
			})
		}
		assert.Empty(t, tg.SendMessageCalls(), "a rejected reply never reaches telegram")
	})

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		s, _ := seededServerWith(t, echoAPI())
		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme",
			Text: strings.Repeat("ы", maxReplyRunes)})
		require.NoError(t, err)
	})

	t.Run("markdown rendered as html", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme",
			Text: "run **`jcmd`** & read <the> output"})
		require.NoError(t, err)

		calls := tg.SendMessageCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, "run <b><code>jcmd</code></b> &amp; read &lt;the&gt; output", calls[0].Text)
		assert.Equal(t, telegram.ParseModeHTML, calls[0].ParseMode)
		assert.Equal(t, "run jcmd & read <the> output", res.Message.Text,
			"the stored message is the text telegram rendered, not the markup")
	})

	t.Run("html rejection falls back to plain text", func(t *testing.T) {
		tg := echoAPI()
		send := tg.SendMessageFunc
		tg.SendMessageFunc = func(callCtx context.Context, chatID int64, text, parseMode string,
			replyTo, threadID int64) (telegram.Message, error) {
			if parseMode != "" {
				return telegram.Message{}, badRequest()
			}
			return send(callCtx, chatID, text, parseMode, replyTo, threadID)
		}
		s, _ := seededServerWith(t, tg)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "see *the* log"})
		require.NoError(t, err, "a 400 delivered nothing, the plain retry cannot double post")

		calls := tg.SendMessageCalls()
		require.Len(t, calls, 2)
		assert.Equal(t, "see <i>the</i> log", calls[0].Text)
		assert.Equal(t, "see *the* log", calls[1].Text, "the retry sends the markdown verbatim")
		assert.Empty(t, calls[1].ParseMode)
		assert.Equal(t, "see *the* log", res.Message.Text)
		assert.Empty(t, res.Warning, "a rescued reply is a log line, not a client warning")
	})

	t.Run("both attempts fail", func(t *testing.T) {
		plainErr := &telegram.APIError{Method: "sendMessage", Code: http.StatusBadRequest,
			Description: "Bad Request: chat not found"}
		tg := &mocks.TelegramAPI{
			SendMessageFunc: func(_ context.Context, _ int64, _, parseMode string, _, _ int64) (telegram.Message, error) {
				if parseMode != "" {
					return telegram.Message{}, badRequest()
				}
				return telegram.Message{}, plainErr
			},
		}
		s, _ := seededServerWith(t, tg)

		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "*hi*"})
		require.ErrorIs(t, err, plainErr, "the plain failure describes the send the caller asked for")
		assert.Len(t, tg.SendMessageCalls(), 2, "exactly one retry, never more")
		assert.NotContains(t, err.Error(), "1001", "chat ids never leave the server")
	})

	t.Run("non 400 is never retried", func(t *testing.T) {
		tg := &mocks.TelegramAPI{
			SendMessageFunc: func(context.Context, int64, string, string, int64, int64) (telegram.Message, error) {
				return telegram.Message{}, &telegram.APIError{Method: "sendMessage", Code: 403,
					Description: "Forbidden: bot was kicked from the group chat"}
			},
		}
		s, _ := seededServerWith(t, tg)

		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "*hi*"})
		require.ErrorContains(t, err, "bot was kicked")
		assert.Len(t, tg.SendMessageCalls(), 1, "the first attempt may have landed")
		assert.NotContains(t, err.Error(), "1001", "chat ids never leave the server")
	})

	t.Run("a transport failure is never retried", func(t *testing.T) {
		tg := &mocks.TelegramAPI{
			SendMessageFunc: func(context.Context, int64, string, string, int64, int64) (telegram.Message, error) {
				return telegram.Message{}, errors.New("dial tcp: i/o timeout")
			},
		}
		s, _ := seededServerWith(t, tg)

		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "*hi*"})
		require.ErrorContains(t, err, "i/o timeout")
		assert.Len(t, tg.SendMessageCalls(), 1, "only a 400 proves nothing was delivered")
		assert.NotContains(t, err.Error(), "1001", "chat ids never leave the server")
	})

	t.Run("length is checked on the raw text, not the html", func(t *testing.T) {
		tg := echoAPI()
		s, _ := seededServerWith(t, tg)

		// every character grows to &amp;, so the rendered message is five times the limit
		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme",
			Text: strings.Repeat("&", maxReplyRunes)})
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("&amp;", maxReplyRunes), tg.SendMessageCalls()[0].Text)
	})

	t.Run("telegram error surfaced", func(t *testing.T) {
		s, _ := seededServerWith(t, &mocks.TelegramAPI{
			SendMessageFunc: func(context.Context, int64, string, string, int64, int64) (telegram.Message, error) {
				return telegram.Message{}, &telegram.APIError{Method: "sendMessage", Code: 429,
					Description: "Too Many Requests: retry after 30", RetryAfter: 30}
			},
		})

		var apiErr *telegram.APIError
		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "hi"})
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 30, apiErr.RetryAfter)
		assert.Contains(t, err.Error(), `send reply to customer "acme"`)
	})

	t.Run("delivered but not logged", func(t *testing.T) {
		st := &mocks.MessageStore{
			UpsertMessageFunc: func(context.Context, store.Message) error { return errors.New("disk is full") },
		}
		s, err := New(Params{Store: st, Telegram: echoAPI(), Chats: testConfig(t, chatMap), AuthToken: testToken})
		require.NoError(t, err)

		_, res, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "hi"})
		require.NoError(t, err, "the message is out, resending it would double post")
		assert.Contains(t, res.Warning, "disk is full")
		assert.Equal(t, int64(101), res.Message.MessageID)
	})

	t.Run("logged even when the caller hung up", func(t *testing.T) {
		var logged store.Message
		st := &mocks.MessageStore{
			UpsertMessageFunc: func(msgCtx context.Context, m store.Message) error {
				logged = m
				return msgCtx.Err()
			},
		}
		s, err := New(Params{Store: st, Telegram: echoAPI(), Chats: testConfig(t, chatMap), AuthToken: testToken})
		require.NoError(t, err)

		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		_, res, err := s.sendReply(canceled, nil, sendReplyParams{Customer: "acme", Text: "hi"})
		require.NoError(t, err)
		assert.Empty(t, res.Warning, "the write runs on a context of its own, not the caller's")
		assert.Equal(t, int64(101), logged.MessageID, "the thread would lose its answer otherwise")
	})

	t.Run("no telegram client", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{})
		_, _, err := s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", Text: "hi"})
		require.ErrorContains(t, err, "no telegram client configured")
	})

	t.Run("lookup error", func(t *testing.T) {
		s, err := New(Params{Telegram: echoAPI(), Chats: testConfig(t, chatMap), AuthToken: testToken,
			Store: &mocks.MessageStore{
				MessageByIDFunc: func(context.Context, int64, int64) (store.Message, error) {
					return store.Message{}, errors.New("db is gone")
				},
			}})
		require.NoError(t, err)

		_, _, err = s.sendReply(ctx, nil, sendReplyParams{Customer: "acme", ReplyTo: 1, Text: "hi"})
		require.ErrorContains(t, err, "look up message 1: db is gone")

		_, _, err = s.sendReply(ctx, nil, sendReplyParams{Customer: "globex", ReplyTo: 1, Text: "hi"})
		require.ErrorContains(t, err, "look up message 1: db is gone")
	})
}

func TestSentMessage(t *testing.T) {
	tests := []struct {
		name string
		sent telegram.Message
		want store.Message
	}{
		{
			name: "as telegram returns it",
			sent: telegram.Message{MessageID: 7, Chat: telegram.Chat{ID: -1001}, Date: 1700000000,
				Text: "answer", From: &telegram.User{ID: 42, FirstName: "tg-mcp"},
				ReplyToMessage: &telegram.Message{MessageID: 3}, MessageThreadID: 9},
			want: store.Message{ChatID: -1001, MessageID: 7, ThreadID: 9,
				Sent: time.Unix(1700000000, 0).UTC(), SenderID: 42, SenderName: "tg-mcp", FromBot: true,
				ReplyTo: 3, Text: "answer"},
		},
		{
			name: "sparse response falls back to what we sent",
			sent: telegram.Message{MessageID: 7},
			want: store.Message{ChatID: -1001, MessageID: 7, SenderName: "unknown", FromBot: true,
				ReplyTo: 3, Text: "answer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sentMessage(tt.sent, -1001, 3, "answer")
			if tt.sent.Date == 0 {
				assert.WithinDuration(t, time.Now(), got.Sent, time.Minute)
				got.Sent = time.Time{}
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestToolsMarkHandled(t *testing.T) {
	ctx := context.Background()

	t.Run("cursor advances and list_new shrinks", func(t *testing.T) {
		s, _ := seededServer(t)

		_, res, err := s.markHandled(ctx, nil, markHandledParams{Customer: "acme", MessageID: 3})
		require.NoError(t, err)
		assert.Equal(t, markHandledResult{Customer: "acme", MarkedUpTo: 3}, res)

		_, list, err := s.listNew(ctx, nil, listNewParams{Customer: "acme"})
		require.NoError(t, err)
		assert.Equal(t, []int64{4, 6}, messageIDs(list.Messages))
	})

	t.Run("marking an older message keeps the cursor", func(t *testing.T) {
		s, _ := seededServer(t)
		for _, id := range []int64{4, 1} {
			_, res, err := s.markHandled(ctx, nil, markHandledParams{Customer: "acme", MessageID: id})
			require.NoError(t, err)
			assert.Equal(t, int64(4), res.MarkedUpTo, "the result reports where the cursor actually is")
		}

		_, list, err := s.listNew(ctx, nil, listNewParams{Customer: "acme"})
		require.NoError(t, err)
		assert.Equal(t, []int64{6}, messageIDs(list.Messages), "the cursor never moves backwards")
	})

	t.Run("labeled group", func(t *testing.T) {
		s, _ := seededServer(t)
		_, res, err := s.markHandled(ctx, nil, markHandledParams{Customer: "globex", Label: "main", MessageID: 10})
		require.NoError(t, err)
		assert.Equal(t, markHandledResult{Customer: "globex", Label: "main", MarkedUpTo: 10}, res)

		_, list, err := s.listNew(ctx, nil, listNewParams{Customer: "globex"})
		require.NoError(t, err)
		assert.Equal(t, []int64{20}, messageIDs(list.Messages))
	})

	t.Run("errors", func(t *testing.T) {
		s, _ := seededServer(t)
		tests := []struct {
			name    string
			params  markHandledParams
			wantErr string
		}{
			{name: "unknown customer", params: markHandledParams{Customer: "initech", MessageID: 1},
				wantErr: `unknown customer "initech"`},
			{name: "ambiguous customer", params: markHandledParams{Customer: "globex", MessageID: 10},
				wantErr: "pass label"},
			{name: "unknown message", params: markHandledParams{Customer: "acme", MessageID: 999},
				wantErr: "message not found"},
			{name: "message of another customer", params: markHandledParams{Customer: "acme", MessageID: 10},
				wantErr: "message not found"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, _, err := s.markHandled(ctx, nil, tt.params)
				require.ErrorContains(t, err, tt.wantErr)
			})
		}
	})

	t.Run("store error", func(t *testing.T) {
		s := failingServer(t, &mocks.MessageStore{
			MessageByIDFunc: func(context.Context, int64, int64) (store.Message, error) {
				return store.Message{}, nil
			},
			SetCursorFunc: func(context.Context, int64, int64) (int64, error) {
				return 0, errors.New("db is gone")
			},
		})
		_, _, err := s.markHandled(ctx, nil, markHandledParams{Customer: "acme", MessageID: 1})
		require.ErrorContains(t, err, "advance triage cursor: db is gone")
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "short enough", in: "hello", limit: 10, want: "hello"},
		{name: "exact fit", in: "hello", limit: 5, want: "hello"},
		{name: "cut", in: "hello world", limit: 5, want: "hello" + ellipsis},
		{name: "trailing space dropped", in: "hello world", limit: 6, want: "hello" + ellipsis},
		{name: "runes not bytes", in: "жабы прыгают", limit: 4, want: "жабы" + ellipsis},
		{name: "empty", in: "", limit: 5, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncate(tt.in, tt.limit))
		})
	}
}

// failingServer builds a server on a mocked store, for the paths a real store cannot produce.
func failingServer(t *testing.T, st *mocks.MessageStore) *Server {
	t.Helper()
	s, err := New(Params{Store: st, Chats: testConfig(t, chatMap), AuthToken: testToken})
	require.NoError(t, err)
	return s
}

// TestToolsDescriptions pins what the registered descriptions must not say: a byte threshold the
// caller would branch on instead of reading the result, and a bearer token the download url no
// longer needs.
func TestToolsDescriptions(t *testing.T) {
	srv := httptest.NewServer(newServer(t).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: bearerClient(testToken)}, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	res, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	descs := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		descs[tool.Name] = tool.Description
		// whole words only: "number" and "member" both carry "mb", and a substring match would
		// fail an innocent rewording with a message pointing nowhere near the cause
		assert.NotRegexp(t, `\b(bearer|tokens?|bytes?|[kmg]i?b)\b`, strings.ToLower(tool.Description),
			"tool %q description leaks a credential or a size threshold", tool.Name)
	}

	assert.Contains(t, descs["get_file"], "own credential")
	assert.Contains(t, descs["get_file"], "never send it to a customer")
	assert.Contains(t, descs["get_thread"], "inline")
	assert.Contains(t, descs["get_thread"], "get_file")
}
