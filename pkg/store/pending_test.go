package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/config"
)

func TestStore_UpsertPendingBatch(t *testing.T) {
	ctx := t.Context()

	t.Run("round trip with all fields", func(t *testing.T) {
		s := testStore(t)
		want := Pending{
			Message: Message{
				ChatID: -100, MessageID: 7, ThreadID: 3,
				Sent:       time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
				SenderID:   42,
				SenderName: "Alice",
				FromBot:    true,
				ReplyTo:    5,
				Text:       "agent connection refused",
				IsMention:  true,
				EditedAt:   time.Date(2026, 9, 1, 10, 5, 0, 0, time.UTC),
				MediaType:  "document", FileID: "fid", FileUniqueID: "uid", FileName: "log.txt", FileSize: 1234,
			},
			ChatTitle: "Acme Ops",
			ChatType:  "supergroup",
			Received:  time.Date(2026, 9, 1, 10, 0, 5, 0, time.UTC),
		}
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{want}))

		got := readPending(t, s, -100)
		require.Len(t, got, 1)
		assert.Equal(t, want, got[0])
		assert.Zero(t, got[0].ID, "pending rows carry no surrogate key")
	})

	t.Run("batch of several chats", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{
			testPending(-1, 1), testPending(-1, 2), testPending(-2, 1),
		}))

		assert.Len(t, readPending(t, s, -1), 2)
		assert.Len(t, readPending(t, s, -2), 1)
	})

	t.Run("empty batch is a no-op", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, nil))
		assert.Empty(t, readPending(t, s, -1))
	})

	t.Run("edit refreshes the text and freezes the snapshot", func(t *testing.T) {
		s := testStore(t)
		first := testPending(-1, 5)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{first}))

		edit := first
		edit.Text = "edited"
		edit.EditedAt = first.Sent.Add(time.Minute)
		edit.ChatTitle = "Renamed"
		edit.ChatType = "channel"
		edit.Received = first.Received.Add(time.Hour)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{edit}))

		got := readPending(t, s, -1)
		require.Len(t, got, 1, "the edit updates the row in place")
		assert.Equal(t, "edited", got[0].Text)
		assert.Equal(t, edit.EditedAt, got[0].EditedAt)
		assert.Equal(t, first.Sent, got[0].Sent, "an edit keeps the original sent")
		assert.Equal(t, first.Received, got[0].Received, "the hold clock does not restart")
		assert.Equal(t, first.ChatTitle, got[0].ChatTitle, "the chat identity stays as first seen")
		assert.Equal(t, first.ChatType, got[0].ChatType)
	})

	t.Run("row rejection is tagged", func(t *testing.T) {
		s := testStore(t)
		_, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX pending_one_text ON pending_messages(text)`)
		require.NoError(t, err)

		err = s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1), testPending(-1, 2)})
		require.Error(t, err, "both rows carry the same text")
		require.ErrorIs(t, err, ErrBadMessage)
		assert.Empty(t, readPending(t, s, -1), "the transaction rolled back")
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		require.Error(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1)}))
	})
}

// testPending builds a minimal valid pending message.
func testPending(chatID, messageID int64) Pending {
	return Pending{
		Message:   testMessage(chatID, messageID),
		ChatTitle: "Unknown Group",
		ChatType:  "supergroup",
		Received:  time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

// readPending returns the buffered rows of one chat, oldest message first.
func readPending(t *testing.T, s *Store, chatID int64) []Pending {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT `+pendingColumns+` FROM pending_messages WHERE chat_id = ? ORDER BY message_id`, chatID)
	require.NoError(t, err)
	defer rows.Close()

	var res []Pending
	for rows.Next() {
		p, err := scanPending(rows)
		require.NoError(t, err)
		res = append(res, p)
	}
	require.NoError(t, rows.Err())
	return res
}

func TestStore_ReplayPending(t *testing.T) {
	ctx := t.Context()

	t.Run("only the given chats are moved", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{
			testPending(-1, 1), testPending(-1, 2), testPending(-2, 9),
		}))

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		assert.Equal(t, []Replayed{{ChatID: -1, Customer: "acme", Count: 2}}, got)

		assert.Empty(t, readPending(t, s, -1), "replayed rows are dropped from the buffer")
		assert.Len(t, readPending(t, s, -2), 1, "an unknown chat stays buffered")

		msgs, err := s.ListNew(ctx, []int64{-1, -2}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2}, messageIDs(msgs))
	})

	t.Run("the replayed row is indexed for full-text search", func(t *testing.T) {
		s := testStore(t)
		p := testPending(-1, 1)
		p.Text = "agent tunnel refused"
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{p}))

		_, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		assert.Equal(t, []int64{1}, ftsMatch(t, s, "tunnel"))
	})

	t.Run("replaying over an existing message replaces its index row", func(t *testing.T) {
		s := testStore(t)
		old := testMessage(-1, 1)
		old.Text = "stale wording"
		require.NoError(t, s.UpsertMessage(ctx, old))

		p := testPending(-1, 1)
		p.Text = "fresh wording"
		p.EditedAt = p.Sent.Add(time.Minute)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{p}))

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		assert.Equal(t, 1, got[0].Count)

		assert.Empty(t, ftsMatch(t, s, "stale"), "the old text is no longer searchable")
		assert.Equal(t, []int64{1}, ftsMatch(t, s, "fresh"))

		m, err := s.MessageByID(ctx, -1, 1)
		require.NoError(t, err)
		assert.Equal(t, "fresh wording", m.Text)
		assert.Equal(t, 1, countRows(t, s, "messages"), "the message was updated, not duplicated")
	})

	t.Run("replaying into a chat that already has messages", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertBatch(ctx, []Message{msgAt(-1, 1, 0), msgAt(-1, 2, time.Minute)}))

		buffered := testPending(-1, 3)
		buffered.Sent = buffered.Sent.Add(2 * time.Minute)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{buffered}))

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		assert.Equal(t, []Replayed{{ChatID: -1, Customer: "acme", Count: 1}}, got)

		msgs, err := s.ListNew(ctx, []int64{-1}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2, 3}, messageIDs(msgs))
	})

	t.Run("a chat with nothing buffered produces no entry", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1)}))

		got, err := s.ReplayPending(ctx, []config.Chat{
			{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}},
			{ID: -2, ChatInfo: config.ChatInfo{Customer: "globex"}},
		})
		require.NoError(t, err)
		assert.Equal(t, []Replayed{{ChatID: -1, Customer: "acme", Count: 1}}, got)
	})

	t.Run("no chats at all", func(t *testing.T) {
		s := testStore(t)
		got, err := s.ReplayPending(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("corrupted timestamp is dropped, not retried forever", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1), testPending(-1, 2)}))
		_, err := s.db.ExecContext(ctx, `UPDATE pending_messages SET sent = 'not-a-time' WHERE message_id = 1`)
		require.NoError(t, err)

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err, "an undecodable row must not stop the startup replay")
		require.Len(t, got, 1)
		assert.Equal(t, 1, got[0].Count)
		assert.Equal(t, []int64{1}, got[0].Dropped)
		assert.Equal(t, 1, countRows(t, s, "messages"), "the sound row still landed")
		assert.Empty(t, readPending(t, s, -1), "the poison row is gone, so the next start is clean")
	})

	t.Run("a buffered row the messages table rejects is dropped, the rest of the chat lands", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1), testPending(-1, 2)}))
		_, err := s.db.ExecContext(ctx, `CREATE TRIGGER messages_no_insert BEFORE INSERT ON
			messages WHEN NEW.message_id = 2 BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		require.NoError(t, err)

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err, "a poison row must not brick every later startup")
		require.Len(t, got, 1)
		assert.Equal(t, 1, got[0].Count)
		assert.Equal(t, []int64{2}, got[0].Dropped)
		assert.Equal(t, 1, countRows(t, s, "messages"), "the message before it stayed committed")
		assert.Empty(t, readPending(t, s, -1), "the chat's buffer is cleared either way")
	})

	t.Run("the transaction survives a skipped row and keeps the index consistent", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{
			testPending(-1, 1), testPending(-1, 2), testPending(-1, 3),
		}))
		// refuse the first message, so the rows after it have to write on a transaction that has
		// already rolled back to the savepoint once.
		_, err := s.db.ExecContext(ctx, `CREATE TRIGGER messages_no_insert BEFORE INSERT ON
			messages WHEN NEW.message_id = 1 BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		require.NoError(t, err)

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, 2, got[0].Count, "the rows after the rejected one still land")
		assert.Equal(t, []int64{1}, got[0].Dropped)
		assert.Equal(t, countRows(t, s, "messages"), countRows(t, s, "messages_fts"),
			"no message is left written but unindexed")
	})

	t.Run("a database-wide failure still aborts the chat", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1)}))
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := s.ReplayPending(canceled, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.Error(t, err, "only a row-level rejection is skippable")
		assert.Len(t, readPending(t, s, -1), 1, "nothing dropped, so a retry can still recover it")
	})

	t.Run("a failed drop rolls the whole replay back", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1)}))
		_, err := s.db.ExecContext(ctx, `CREATE TRIGGER pending_no_delete BEFORE DELETE ON
			pending_messages BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		require.NoError(t, err)

		_, err = s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "drop buffered messages")
		assert.Zero(t, countRows(t, s, "messages"), "the replayed write rolled back with it")
		assert.Len(t, readPending(t, s, -1), 1)
	})

	t.Run("corrupted edit timestamp", func(t *testing.T) {
		s := testStore(t)
		p := testPending(-1, 1)
		p.EditedAt = p.Sent.Add(time.Minute)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{p}))
		_, err := s.db.ExecContext(ctx, `UPDATE pending_messages SET edited_at = 'not-a-time'`)
		require.NoError(t, err)

		got, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, []int64{1}, got[0].Dropped)
		assert.Zero(t, got[0].Count)
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		_, err := s.ReplayPending(ctx, []config.Chat{{ID: -1, ChatInfo: config.ChatInfo{Customer: "acme"}}})
		require.Error(t, err)
	})
}

func TestStore_SweepPending(t *testing.T) {
	ctx := t.Context()

	t.Run("only expired rows go", func(t *testing.T) {
		s := testStore(t)
		fresh, stale := testPending(-1, 1), testPending(-1, 2)
		fresh.Received = time.Now().Add(-time.Hour)
		stale.Received = time.Now().Add(-48 * time.Hour)
		old := testPending(-2, 1)
		old.Received = time.Now().Add(-72 * time.Hour)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{fresh, stale, old}))

		n, err := s.SweepPending(ctx, 24*time.Hour)
		require.NoError(t, err)
		assert.Equal(t, 2, n)

		kept := readPending(t, s, -1)
		require.Len(t, kept, 1)
		assert.Equal(t, int64(1), kept[0].MessageID)
		assert.Empty(t, readPending(t, s, -2))
	})

	t.Run("zero ttl uses the default", func(t *testing.T) {
		s := testStore(t)
		young, ancient := testPending(-1, 1), testPending(-1, 2)
		young.Received = time.Now().Add(-defaultPendingTTL + time.Hour)
		ancient.Received = time.Now().Add(-defaultPendingTTL - time.Hour)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{young, ancient}))

		n, err := s.SweepPending(ctx, 0)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		kept := readPending(t, s, -1)
		require.Len(t, kept, 1)
		assert.Equal(t, int64(1), kept[0].MessageID)
	})

	t.Run("nothing to sweep", func(t *testing.T) {
		s := testStore(t)
		n, err := s.SweepPending(ctx, time.Hour)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		_, err := s.SweepPending(ctx, time.Hour)
		require.Error(t, err)
	})
}

func TestStore_PendingChats(t *testing.T) {
	ctx := t.Context()

	t.Run("count, oldest and the first seen title", func(t *testing.T) {
		s := testStore(t)
		first := testPending(-1, 1)
		first.ChatTitle, first.ChatType = "Old Name", "group"
		first.Received = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
		renamed := testPending(-1, 2)
		renamed.ChatTitle, renamed.ChatType = "New Name", "supergroup"
		renamed.Received = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
		other := testPending(-2, 1)
		other.Received = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{renamed, first, other}))

		got, err := s.PendingChats(ctx)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, PendingChat{
			ChatID: -1, Title: "Old Name", Type: "group", Messages: 2, Oldest: first.Received,
		}, got[0], "the oldest backlog first, reported as the chat was first seen")
		assert.Equal(t, int64(-2), got[1].ChatID)
	})

	t.Run("a tie on received breaks on message id", func(t *testing.T) {
		s := testStore(t)
		same := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
		// one fetched batch shares a single received stamp and can straddle a rename
		before := testPending(-1, 1)
		before.ChatTitle, before.Received = "Old Name", same
		after := testPending(-1, 2)
		after.ChatTitle, after.Received = "New Name", same
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{after, before}))

		got, err := s.PendingChats(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "Old Name", got[0].Title, "the lowest message id wins the tie")
		assert.Equal(t, 2, got[0].Messages)
		assert.Equal(t, same, got[0].Oldest)
	})

	t.Run("a refreshing upsert would falsify the first seen title", func(t *testing.T) {
		s := testStore(t)
		a := testPending(-1, 1)
		a.ChatTitle = "Old"
		a.Received = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
		b := testPending(-1, 2)
		b.ChatTitle = "New"
		b.Received = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{a, b}))

		reEdited := a
		reEdited.ChatTitle = "New"
		reEdited.Received = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{reEdited}))

		got, err := s.PendingChats(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "Old", got[0].Title, "the conflict clause freezes chat_title")
		assert.Equal(t, a.Received, got[0].Oldest, "and received with it")
	})

	t.Run("empty table returns no rows", func(t *testing.T) {
		s := testStore(t)
		got, err := s.PendingChats(ctx)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("corrupted timestamp still reports the chat", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertPendingBatch(ctx, []Pending{testPending(-1, 1)}))
		_, err := s.db.ExecContext(ctx, `UPDATE pending_messages SET received = 'not-a-time'`)
		require.NoError(t, err)

		got, err := s.PendingChats(ctx)
		require.NoError(t, err, "the summary is a log line, it must not be able to stop the startup")
		require.Len(t, got, 1, "the operator still needs the chat id and the count")
		assert.Equal(t, int64(-1), got[0].ChatID)
		assert.Equal(t, 1, got[0].Messages)
		assert.True(t, got[0].Oldest.IsZero(), "an undatable row reads as the anomaly it is")
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		_, err := s.PendingChats(ctx)
		require.Error(t, err)
	})
}
