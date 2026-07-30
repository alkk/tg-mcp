package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/ingest/mocks"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

const (
	allowedChat = int64(-100100)
	otherChat   = int64(-100200)
	botID       = int64(42)
	botName     = "tgbot"
)

// testConfig writes a chat map with a single allowlisted chat.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chats.yml")
	require.NoError(t, os.WriteFile(path, []byte("chats:\n  -100100:\n    customer: acme\n"), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	return cfg
}

// newService wires a service with fast retries around the given mocks.
func newService(t *testing.T, api botAPI, st messageStore) *Service {
	t.Helper()
	s := New(Params{API: api, Store: st, Chats: testConfig(t), BotID: botID, BotUsername: "@" + botName,
		PollTimeout: time.Millisecond})
	s.backoffBase, s.maxBackoff = time.Millisecond, 2*time.Millisecond
	return s
}

// scriptedAPI replays the given batches one per GetUpdates call and cancels the run once they
// are exhausted, so Run returns instead of polling forever.
func scriptedAPI(cancel context.CancelFunc, batches ...[]telegram.Update) *mocks.BotAPI {
	var mu sync.Mutex
	var call int
	return &mocks.BotAPI{
		DeleteWebhookFunc: func(_ context.Context) error { return nil },
		GetUpdatesFunc: func(_ context.Context, _ int64, _ time.Duration) ([]telegram.Update, error) {
			mu.Lock()
			defer mu.Unlock()
			if call >= len(batches) {
				cancel()
				return nil, nil
			}
			call++
			return batches[call-1], nil
		},
	}
}

// collectingStore records every batch it is handed.
func collectingStore() (*mocks.MessageStore, func() [][]store.Message) {
	var mu sync.Mutex
	var batches [][]store.Message
	st := &mocks.MessageStore{
		UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
			mu.Lock()
			defer mu.Unlock()
			batches = append(batches, msgs)
			return nil
		},
	}
	return st, func() [][]store.Message {
		mu.Lock()
		defer mu.Unlock()
		return batches
	}
}

func textUpdate(id, msgID, chatID int64, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{
		MessageID: msgID,
		Chat:      telegram.Chat{ID: chatID, Type: "supergroup", Title: "group"},
		From:      &telegram.User{ID: 7, FirstName: "Ann"},
		Date:      1700000000,
		Text:      text,
	}}
}

// runOnce drives Run over the scripted batches and returns its error.
func runOnce(t *testing.T, st messageStore, batches ...[]telegram.Update) (*mocks.BotAPI, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	api := scriptedAPI(cancel, batches...)
	return api, newService(t, api, st).Run(ctx)
}

func TestServiceFilters(t *testing.T) {
	mention := textUpdate(2, 11, allowedChat, "hey @tgbot look")
	mention.Message.Entities = []telegram.Entity{{Type: "mention", Offset: 4, Length: 6}}

	replyToBot := textUpdate(3, 12, allowedChat, "thanks")
	replyToBot.Message.ReplyToMessage = &telegram.Message{
		MessageID: 5, From: &telegram.User{ID: botID, IsBot: true, Username: botName}}

	service := telegram.Update{UpdateID: 4, Message: &telegram.Message{
		MessageID: 13, Chat: telegram.Chat{ID: allowedChat}, Date: 1700000000}}

	migrate := telegram.Update{UpdateID: 5, Message: &telegram.Message{
		MessageID: 14, Chat: telegram.Chat{ID: allowedChat}, Date: 1700000000, MigrateToChatID: -100999}}

	photo := telegram.Update{UpdateID: 6, Message: &telegram.Message{
		MessageID: 15,
		Chat:      telegram.Chat{ID: allowedChat},
		From:      &telegram.User{ID: 7, FirstName: "Ann"},
		Date:      1700000000,
		Caption:   "screenshot",
		Photo: []telegram.PhotoSize{
			{FileID: "small", FileUniqueID: "u1", Width: 10, Height: 10, FileSize: 100},
			{FileID: "big", FileUniqueID: "u2", Width: 100, Height: 100, FileSize: 900},
		},
	}}

	// no caption and no text: only the attachment keeps it from looking like a service message
	sticker := telegram.Update{UpdateID: 7, Message: &telegram.Message{
		MessageID: 16,
		Chat:      telegram.Chat{ID: allowedChat},
		From:      &telegram.User{ID: 7, FirstName: "Ann"},
		Date:      1700000000,
		Sticker:   &telegram.Sticker{FileID: "s", FileUniqueID: "u3", Emoji: "👍", FileSize: 7},
	}}

	audio := telegram.Update{UpdateID: 8, Message: &telegram.Message{
		MessageID: 17,
		Chat:      telegram.Chat{ID: allowedChat},
		From:      &telegram.User{ID: 7, FirstName: "Ann"},
		Date:      1700000000,
		Audio:     &telegram.Audio{FileID: "a", FileUniqueID: "u4", FileName: "call.m4a", FileSize: 42},
	}}

	tbl := []struct {
		name   string
		update telegram.Update
		want   *store.Message
	}{
		{"plain message stored", textUpdate(1, 10, allowedChat, "hello"),
			&store.Message{ChatID: allowedChat, MessageID: 10, SenderID: 7, SenderName: "Ann", Text: "hello"}},
		{"chat outside allowlist dropped", textUpdate(1, 10, otherChat, "hello"), nil},
		{"mention flagged", mention,
			&store.Message{ChatID: allowedChat, MessageID: 11, SenderID: 7, SenderName: "Ann",
				Text: "hey @tgbot look", IsMention: true}},
		{"reply to bot flagged", replyToBot,
			&store.Message{ChatID: allowedChat, MessageID: 12, SenderID: 7, SenderName: "Ann",
				Text: "thanks", ReplyTo: 5, IsMention: true}},
		{"update without a message dropped", telegram.Update{UpdateID: 9}, nil},
		{"service message dropped", service, nil},
		{"migration dropped", migrate, nil},
		{"media stored with the largest photo", photo,
			&store.Message{ChatID: allowedChat, MessageID: 15, SenderID: 7, SenderName: "Ann",
				Text: "screenshot", MediaType: "photo", FileID: "big", FileUniqueID: "u2",
				FileName: "u2.jpg", FileSize: 900}},
		{"caption-less sticker kept", sticker,
			&store.Message{ChatID: allowedChat, MessageID: 16, SenderID: 7, SenderName: "Ann",
				MediaType: "sticker", FileID: "s", FileUniqueID: "u3", FileName: "u3.webp", FileSize: 7}},
		{"caption-less audio kept", audio,
			&store.Message{ChatID: allowedChat, MessageID: 17, SenderID: 7, SenderName: "Ann",
				MediaType: "audio", FileID: "a", FileUniqueID: "u4", FileName: "call.m4a", FileSize: 42}},
	}

	for _, tt := range tbl {
		t.Run(tt.name, func(t *testing.T) {
			st, batches := collectingStore()
			_, err := runOnce(t, st, []telegram.Update{tt.update})
			require.NoError(t, err)

			if tt.want == nil {
				assert.Empty(t, batches(), "update should have been filtered out")
				return
			}
			require.Len(t, batches(), 1)
			require.Len(t, batches()[0], 1)

			got := batches()[0][0]
			assert.Equal(t, time.Unix(1700000000, 0).UTC(), got.Sent)
			got.Sent = time.Time{}
			assert.Equal(t, *tt.want, got)
		})
	}
}

func TestServiceAnonymousSender(t *testing.T) {
	u := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID:  10,
		Chat:       telegram.Chat{ID: allowedChat},
		SenderChat: &telegram.Chat{ID: allowedChat, Title: "Acme Group"},
		Date:       1700000000,
		Text:       "posted anonymously",
	}}

	st, batches := collectingStore()
	_, err := runOnce(t, st, []telegram.Update{u})
	require.NoError(t, err)

	require.Len(t, batches(), 1)
	assert.Equal(t, int64(0), batches()[0][0].SenderID)
	assert.Equal(t, "Acme Group", batches()[0][0].SenderName)
}

func TestServiceEditedMessage(t *testing.T) {
	edited := telegram.Update{UpdateID: 2, EditedMessage: &telegram.Message{
		MessageID: 10,
		Chat:      telegram.Chat{ID: allowedChat},
		From:      &telegram.User{ID: 7, FirstName: "Ann"},
		Date:      1700000000,
		EditDate:  1700000600,
		Text:      "hello, fixed",
	}}

	st, batches := collectingStore()
	_, err := runOnce(t, st,
		[]telegram.Update{textUpdate(1, 10, allowedChat, "hello")},
		[]telegram.Update{edited})
	require.NoError(t, err)

	require.Len(t, batches(), 2)
	got := batches()[1][0]
	assert.Equal(t, int64(10), got.MessageID, "edit goes through the same upsert path")
	assert.Equal(t, "hello, fixed", got.Text)
	assert.Equal(t, time.Unix(1700000600, 0).UTC(), got.EditedAt)
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), got.Sent)
}

func TestServiceBatchIsOneTransaction(t *testing.T) {
	st, batches := collectingStore()
	api, err := runOnce(t, st, []telegram.Update{
		textUpdate(7, 10, allowedChat, "one"),
		textUpdate(8, 11, otherChat, "not ours"),
		textUpdate(9, 12, allowedChat, "two"),
	})
	require.NoError(t, err)

	require.Len(t, batches(), 1, "the whole batch is stored in a single call")
	assert.Len(t, batches()[0], 2)

	calls := api.GetUpdatesCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, int64(0), calls[0].Offset)
	assert.Equal(t, int64(10), calls[1].Offset, "offset is last update id + 1")
}

func TestServiceOffsetHeldUntilStored(t *testing.T) {
	var mu sync.Mutex
	var calls int
	st := &mocks.MessageStore{UpsertBatchFunc: func(_ context.Context, _ []store.Message) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("disk on fire")
		}
		return nil
	}}

	batch := []telegram.Update{textUpdate(7, 10, allowedChat, "hello")}
	api, err := runOnce(t, st, batch, batch)
	require.NoError(t, err)

	got := api.GetUpdatesCalls()
	require.Len(t, got, 3)
	assert.Equal(t, int64(0), got[0].Offset)
	assert.Equal(t, int64(0), got[1].Offset, "failed store must not advance the offset")
	assert.Equal(t, int64(8), got[2].Offset)
}

func TestServicePoisonSkippedAfterRetries(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	st := &mocks.MessageStore{UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(msgs))
		if len(msgs) == 1 && msgs[0].MessageID == 11 {
			return nil // only the second message of the batch is poison
		}
		return fmt.Errorf("upsert message: %w", store.ErrBadMessage)
	}}

	batch := []telegram.Update{
		textUpdate(7, 10, allowedChat, "poison"),
		textUpdate(8, 11, allowedChat, "innocent"),
	}
	api, err := runOnce(t, st, batch, batch, batch)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{2, 2, 2, 1, 1}, sizes, "three batch attempts, then message by message")

	got := api.GetUpdatesCalls()
	require.Len(t, got, 4)
	assert.Equal(t, []int64{0, 0, 0}, []int64{got[0].Offset, got[1].Offset, got[2].Offset})
	assert.Equal(t, int64(9), got[3].Offset, "offset advances past the poison update")
}

func TestServicePoisonSkippedWhenRedeliveryGrows(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	st := &mocks.MessageStore{UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(msgs))
		if len(msgs) == 1 && msgs[0].MessageID != 10 {
			return nil // only the first message of the batch is poison
		}
		return fmt.Errorf("upsert message: %w", store.ErrBadMessage)
	}}

	// a talkative chat: the redelivery repeats the unconfirmed updates and appends the new ones
	poison := textUpdate(7, 10, allowedChat, "poison")
	first := []telegram.Update{poison}
	second := []telegram.Update{poison, textUpdate(8, 11, allowedChat, "innocent")}
	third := append(append([]telegram.Update{}, second...), textUpdate(9, 12, allowedChat, "later"))

	api, err := runOnce(t, st, first, second, third)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{1, 2, 3, 1, 1, 1}, sizes,
		"the retry counter must survive a growing batch and reach the per-message skip")

	got := api.GetUpdatesCalls()
	require.Len(t, got, 4)
	assert.Equal(t, []int64{0, 0, 0}, []int64{got[0].Offset, got[1].Offset, got[2].Offset})
	assert.Equal(t, int64(10), got[3].Offset, "offset advances past the poison update")
}

func TestServiceDatabaseOutageNeverDrops(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	st := &mocks.MessageStore{UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(msgs))
		return errors.New("disk full") // not a rejected message: the database itself is unusable
	}}

	batch := []telegram.Update{
		textUpdate(7, 10, allowedChat, "hello"),
		textUpdate(8, 11, allowedChat, "again"),
	}
	api, err := runOnce(t, st, batch, batch, batch, batch, batch)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{2, 2, 2, 2, 2}, sizes, "the batch is never replayed message by message")

	for i, call := range api.GetUpdatesCalls() {
		assert.Equal(t, int64(0), call.Offset, "call %d must not advance past unstored messages", i)
	}
}

func TestServiceReplayAbortsWhenDatabaseGoesDown(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	st := &mocks.MessageStore{UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(msgs))
		if len(msgs) == 1 && msgs[0].MessageID == 11 {
			return errors.New("disk full") // the database dies halfway through the replay
		}
		return fmt.Errorf("upsert message: %w", store.ErrBadMessage)
	}}

	batch := []telegram.Update{
		textUpdate(7, 10, allowedChat, "poison"),
		textUpdate(8, 11, allowedChat, "innocent"),
	}
	api, err := runOnce(t, st, batch, batch, batch)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{2, 2, 2, 1, 1}, sizes,
		"the replay stops at the message the database could not take")

	for i, call := range api.GetUpdatesCalls() {
		assert.Equal(t, int64(0), call.Offset, "call %d must not advance on an aborted replay", i)
	}
}

func TestServiceConflictFailsFast(t *testing.T) {
	api := &mocks.BotAPI{
		DeleteWebhookFunc: func(_ context.Context) error { return nil },
		GetUpdatesFunc: func(_ context.Context, _ int64, _ time.Duration) ([]telegram.Update, error) {
			return nil, &telegram.APIError{Method: "getUpdates", Code: http.StatusConflict,
				Description: "terminated by other getUpdates request"}
		},
	}
	st, _ := collectingStore()

	err := newService(t, api, st).Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a webhook or another poller is active")
	assert.Len(t, api.GetUpdatesCalls(), 1, "no retry on 409")
}

func TestServiceRejectedTokenFailsFast(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			api := &mocks.BotAPI{
				DeleteWebhookFunc: func(_ context.Context) error { return nil },
				GetUpdatesFunc: func(_ context.Context, _ int64, _ time.Duration) ([]telegram.Update, error) {
					return nil, &telegram.APIError{Method: "getUpdates", Code: code, Description: "Unauthorized"}
				},
			}
			st, _ := collectingStore()

			err := newService(t, api, st).Run(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the bot token is invalid or revoked")
			assert.Len(t, api.GetUpdatesCalls(), 1, "backing off forever would stall ingest silently")
		})
	}
}

func TestServiceDeleteWebhookFails(t *testing.T) {
	api := &mocks.BotAPI{
		DeleteWebhookFunc: func(_ context.Context) error { return errors.New("unauthorized") },
	}
	st, _ := collectingStore()

	err := newService(t, api, st).Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete webhook")
	assert.Empty(t, api.GetUpdatesCalls())
}

func TestServicePollBackoffAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var calls int
	api := &mocks.BotAPI{
		DeleteWebhookFunc: func(_ context.Context) error { return nil },
		GetUpdatesFunc: func(_ context.Context, _ int64, _ time.Duration) ([]telegram.Update, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls >= 3 {
				cancel()
			}
			return nil, errors.New("connection reset")
		},
	}
	st, batches := collectingStore()

	require.NoError(t, newService(t, api, st).Run(ctx), "cancellation is a clean shutdown")
	assert.GreaterOrEqual(t, len(api.GetUpdatesCalls()), 3, "transient poll errors are retried")
	assert.Empty(t, batches())
}

func TestServiceEmptyBatchKeepsOffset(t *testing.T) {
	st, batches := collectingStore()
	api, err := runOnce(t, st, nil, []telegram.Update{textUpdate(3, 10, allowedChat, "hi")})
	require.NoError(t, err)

	require.Len(t, batches(), 1)
	got := api.GetUpdatesCalls()
	require.Len(t, got, 3)
	assert.Equal(t, int64(0), got[1].Offset, "an empty poll leaves the offset alone")
	assert.Equal(t, int64(4), got[2].Offset)
}

func TestServiceFilteredBatchAdvancesOffset(t *testing.T) {
	st, batches := collectingStore()
	api, err := runOnce(t, st, []telegram.Update{textUpdate(7, 10, otherChat, "not ours")})
	require.NoError(t, err)

	assert.Empty(t, batches(), "nothing to store")
	got := api.GetUpdatesCalls()
	require.Len(t, got, 2)
	assert.Equal(t, int64(8), got[1].Offset, "a fully filtered batch still confirms the updates")
}

func TestServiceOwnMessageFlagged(t *testing.T) {
	u := textUpdate(1, 10, allowedChat, "our reply")
	u.Message.From = &telegram.User{ID: botID, IsBot: true, Username: botName}

	st, batches := collectingStore()
	_, err := runOnce(t, st, []telegram.Update{u})
	require.NoError(t, err)

	require.Len(t, batches(), 1)
	assert.True(t, batches()[0][0].FromBot)
}

func TestNewDefaults(t *testing.T) {
	s := New(Params{Chats: testConfig(t), BotUsername: "@bot"})
	assert.Equal(t, defaultPollTimeout, s.pollTimeout)
	assert.Equal(t, "bot", s.botUsername)
	assert.Equal(t, storeRetries, s.retries)
}

func TestServiceBackoff(t *testing.T) {
	s := New(Params{Chats: testConfig(t)})
	assert.Equal(t, time.Second, s.backoff(1))
	assert.Equal(t, 4*time.Second, s.backoff(3))
	assert.Equal(t, maxBackoff, s.backoff(20))
}

func TestServiceWaitCanceled(t *testing.T) {
	s := New(Params{Chats: testConfig(t)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wait(ctx, time.Minute)
		s.wait(ctx, 0)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait ignored the canceled context")
	}
}

func TestServiceIsBotByUsername(t *testing.T) {
	s := New(Params{Chats: testConfig(t), BotUsername: botName}) // no id known

	assert.True(t, s.isBot(&telegram.User{ID: 9, IsBot: true, Username: "TgBot"}))
	assert.False(t, s.isBot(&telegram.User{ID: 9, IsBot: false, Username: botName}))
	assert.False(t, s.isBot(nil))
	assert.False(t, New(Params{Chats: testConfig(t)}).isBot(&telegram.User{ID: 9, IsBot: true}))
}

func TestPayload(t *testing.T) {
	assert.JSONEq(t, `{"update_id":1,"message":null,"edited_message":null}`,
		payload(telegram.Update{UpdateID: 1}))
}
