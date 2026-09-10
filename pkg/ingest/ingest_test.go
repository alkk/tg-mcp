package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	allowedChat  = int64(-100100)
	otherChat    = int64(-100200)
	thirdChat    = int64(-100300)
	strangerChat = int64(555)
	botID        = int64(42)
	botName      = "tgbot"
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

// captureLogs points the default logger at a buffer for the duration of the test. Run and
// everything logging under it are single-goroutine, so a plain buffer is enough.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// logLines returns the captured lines carrying the given message.
func logLines(logs, msg string) []string {
	var got []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			got = append(got, line)
		}
	}
	return got
}

// storeCall is one store write as ingest issued it, tagged by the table it targeted.
type storeCall struct {
	pend bool
	msgs []store.Message
	pnd  []store.Pending
}

func (c storeCall) ids() []int64 {
	got := make([]int64, 0, len(c.msgs)+len(c.pnd))
	for _, m := range c.msgs {
		got = append(got, m.MessageID)
	}
	for _, m := range c.pnd {
		got = append(got, m.MessageID)
	}
	return got
}

// recordingStore records every call in order and lets each table decide what to return.
type recordingStore struct {
	mu     sync.Mutex
	calls  []storeCall
	onMsgs func([]store.Message) error
	onPend func([]store.Pending) error
}

func (r *recordingStore) mock() *mocks.MessageStore {
	return &mocks.MessageStore{
		UpsertBatchFunc: func(_ context.Context, msgs []store.Message) error {
			r.mu.Lock()
			r.calls = append(r.calls, storeCall{msgs: msgs})
			r.mu.Unlock()
			if r.onMsgs == nil {
				return nil
			}
			return r.onMsgs(msgs)
		},
		UpsertPendingBatchFunc: func(_ context.Context, pnd []store.Pending) error {
			r.mu.Lock()
			r.calls = append(r.calls, storeCall{pend: true, pnd: pnd})
			r.mu.Unlock()
			if r.onPend == nil {
				return nil
			}
			return r.onPend(pnd)
		},
	}
}

func (r *recordingStore) log() []storeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]storeCall{}, r.calls...)
}

// msgBatches and pendBatches are the call log split by target table, for the tests that assert
// what went into a batch rather than in which order the batches were issued.
func (r *recordingStore) msgBatches() [][]store.Message {
	var got [][]store.Message
	for _, c := range r.log() {
		if !c.pend {
			got = append(got, c.msgs)
		}
	}
	return got
}

func (r *recordingStore) pendBatches() [][]store.Pending {
	var got [][]store.Pending
	for _, c := range r.log() {
		if c.pend {
			got = append(got, c.pnd)
		}
	}
	return got
}

// shape renders the call log as "table:ids" strings, which is what the mixed-batch tests assert.
func (r *recordingStore) shape() []string {
	calls := r.log()
	got := make([]string, 0, len(calls))
	for _, c := range calls {
		table := "messages"
		if c.pend {
			table = "pending"
		}
		got = append(got, fmt.Sprintf("%s:%v", table, c.ids()))
	}
	return got
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
		name     string
		update   telegram.Update
		want     *store.Message
		buffered bool // want goes to the pending buffer instead of the messages table
	}{
		{name: "plain message stored", update: textUpdate(1, 10, allowedChat, "hello"),
			want: &store.Message{ChatID: allowedChat, MessageID: 10, SenderID: 7, SenderName: "Ann",
				Text: "hello"}},
		{name: "chat outside allowlist buffered", update: textUpdate(1, 10, otherChat, "hello"),
			want: &store.Message{ChatID: otherChat, MessageID: 10, SenderID: 7, SenderName: "Ann",
				Text: "hello"},
			buffered: true},
		{name: "mention flagged", update: mention,
			want: &store.Message{ChatID: allowedChat, MessageID: 11, SenderID: 7, SenderName: "Ann",
				Text: "hey @tgbot look", IsMention: true}},
		{name: "reply to bot flagged", update: replyToBot,
			want: &store.Message{ChatID: allowedChat, MessageID: 12, SenderID: 7, SenderName: "Ann",
				Text: "thanks", ReplyTo: 5, IsMention: true}},
		{name: "update without a message dropped", update: telegram.Update{UpdateID: 9}},
		{name: "service message dropped", update: service},
		{name: "migration dropped", update: migrate},
		{name: "media stored with the largest photo", update: photo,
			want: &store.Message{ChatID: allowedChat, MessageID: 15, SenderID: 7, SenderName: "Ann",
				Text: "screenshot", MediaType: "photo", FileID: "big", FileUniqueID: "u2",
				FileName: "u2.jpg", FileSize: 900}},
		{name: "caption-less sticker kept", update: sticker,
			want: &store.Message{ChatID: allowedChat, MessageID: 16, SenderID: 7, SenderName: "Ann",
				MediaType: "sticker", FileID: "s", FileUniqueID: "u3", FileName: "u3.webp", FileSize: 7}},
		{name: "caption-less audio kept", update: audio,
			want: &store.Message{ChatID: allowedChat, MessageID: 17, SenderID: 7, SenderName: "Ann",
				MediaType: "audio", FileID: "a", FileUniqueID: "u4", FileName: "call.m4a", FileSize: 42}},
	}

	for _, tt := range tbl {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingStore{}
			st := rec.mock()
			_, err := runOnce(t, st, []telegram.Update{tt.update})
			require.NoError(t, err)

			if tt.buffered {
				assert.Empty(t, rec.msgBatches(), "a chat outside the allowlist never reaches the messages table")
				require.Len(t, rec.pendBatches(), 1)
				require.Len(t, rec.pendBatches()[0], 1)

				got := rec.pendBatches()[0][0]
				assert.Equal(t, time.Unix(1700000000, 0).UTC(), got.Sent)
				assert.False(t, got.Received.IsZero(), "the hold clock starts at the buffered write")
				got.Sent, got.Received = time.Time{}, time.Time{}
				assert.Equal(t, store.Pending{Message: *tt.want, ChatTitle: "group", ChatType: "supergroup"}, got)
				return
			}
			if tt.want == nil {
				assert.Empty(t, rec.msgBatches(), "update should have been filtered out")
				assert.Empty(t, rec.pendBatches(), "and not buffered either")
				return
			}
			require.Len(t, rec.msgBatches(), 1)
			require.Len(t, rec.msgBatches()[0], 1)
			assert.Empty(t, rec.pendBatches(), "an allowlisted chat is never buffered")

			got := rec.msgBatches()[0][0]
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

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{u})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 1)
	assert.Equal(t, int64(0), rec.msgBatches()[0][0].SenderID)
	assert.Equal(t, "Acme Group", rec.msgBatches()[0][0].SenderName)
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

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st,
		[]telegram.Update{textUpdate(1, 10, allowedChat, "hello")},
		[]telegram.Update{edited})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 2)
	got := rec.msgBatches()[1][0]
	assert.Equal(t, int64(10), got.MessageID, "edit goes through the same upsert path")
	assert.Equal(t, "hello, fixed", got.Text)
	assert.Equal(t, time.Unix(1700000600, 0).UTC(), got.EditedAt)
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), got.Sent)
}

func TestServiceBatchIsOneTransaction(t *testing.T) {
	rec := &recordingStore{}
	st := rec.mock()
	api, err := runOnce(t, st, []telegram.Update{
		textUpdate(7, 10, allowedChat, "one"),
		textUpdate(8, 11, otherChat, "not ours"),
		textUpdate(9, 12, allowedChat, "two"),
	})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 1, "the whole batch is stored in a single call")
	assert.Len(t, rec.msgBatches()[0], 2)
	require.Len(t, rec.pendBatches(), 1, "and buffered in a single call of its own")
	assert.Len(t, rec.pendBatches()[0], 1)

	calls := api.GetUpdatesCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, int64(0), calls[0].Offset)
	assert.Equal(t, int64(10), calls[1].Offset, "offset is last update id + 1")
}

func TestServiceNoPendingWriteWhenAllChatsAllowlisted(t *testing.T) {
	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{
		textUpdate(1, 10, allowedChat, "one"),
		textUpdate(2, 11, allowedChat, "two"),
	})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 1)
	assert.Len(t, rec.msgBatches()[0], 2)
	assert.Empty(t, rec.pendBatches(), "nothing is buffered while every chat is in the allowlist")
	assert.Empty(t, st.UpsertPendingBatchCalls(), "the pending store is not called at all")
}

func TestServiceUnknownChatAnnouncedWithoutAnythingToBuffer(t *testing.T) {
	logs := captureLogs(t)

	// the bot was just added to a group and nobody has spoken: the join event is all there is
	join := telegram.Update{UpdateID: 3, Message: &telegram.Message{
		MessageID: 10,
		Chat:      telegram.Chat{ID: otherChat, Type: "group", Title: "New Group"},
		Date:      1700000000,
	}}

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{join})
	require.NoError(t, err)

	assert.Empty(t, rec.msgBatches(), "a service message carries nothing worth storing")
	assert.Empty(t, rec.pendBatches(), "or buffering")

	lines := logLines(logs(), "buffering messages from a chat outside the allowlist")
	require.Len(t, lines, 1, "a quiet group must still announce its chat id")
	assert.Contains(t, lines[0], "level=INFO")
	assert.Contains(t, lines[0], "chat_id=-100200")
	assert.Contains(t, lines[0], `title="New Group"`)
	assert.Contains(t, lines[0], "type=group")
}

func TestServiceBufferedMessageCarriesChatIdentity(t *testing.T) {
	u := textUpdate(1, 10, otherChat, "we have a problem")
	u.Message.Chat.Title, u.Message.Chat.Type = "Wegagen NetXMS Support", "group"

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{u})
	require.NoError(t, err)

	require.Len(t, rec.pendBatches(), 1)
	require.Len(t, rec.pendBatches()[0], 1)
	got := rec.pendBatches()[0][0]
	assert.Equal(t, "Wegagen NetXMS Support", got.ChatTitle)
	assert.Equal(t, "group", got.ChatType)
	assert.Equal(t, otherChat, got.ChatID)
	assert.Equal(t, "we have a problem", got.Text)
}

func TestServicePrivateChatNotBuffered(t *testing.T) {
	logs := captureLogs(t)

	dm := textUpdate(1, 10, strangerChat, "hi bot")
	dm.Message.Chat.Type, dm.Message.Chat.Title = "private", ""

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{dm})
	require.NoError(t, err)

	assert.Empty(t, rec.msgBatches())
	assert.Empty(t, rec.pendBatches(), "a direct message has no chats.yml line waiting for it")

	assert.Empty(t, logLines(logs(), "buffering messages from a chat outside the allowlist"))
	lines := logLines(logs(), "dropping messages from a chat outside the allowlist")
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "type=private")
}

func TestServiceMigratedUnknownChatNeitherStoredNorBuffered(t *testing.T) {
	logs := captureLogs(t)

	migrate := telegram.Update{UpdateID: 4, Message: &telegram.Message{
		MessageID: 10, Chat: telegram.Chat{ID: otherChat, Title: "Old Group"}, Date: 1700000000,
		MigrateToChatID: -100999, Text: "hello"}}

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{migrate})
	require.NoError(t, err)

	assert.Empty(t, rec.msgBatches())
	assert.Empty(t, rec.pendBatches(), "buffering it would hold messages under an id that no longer exists")
	assert.Len(t, logLines(logs(), "chat migrated to a supergroup, update the chat map"), 1)
	assert.Empty(t, logLines(logs(), "buffering messages from a chat outside the allowlist"))
}

func TestServiceUnknownChatAnnouncedOncePerRun(t *testing.T) {
	logs := captureLogs(t)

	first := make([]telegram.Update, 0, 5)
	for i := range int64(5) {
		first = append(first, textUpdate(10+i, 100+i, otherChat, "chatter"))
	}

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, first, []telegram.Update{textUpdate(20, 200, otherChat, "more")})
	require.NoError(t, err)

	require.Len(t, rec.pendBatches(), 2)
	assert.Len(t, rec.pendBatches()[0], 5, "every message is buffered even though only one is logged")
	assert.Len(t, logLines(logs(), "buffering messages from a chat outside the allowlist"), 1)
}

func TestServiceEveryUnknownChatAnnouncedWithItsOwnID(t *testing.T) {
	logs := captureLogs(t)

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{
		textUpdate(1, 10, otherChat, "first stranger"),
		textUpdate(2, 20, thirdChat, "second stranger"),
		textUpdate(3, 30, otherChat, "first stranger again"),
	})
	require.NoError(t, err)

	require.Len(t, rec.pendBatches(), 1)
	assert.Len(t, rec.pendBatches()[0], 3)

	lines := logLines(logs(), "buffering messages from a chat outside the allowlist")
	require.Len(t, lines, 2, "the announcement is keyed per chat, not per run")
	assert.Contains(t, lines[0], "chat_id=-100200")
	assert.Contains(t, lines[1], "chat_id=-100300")
}

func TestServicePendingWriteFailureHoldsOffset(t *testing.T) {
	rec := &recordingStore{}
	var calls int
	rec.onPend = func([]store.Pending) error {
		calls++
		if calls == 1 {
			return errors.New("disk on fire")
		}
		return nil
	}

	batch := []telegram.Update{textUpdate(7, 10, otherChat, "hello")}
	api, err := runOnce(t, rec.mock(), batch, batch)
	require.NoError(t, err)

	assert.Equal(t, []string{"pending:[10]", "pending:[10]"}, rec.shape())
	got := api.GetUpdatesCalls()
	require.Len(t, got, 3)
	assert.Equal(t, int64(0), got[1].Offset, "a failed buffered write must not advance the offset")
	assert.Equal(t, int64(8), got[2].Offset)
}

func TestServiceSkipPoisonRoutesEachRecordToItsTable(t *testing.T) {
	rec := &recordingStore{}
	recs := []record{
		{update: textUpdate(1, 10, allowedChat, "ours"),
			msg: store.Message{ChatID: allowedChat, MessageID: 10, Text: "ours"}},
		{update: textUpdate(2, 11, otherChat, "theirs"),
			msg:     store.Message{ChatID: otherChat, MessageID: 11, Text: "theirs"},
			unknown: &chatRef{title: "New Group", kind: "group"}},
	}

	s := newService(t, nil, rec.mock())
	assert.True(t, s.skipPoison(context.Background(), recs))

	assert.Equal(t, []string{"messages:[10]", "pending:[11]"}, rec.shape())
	buffered := rec.log()[1].pnd[0]
	assert.Equal(t, "New Group", buffered.ChatTitle)
	assert.Equal(t, "group", buffered.ChatType)
}

// mixedBatch is one delivery carrying two allowlisted messages and one from an unknown chat,
// which is the case the two store calls in persist actually change.
func mixedBatch(round string) []telegram.Update {
	return []telegram.Update{
		textUpdate(7, 10, allowedChat, "a "+round),
		textUpdate(8, 11, otherChat, "p "+round),
		textUpdate(9, 12, allowedChat, "b "+round),
	}
}

func TestServiceMixedBatch(t *testing.T) {
	t.Run("messages write fails transiently, the redelivery lands", func(t *testing.T) {
		rec := &recordingStore{}
		var msgs int
		rec.onMsgs = func([]store.Message) error {
			msgs++
			if msgs == 1 {
				return errors.New("disk on fire")
			}
			return nil
		}

		api, err := runOnce(t, rec.mock(), mixedBatch("first"), mixedBatch("second"))
		require.NoError(t, err)

		assert.Equal(t, []string{"pending:[11]", "messages:[10 12]", "pending:[11]", "messages:[10 12]"},
			rec.shape(), "buffered rows go first, both calls repeat on the redelivery")

		calls := rec.log()
		assert.Equal(t, "p second", calls[2].pnd[0].Text, "the latest content wins for a repeated key")
		assert.Equal(t, "a second", calls[3].msgs[0].Text)
		// received is stamped once per batch; the store keeps the first one it saw for a key, so a
		// duplicate write does not restart the hold clock (pkg/store/pending_test.go)
		assert.False(t, calls[0].pnd[0].Received.IsZero())

		got := api.GetUpdatesCalls()
		require.Len(t, got, 3)
		assert.Equal(t, []int64{0, 0, 10}, []int64{got[0].Offset, got[1].Offset, got[2].Offset})
	})

	t.Run("poison message skipped while the buffered row survives", func(t *testing.T) {
		rec := &recordingStore{}
		rec.onMsgs = func(msgs []store.Message) error {
			if len(msgs) == 1 && msgs[0].MessageID != 12 {
				return nil // only the last message of the batch is poison
			}
			return fmt.Errorf("upsert message: %w", store.ErrBadMessage)
		}

		batch := mixedBatch("only")
		api, err := runOnce(t, rec.mock(), batch, batch, batch)
		require.NoError(t, err)

		assert.Equal(t, []string{
			"pending:[11]", "messages:[10 12]",
			"pending:[11]", "messages:[10 12]",
			"pending:[11]", "messages:[10 12]",
			"messages:[10]", "pending:[11]", "messages:[12]",
		}, rec.shape(), "a successful pending call must not reset the retry counter")

		got := api.GetUpdatesCalls()
		require.Len(t, got, 4)
		assert.Equal(t, []int64{0, 0, 0}, []int64{got[0].Offset, got[1].Offset, got[2].Offset})
		assert.Equal(t, int64(10), got[3].Offset, "offset advances past the poison update")
	})

	t.Run("poison buffered row skipped while the messages land", func(t *testing.T) {
		rec := &recordingStore{}
		rec.onPend = func([]store.Pending) error {
			return fmt.Errorf("upsert pending message: %w", store.ErrBadMessage)
		}

		batch := mixedBatch("only")
		api, err := runOnce(t, rec.mock(), batch, batch, batch)
		require.NoError(t, err)

		assert.Equal(t, []string{
			"pending:[11]", "pending:[11]", "pending:[11]",
			"messages:[10]", "pending:[11]", "messages:[12]",
		}, rec.shape(), "the messages call is never reached until the replay splits the batch")

		calls := rec.log()
		assert.Equal(t, "a only", calls[3].msgs[0].Text)
		assert.Equal(t, "b only", calls[5].msgs[0].Text)

		got := api.GetUpdatesCalls()
		require.Len(t, got, 4)
		assert.Equal(t, []int64{0, 0, 0}, []int64{got[0].Offset, got[1].Offset, got[2].Offset})
		assert.Equal(t, int64(10), got[3].Offset)
	})

	t.Run("outage during the replay pins the offset", func(t *testing.T) {
		rec := &recordingStore{}
		var pend int
		rec.onPend = func([]store.Pending) error {
			pend++
			if pend <= 3 {
				return fmt.Errorf("upsert pending message: %w", store.ErrBadMessage)
			}
			return errors.New("disk full") // the database dies halfway through the replay
		}

		batch := mixedBatch("only")
		api, err := runOnce(t, rec.mock(), batch, batch, batch, batch)
		require.NoError(t, err)

		assert.Equal(t, []string{
			"pending:[11]", "pending:[11]", "pending:[11]",
			"messages:[10]", "pending:[11]",
			"pending:[11]",
		}, rec.shape(), "the replay stops at the write the database could not take")

		for i, call := range api.GetUpdatesCalls() {
			assert.Equal(t, int64(0), call.Offset,
				"call %d must not advance despite the singleton write that already committed", i)
		}
	})
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
	rec := &recordingStore{}
	st := rec.mock()

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
			rec := &recordingStore{}
			st := rec.mock()

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
	rec := &recordingStore{}
	st := rec.mock()

	err := newService(t, api, st).Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete webhook")
	assert.Empty(t, api.GetUpdatesCalls())
}

func TestServiceDeleteWebhookCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &mocks.BotAPI{
		DeleteWebhookFunc: func(callCtx context.Context) error {
			cancel()
			return callCtx.Err()
		},
	}
	rec := &recordingStore{}
	st := rec.mock()

	require.NoError(t, newService(t, api, st).Run(ctx))
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
	rec := &recordingStore{}
	st := rec.mock()

	require.NoError(t, newService(t, api, st).Run(ctx), "cancellation is a clean shutdown")
	assert.GreaterOrEqual(t, len(api.GetUpdatesCalls()), 3, "transient poll errors are retried")
	assert.Empty(t, rec.msgBatches())
}

func TestServiceEmptyBatchKeepsOffset(t *testing.T) {
	rec := &recordingStore{}
	st := rec.mock()
	api, err := runOnce(t, st, nil, []telegram.Update{textUpdate(3, 10, allowedChat, "hi")})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 1)
	got := api.GetUpdatesCalls()
	require.Len(t, got, 3)
	assert.Equal(t, int64(0), got[1].Offset, "an empty poll leaves the offset alone")
	assert.Equal(t, int64(4), got[2].Offset)
}

func TestServiceBufferedBatchAdvancesOffset(t *testing.T) {
	rec := &recordingStore{}
	st := rec.mock()
	api, err := runOnce(t, st, []telegram.Update{textUpdate(7, 10, otherChat, "not ours")})
	require.NoError(t, err)

	assert.Empty(t, rec.msgBatches(), "nothing for the messages table")
	require.Len(t, rec.pendBatches(), 1)
	assert.Len(t, rec.pendBatches()[0], 1, "the unknown chat is buffered, not dropped")

	got := api.GetUpdatesCalls()
	require.Len(t, got, 2)
	assert.Equal(t, int64(8), got[1].Offset, "a fully buffered batch still confirms the updates")
}

func TestServiceOwnMessageFlagged(t *testing.T) {
	u := textUpdate(1, 10, allowedChat, "our reply")
	u.Message.From = &telegram.User{ID: botID, IsBot: true, Username: botName}

	rec := &recordingStore{}
	st := rec.mock()
	_, err := runOnce(t, st, []telegram.Update{u})
	require.NoError(t, err)

	require.Len(t, rec.msgBatches(), 1)
	assert.True(t, rec.msgBatches()[0][0].FromBot)
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
