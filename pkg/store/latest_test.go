package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_TrackLatest(t *testing.T) {
	ctx := t.Context()

	t.Run("picks the highest message id per chat", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertBatch(ctx, []Message{
			testMessage(-1, 5), testMessage(-1, 9), testMessage(-1, 7),
			testMessage(-2, 3), testMessage(-2, 2),
		}))
		require.NoError(t, s.TrackLatest(ctx, -1))
		require.NoError(t, s.TrackLatest(ctx, -2))

		m, ok := s.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(9), m.MessageID)
		assert.NotZero(t, m.ID, "the row comes from the database")
		m, ok = s.Latest(-2)
		require.True(t, ok)
		assert.Equal(t, int64(3), m.MessageID)
	})

	t.Run("empty chat fills on its first message", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.TrackLatest(ctx, -1))
		_, ok := s.Latest(-1)
		assert.False(t, ok)

		require.NoError(t, s.UpsertMessage(ctx, testMessage(-1, 4)))
		m, ok := s.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(4), m.MessageID)
	})

	t.Run("restart agrees with the live cache on a same-second reorder", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(dir)
		require.NoError(t, err)
		require.NoError(t, s.TrackLatest(ctx, -1))
		reply, customer := testMessage(-1, 11), testMessage(-1, 10)
		reply.Text, customer.Text = "bot reply", "customer"
		require.NoError(t, s.UpsertMessage(ctx, reply))
		require.NoError(t, s.UpsertMessage(ctx, customer))
		live, ok := s.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(11), live.MessageID)
		require.NoError(t, s.Close())

		s2, err := New(dir)
		require.NoError(t, err)
		defer s2.Close()
		require.NoError(t, s2.TrackLatest(ctx, -1))
		loaded, ok := s2.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(11), loaded.MessageID)
		assert.Equal(t, "bot reply", loaded.Text)
	})

	t.Run("undecodable newest row stays tracked and fills on the next message", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(ctx, testMessage(-1, 5)))
		_, err := s.db.ExecContext(ctx, `UPDATE messages SET sent = 'not a time' WHERE message_id = 5`)
		require.NoError(t, err)

		err = s.TrackLatest(ctx, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "load latest message")
		_, ok := s.Latest(-1)
		assert.False(t, ok)

		require.NoError(t, s.UpsertMessage(ctx, testMessage(-1, 6)))
		m, ok := s.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(6), m.MessageID)
	})

	t.Run("closed store", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.Close())
		err := s.TrackLatest(ctx, -1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "load latest message")
	})
}

func TestStore_NoteLatest(t *testing.T) {
	ctx := t.Context()

	msg := func(chatID, messageID int64, text string) Message {
		m := testMessage(chatID, messageID)
		m.Text = text
		return m
	}

	tests := []struct {
		name     string
		seed     []Message
		write    []Message
		chatID   int64
		wantOK   bool
		wantID   int64
		wantText string
	}{
		{name: "untracked chat is never cached", write: []Message{msg(-9, 1, "x")}, chatID: -9},
		{name: "higher id replaces", seed: []Message{msg(-1, 5, "old")}, write: []Message{msg(-1, 6, "new")},
			chatID: -1, wantOK: true, wantID: 6, wantText: "new"},
		{name: "lower id is ignored", seed: []Message{msg(-1, 5, "newest")}, write: []Message{msg(-1, 4, "older")},
			chatID: -1, wantOK: true, wantID: 5, wantText: "newest"},
		{name: "edit of an older message is ignored", seed: []Message{msg(-1, 4, "older"), msg(-1, 5, "newest")},
			write: []Message{msg(-1, 4, "older, edited")}, chatID: -1, wantOK: true, wantID: 5, wantText: "newest"},
		{name: "same id replaces", seed: []Message{msg(-1, 5, "before")}, write: []Message{msg(-1, 5, "after")},
			chatID: -1, wantOK: true, wantID: 5, wantText: "after"},
		{name: "newest of a batch wins whatever its position",
			write:  []Message{msg(-1, 3, "a"), msg(-1, 8, "b"), msg(-1, 6, "c")},
			chatID: -1, wantOK: true, wantID: 8, wantText: "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testStore(t)
			require.NoError(t, s.UpsertBatch(ctx, tt.seed))
			require.NoError(t, s.TrackLatest(ctx, -1))
			require.NoError(t, s.UpsertBatch(ctx, tt.write))

			m, ok := s.Latest(tt.chatID)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantID, m.MessageID)
			assert.Equal(t, tt.wantText, m.Text)
		})
	}

	t.Run("edit of the newest keeps its sent", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.TrackLatest(ctx, -1))
		first := msg(-1, 5, "before")
		require.NoError(t, s.UpsertMessage(ctx, first))

		edit := msg(-1, 5, "after")
		edit.Sent = first.Sent.Add(time.Hour)
		edit.EditedAt = first.Sent.Add(2 * time.Hour)
		require.NoError(t, s.UpsertMessage(ctx, edit))

		cached, ok := s.Latest(-1)
		require.True(t, ok)
		stored, err := s.MessageByID(ctx, -1, 5)
		require.NoError(t, err)
		assert.Equal(t, "after", cached.Text)
		assert.Equal(t, edit.EditedAt, cached.EditedAt)
		assert.Equal(t, first.Sent, cached.Sent)
		assert.Equal(t, stored.Sent, cached.Sent)
	})

	t.Run("rolled back batch leaves the cache untouched", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.UpsertMessage(ctx, msg(-1, 5, "committed")))
		require.NoError(t, s.TrackLatest(ctx, -1))
		_, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX messages_one_text ON messages(text)`)
		require.NoError(t, err)

		err = s.UpsertBatch(ctx, []Message{msg(-1, 6, "valid"), msg(-1, 7, "committed")})
		require.ErrorIs(t, err, ErrBadMessage)

		m, ok := s.Latest(-1)
		require.True(t, ok)
		assert.Equal(t, int64(5), m.MessageID)
		assert.Equal(t, "committed", m.Text)
	})
}
