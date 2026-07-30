package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/config"
)

func TestNew(t *testing.T) {
	t.Run("creates schema and data dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "data")
		s, err := New(dir)
		require.NoError(t, err)
		defer s.Close()

		assert.Equal(t, dir, s.Dir())
		for _, table := range []string{"messages", "chats", "cursors", "messages_fts"} {
			var name string
			err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
			require.NoError(t, err, "table %s missing", table)
			assert.Equal(t, table, name)
		}

		var mode string
		require.NoError(t, s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
		assert.Equal(t, "wal", mode)
	})

	t.Run("reopen keeps data", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(dir)
		require.NoError(t, err)
		require.NoError(t, s.UpsertMessage(t.Context(), testMessage(1, 10)))
		require.NoError(t, s.Close())

		s2, err := New(dir)
		require.NoError(t, err)
		defer s2.Close()

		m, err := s2.MessageByID(t.Context(), 1, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(10), m.MessageID)
	})

	t.Run("unusable data dir", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "regular-file")
		writeFile(t, file, "x")

		_, err := New(filepath.Join(file, "data"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create data dir")
	})
}

func TestStore_UpsertMessage(t *testing.T) {
	ctx := t.Context()

	t.Run("round trip with all fields", func(t *testing.T) {
		s := testStore(t)
		want := Message{
			ChatID: -100, MessageID: 7, ThreadID: 3,
			Sent:       time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
			SenderID:   42,
			SenderName: "Alice",
			FromBot:    true,
			ReplyTo:    5,
			Text:       "agent connection refused",
			IsMention:  true,
			EditedAt:   time.Date(2026, 7, 30, 10, 5, 0, 0, time.UTC),
			MediaType:  "document", FileID: "fid", FileUniqueID: "uid", FileName: "log.txt", FileSize: 1234,
		}
		require.NoError(t, s.UpsertMessage(ctx, want))

		got, err := s.MessageByID(ctx, -100, 7)
		require.NoError(t, err)
		want.ID = got.ID
		assert.Equal(t, want, got)
		assert.True(t, got.HasMedia())
	})

	t.Run("optional fields stay null", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(ctx, testMessage(-100, 1)))

		var thread, replyTo, fileSize, edited, mediaType any
		err := s.db.QueryRow(
			`SELECT thread_id, reply_to, file_size, edited_at, media_type FROM messages WHERE message_id = 1`).
			Scan(&thread, &replyTo, &fileSize, &edited, &mediaType)
		require.NoError(t, err)
		assert.Nil(t, thread)
		assert.Nil(t, replyTo)
		assert.Nil(t, fileSize)
		assert.Nil(t, edited)
		assert.Nil(t, mediaType)

		got, err := s.MessageByID(ctx, -100, 1)
		require.NoError(t, err)
		assert.False(t, got.HasMedia())
		assert.True(t, got.EditedAt.IsZero())
	})

	t.Run("idempotent redelivery keeps surrogate id", func(t *testing.T) {
		s := testStore(t)
		m := testMessage(-100, 1)
		require.NoError(t, s.UpsertMessage(ctx, m))
		first, err := s.MessageByID(ctx, -100, 1)
		require.NoError(t, err)

		require.NoError(t, s.UpsertMessage(ctx, m))
		second, err := s.MessageByID(ctx, -100, 1)
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID)
		assert.Equal(t, 1, countRows(t, s, "messages"))
		assert.Equal(t, 1, countRows(t, s, "messages_fts"))
	})

	t.Run("edit keeps sent and sets edited_at", func(t *testing.T) {
		s := testStore(t)
		orig := testMessage(-100, 1)
		orig.Text = "old text"
		require.NoError(t, s.UpsertMessage(ctx, orig))

		edit := orig
		edit.Text = "new text"
		edit.Sent = orig.Sent.Add(time.Hour) // telegram resends date, but an edit must not move it
		edit.EditedAt = orig.Sent.Add(2 * time.Minute)
		require.NoError(t, s.UpsertMessage(ctx, edit))

		got, err := s.MessageByID(ctx, -100, 1)
		require.NoError(t, err)
		assert.Equal(t, orig.Sent, got.Sent)
		assert.Equal(t, edit.EditedAt, got.EditedAt)
		assert.Equal(t, "new text", got.Text)
	})

	t.Run("edit reindexes fts, old text no longer matches", func(t *testing.T) {
		s := testStore(t)
		orig := testMessage(-100, 1)
		orig.Text = "agent connection refused"
		require.NoError(t, s.UpsertMessage(ctx, orig))
		assert.Equal(t, []int64{1}, ftsMatch(t, s, "refused"))

		edit := orig
		edit.Text = "database is locked"
		edit.EditedAt = orig.Sent.Add(time.Minute)
		require.NoError(t, s.UpsertMessage(ctx, edit))

		assert.Empty(t, ftsMatch(t, s, "refused"), "stale fts row survived the edit")
		assert.Equal(t, []int64{1}, ftsMatch(t, s, "locked"))
		assert.Equal(t, 1, countRows(t, s, "messages_fts"))
	})

	t.Run("same message id in different chats", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(ctx, testMessage(-100, 1)))
		require.NoError(t, s.UpsertMessage(ctx, testMessage(-200, 1)))
		assert.Equal(t, 2, countRows(t, s, "messages"))
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		require.Error(t, s.UpsertMessage(ctx, testMessage(-100, 1)))
	})
}

func TestStore_UpsertBatch(t *testing.T) {
	ctx := t.Context()

	t.Run("stores every message with its fts row", func(t *testing.T) {
		s := testStore(t)
		batch := []Message{testMessage(-100, 1), testMessage(-100, 2), testMessage(-200, 1)}
		batch[1].Text = "unique needle here"
		require.NoError(t, s.UpsertBatch(ctx, batch))

		assert.Equal(t, 3, countRows(t, s, "messages"))
		assert.Equal(t, 3, countRows(t, s, "messages_fts"))
		assert.Equal(t, []int64{2}, ftsMatch(t, s, "needle"))
	})

	t.Run("empty batch is a no-op", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertBatch(ctx, nil))
		assert.Equal(t, 0, countRows(t, s, "messages"))
	})

	t.Run("aborted batch leaves nothing behind", func(t *testing.T) {
		s := testStore(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		require.Error(t, s.UpsertBatch(canceled, []Message{testMessage(-100, 1), testMessage(-100, 2)}))
		assert.Equal(t, 0, countRows(t, s, "messages"))
		assert.Equal(t, 0, countRows(t, s, "messages_fts"))
	})

	t.Run("failure mid batch rolls the earlier messages back", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(ctx, testMessage(-100, 1)))
		// the fts write of every message now fails, with the message row already inserted
		_, err := s.db.Exec(`DROP TABLE messages_fts`)
		require.NoError(t, err)

		err = s.UpsertBatch(ctx, []Message{testMessage(-100, 2), testMessage(-100, 3)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fts")
		assert.Equal(t, 1, countRows(t, s, "messages"), "neither message of the batch may survive")
	})
}

func TestStore_MessageByID(t *testing.T) {
	// each subtest gets its own store: the last one corrupts the row the others read
	seeded := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(t.Context(), testMessage(-100, 1)))
		return s
	}

	t.Run("found", func(t *testing.T) {
		m, err := seeded(t).MessageByID(t.Context(), -100, 1)
		require.NoError(t, err)
		assert.Equal(t, int64(1), m.MessageID)
	})

	t.Run("unknown message", func(t *testing.T) {
		_, err := seeded(t).MessageByID(t.Context(), -100, 99)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("unknown chat", func(t *testing.T) {
		_, err := seeded(t).MessageByID(t.Context(), -999, 1)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("error names no chat id", func(t *testing.T) {
		_, err := seeded(t).MessageByID(t.Context(), -100, 99)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "-100", "chat ids must not travel to mcp clients in errors")
	})

	t.Run("corrupted timestamp", func(t *testing.T) {
		s := seeded(t)
		_, err := s.db.Exec(`UPDATE messages SET sent = 'not-a-time' WHERE message_id = 1`)
		require.NoError(t, err)
		_, err = s.MessageByID(t.Context(), -100, 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse timestamp")
	})
}

func TestStore_SyncChats(t *testing.T) {
	ctx := t.Context()

	t.Run("insert, update and keep removed", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.SyncChats(ctx, []config.Chat{
			{ID: -100, ChatInfo: config.ChatInfo{Customer: "acme", Label: "main"}},
			{ID: -200, ChatInfo: config.ChatInfo{Customer: "globex"}},
		}))

		got, err := s.Chats(ctx)
		require.NoError(t, err)
		assert.Equal(t, []config.Chat{
			{ID: -100, ChatInfo: config.ChatInfo{Customer: "acme", Label: "main"}},
			{ID: -200, ChatInfo: config.ChatInfo{Customer: "globex"}},
		}, got)

		// acme renamed, globex dropped from the allowlist
		require.NoError(t, s.SyncChats(ctx, []config.Chat{
			{ID: -100, ChatInfo: config.ChatInfo{Customer: "acme-corp", Label: "ops"}},
		}))
		got, err = s.Chats(ctx)
		require.NoError(t, err)
		assert.Equal(t, []config.Chat{
			{ID: -100, ChatInfo: config.ChatInfo{Customer: "acme-corp", Label: "ops"}},
			{ID: -200, ChatInfo: config.ChatInfo{Customer: "globex"}},
		}, got, "history of a removed chat must keep its customer")
	})

	t.Run("empty chat map", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.SyncChats(ctx, nil))
		got, err := s.Chats(ctx)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		require.Error(t, s.SyncChats(ctx, []config.Chat{{ID: -100, ChatInfo: config.ChatInfo{Customer: "acme"}}}))
		_, err := s.Chats(ctx)
		require.Error(t, err)
	})
}

func TestStore_Cursor(t *testing.T) {
	ctx := t.Context()

	t.Run("never triaged chat", func(t *testing.T) {
		s := testStore(t)
		got, err := s.Cursor(ctx, -100)
		require.NoError(t, err)
		assert.Equal(t, int64(0), got)
	})

	t.Run("set, advance and never move back", func(t *testing.T) {
		s := testStore(t)
		at, err := s.SetCursor(ctx, -100, 5)
		require.NoError(t, err)
		assert.Equal(t, int64(5), at)
		got, err := s.Cursor(ctx, -100)
		require.NoError(t, err)
		assert.Equal(t, int64(5), got)

		at, err = s.SetCursor(ctx, -100, 9)
		require.NoError(t, err)
		assert.Equal(t, int64(9), at)
		got, err = s.Cursor(ctx, -100)
		require.NoError(t, err)
		assert.Equal(t, int64(9), got)

		at, err = s.SetCursor(ctx, -100, 2)
		require.NoError(t, err)
		assert.Equal(t, int64(9), at, "a backwards mark reports where the cursor actually stayed")
		got, err = s.Cursor(ctx, -100)
		require.NoError(t, err)
		assert.Equal(t, int64(9), got, "marking an older message handled must not resurface newer ones")
	})

	t.Run("cursors are per chat", func(t *testing.T) {
		s := testStore(t)
		_, err := s.SetCursor(ctx, -100, 5)
		require.NoError(t, err)
		got, err := s.Cursor(ctx, -200)
		require.NoError(t, err)
		assert.Equal(t, int64(0), got)
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		_, err := s.SetCursor(ctx, -100, 1)
		require.Error(t, err)
		_, err = s.Cursor(ctx, -100)
		require.Error(t, err)
	})
}

func TestStore_UnreadCounts(t *testing.T) {
	ctx := t.Context()

	seed := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		reply := msgAt(-100, 3, 2*time.Minute)
		reply.FromBot = true
		require.NoError(t, s.UpsertBatch(ctx, []Message{
			msgAt(-100, 1, 0), msgAt(-100, 2, time.Minute), reply,
			msgAt(-200, 1, 0),
		}))
		return s
	}

	t.Run("everything unread without a cursor", func(t *testing.T) {
		s := seed(t)
		got, err := s.UnreadCounts(ctx, []int64{-100, -200})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{-100: 2, -200: 1}, got, "own replies must not count as unread")
	})

	t.Run("cursor cuts the count", func(t *testing.T) {
		s := seed(t)
		_, err := s.SetCursor(ctx, -100, 1)
		require.NoError(t, err)
		got, err := s.UnreadCounts(ctx, []int64{-100, -200})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{-100: 1, -200: 1}, got)
	})

	t.Run("fully triaged chat is absent", func(t *testing.T) {
		s := seed(t)
		_, err := s.SetCursor(ctx, -100, 99)
		require.NoError(t, err)
		got, err := s.UnreadCounts(ctx, []int64{-100, -200})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{-200: 1}, got)
	})

	t.Run("unknown and empty chat sets", func(t *testing.T) {
		s := seed(t)
		got, err := s.UnreadCounts(ctx, []int64{-999})
		require.NoError(t, err)
		assert.Empty(t, got)

		got, err = s.UnreadCounts(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("closed store", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.Close())
		_, err := s.UnreadCounts(ctx, []int64{-100})
		require.Error(t, err)
	})
}

func TestStore_ListNew(t *testing.T) {
	ctx := t.Context()

	seed := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		reply := msgAt(-100, 4, 3*time.Minute)
		reply.FromBot = true
		mention := msgAt(-100, 2, time.Minute)
		mention.IsMention = true
		media := msgAt(-200, 1, 30*time.Second)
		media.MediaType, media.FileID, media.FileUniqueID = "document", "fid", "uid"
		require.NoError(t, s.UpsertBatch(ctx, []Message{
			msgAt(-100, 1, 0), mention, msgAt(-100, 3, 2*time.Minute), reply, media,
		}))
		return s
	}

	t.Run("oldest first across chats with flags", func(t *testing.T) {
		s := seed(t)
		got, err := s.ListNew(ctx, []int64{-100, -200}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{-100, -200, -100, -100}, chatIDs(got))
		assert.Equal(t, []int64{1, 1, 2, 3}, messageIDs(got), "own reply must not show up as new")
		assert.True(t, got[2].IsMention)
		assert.True(t, got[1].HasMedia())
	})

	t.Run("above cursor only", func(t *testing.T) {
		s := seed(t)
		_, err := s.SetCursor(ctx, -100, 2)
		require.NoError(t, err)
		got, err := s.ListNew(ctx, []int64{-100, -200}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 3}, messageIDs(got))
	})

	t.Run("limit keeps the oldest", func(t *testing.T) {
		s := seed(t)
		got, err := s.ListNew(ctx, []int64{-100, -200}, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 1}, messageIDs(got))
		assert.Equal(t, []int64{-100, -200}, chatIDs(got))
	})

	t.Run("single chat scope", func(t *testing.T) {
		s := seed(t)
		got, err := s.ListNew(ctx, []int64{-200}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{-200}, chatIDs(got))
	})

	t.Run("nothing to triage", func(t *testing.T) {
		s := seed(t)
		_, err := s.SetCursor(ctx, -100, 99)
		require.NoError(t, err)
		got, err := s.ListNew(ctx, []int64{-100}, 0)
		require.NoError(t, err)
		assert.Empty(t, got)

		got, err = s.ListNew(ctx, nil, 10)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("closed store", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.Close())
		_, err := s.ListNew(ctx, []int64{-100}, 0)
		require.Error(t, err)
	})
}

func TestStore_History(t *testing.T) {
	ctx := t.Context()
	base := testMessage(-100, 1).Sent

	seed := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		batch := make([]Message, 0, 6)
		for i := range 5 {
			batch = append(batch, msgAt(-100, int64(i+1), time.Duration(i)*time.Minute))
		}
		batch = append(batch, msgAt(-200, 1, 90*time.Second))
		require.NoError(t, s.UpsertBatch(ctx, batch))
		return s
	}

	t.Run("chronological, unbounded", func(t *testing.T) {
		s := seed(t)
		got, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2, 3, 4, 5}, messageIDs(got))
	})

	t.Run("merges chats in time order", func(t *testing.T) {
		s := seed(t)
		got, err := s.History(ctx, []int64{-100, -200}, time.Time{}, time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{-100, -100, -200, -100, -100, -100}, chatIDs(got))
	})

	t.Run("bounds are inclusive", func(t *testing.T) {
		s := seed(t)
		got, err := s.History(ctx, []int64{-100}, base.Add(time.Minute), base.Add(3*time.Minute), nil, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 3, 4}, messageIDs(got))

		got, err = s.History(ctx, []int64{-100}, base.Add(3*time.Minute), time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{4, 5}, messageIDs(got))

		got, err = s.History(ctx, []int64{-100}, time.Time{}, base.Add(time.Minute), nil, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2}, messageIDs(got))
	})

	t.Run("limit keeps the newest of the range", func(t *testing.T) {
		s := seed(t)
		got, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{4, 5}, messageIDs(got))
	})

	t.Run("paginate backwards by cursor", func(t *testing.T) {
		s := seed(t)
		page, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 2)
		require.NoError(t, err)
		require.Equal(t, []int64{4, 5}, messageIDs(page))

		page, err = s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, cursorOf(page[0]), 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 3}, messageIDs(page), "the page continues strictly older")

		page, err = s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, cursorOf(page[0]), 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{1}, messageIDs(page))
	})

	t.Run("messages sharing a second are paged through, not repeated", func(t *testing.T) {
		s := testStore(t)
		batch := make([]Message, 0, 4)
		for i := range 4 {
			batch = append(batch, msgAt(-100, int64(i+1), 0)) // one album: four messages, one second
		}
		require.NoError(t, s.UpsertBatch(ctx, batch))

		var seen []int64
		var before *HistoryCursor
		for range 4 {
			page, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, before, 1)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].MessageID)
			before = cursorOf(page[0])
		}
		assert.Equal(t, []int64{4, 3, 2, 1}, seen, "a time-only bound would hand back message 4 forever")
	})

	t.Run("empty results", func(t *testing.T) {
		s := seed(t)
		got, err := s.History(ctx, []int64{-100}, base.Add(time.Hour), time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Empty(t, got)

		got, err = s.History(ctx, []int64{-999}, time.Time{}, time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Empty(t, got)

		got, err = s.History(ctx, nil, time.Time{}, time.Time{}, nil, 0)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("corrupted timestamp", func(t *testing.T) {
		s := seed(t)
		_, err := s.db.Exec(`UPDATE messages SET sent = 'not-a-time' WHERE chat_id = -100 AND message_id = 1`)
		require.NoError(t, err)
		_, err = s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse timestamp")
	})

	t.Run("closed store", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.Close())
		_, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 0)
		require.Error(t, err)
	})
}

// TestStore_WALConcurrency hammers the store from a writer and two readers at once: WAL plus the
// busy timeout must keep readers from ever seeing "database is locked".
func TestStore_WALConcurrency(t *testing.T) {
	const rounds = 40

	s := testStore(t)
	ctx := t.Context()
	errs := make(chan error, 3)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range rounds {
			batch := []Message{
				msgAt(-100, int64(2*i+1), time.Duration(2*i)*time.Second),
				msgAt(-100, int64(2*i+2), time.Duration(2*i+1)*time.Second),
			}
			if err := s.UpsertBatch(ctx, batch); err != nil {
				errs <- err
				return
			}
			if _, err := s.SetCursor(ctx, -100, int64(i)); err != nil {
				errs <- err
				return
			}
		}
	}()
	for range 2 {
		go func() {
			defer wg.Done()
			for range rounds {
				if _, err := s.History(ctx, []int64{-100}, time.Time{}, time.Time{}, nil, 100); err != nil {
					errs <- err
					return
				}
				if _, err := s.ListNew(ctx, []int64{-100}, 100); err != nil {
					errs <- err
					return
				}
				if _, err := s.UnreadCounts(ctx, []int64{-100}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 2*rounds, countRows(t, s, "messages"))
}

func TestClassify(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("row rejection is tagged", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx, `INSERT INTO messages (chat_id) VALUES (1)`)
		require.Error(t, err, "sender_name and sent are NOT NULL")
		require.ErrorIs(t, classify(err), ErrBadMessage)
		require.ErrorIs(t, classify(err), err, "the driver error stays reachable")
	})

	t.Run("database-wide failure is left alone", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx, `INSERT INTO nowhere (chat_id) VALUES (1)`)
		require.Error(t, err)
		assert.NotErrorIs(t, classify(err), ErrBadMessage)
	})

	t.Run("non-sqlite error is left alone", func(t *testing.T) {
		err := errors.New("disk full")
		assert.Same(t, err, classify(err))
	})
}

func TestStore_Close(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "close is idempotent, cleanup must not fail")
}

// testStore opens a store in a temp dir, closed at the end of the test.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testMessage builds a minimal valid message.
func testMessage(chatID, messageID int64) Message {
	return Message{
		ChatID:     chatID,
		MessageID:  messageID,
		Sent:       time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		SenderID:   1,
		SenderName: "Alice",
		Text:       "hello",
	}
}

// msgAt builds a minimal valid message sent offset after the base time.
func msgAt(chatID, messageID int64, offset time.Duration) Message {
	m := testMessage(chatID, messageID)
	m.Sent = m.Sent.Add(offset)
	return m
}

// cursorOf marks a message as the point a page stopped at, the way the server does.
func cursorOf(m Message) *HistoryCursor {
	return &HistoryCursor{Sent: m.Sent, ID: m.ID}
}

func messageIDs(msgs []Message) []int64 {
	res := make([]int64, len(msgs))
	for i, m := range msgs {
		res[i] = m.MessageID
	}
	return res
}

func chatIDs(msgs []Message) []int64 {
	res := make([]int64, len(msgs))
	for i, m := range msgs {
		res[i] = m.ChatID
	}
	return res
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&n))
	return n
}

// ftsMatch runs a full-text query and returns the telegram message ids of the hits.
func ftsMatch(t *testing.T, s *Store, query string) []int64 {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT m.message_id FROM messages_fts f JOIN messages m ON m.id = f.rowid
		 WHERE messages_fts MATCH ? ORDER BY m.message_id`, query)
	require.NoError(t, err)
	defer rows.Close()

	var res []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		res = append(res, id)
	}
	require.NoError(t, rows.Err())
	return res
}
