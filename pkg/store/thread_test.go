package store

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_Thread(t *testing.T) {
	ctx := t.Context()

	// chat -100: a two-level reply chain with an answer typed without hitting "reply", plus
	// messages far outside the span; chat -200 shares the timeframe but must never be mixed in.
	seed := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		answer := msgAt(-100, 4, 2*time.Minute)
		answer.FromBot = true
		answer.Text = "restart the agent"
		orphan := msgAt(-100, 7, 6*time.Hour)
		orphan.ReplyTo = 999 // parent predates the bot joining the group
		require.NoError(t, s.UpsertBatch(ctx, []Message{
			msgAt(-100, 1, -time.Hour),
			msgAt(-100, 2, 0),
			reply(msgAt(-100, 3, time.Minute), 2),
			answer,
			reply(msgAt(-100, 5, 3*time.Minute), 3),
			msgAt(-100, 6, 2*time.Hour),
			orphan,
			msgAt(-200, 1, time.Minute),
		}))
		return s
	}

	t.Run("chain, nested replies and span fill", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 3)
		require.NoError(t, err)
		assert.False(t, got.Truncated)
		assert.Equal(t, []int64{2, 3, 4, 5}, messageIDs(got.Messages),
			"root, chain, nested reply and the answer typed without a reply")
		assert.True(t, got.Messages[2].FromBot, "own replies stay part of the thread")
	})

	t.Run("anchored at the root", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 3, 4, 5}, messageIDs(got.Messages))
	})

	t.Run("anchored at the leaf", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 5)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 3, 4, 5}, messageIDs(got.Messages))
	})

	t.Run("single message outside any chain", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{1}, messageIDs(got.Messages))
	})

	t.Run("chain into a message logged before the bot joined", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 7)
		require.NoError(t, err)
		assert.Equal(t, []int64{7}, messageIDs(got.Messages))
	})

	t.Run("other chats never leak in", func(t *testing.T) {
		s := seed(t)
		got, err := s.Thread(ctx, -100, 3)
		require.NoError(t, err)
		for _, m := range got.Messages {
			assert.Equal(t, int64(-100), m.ChatID)
		}
	})

	t.Run("unknown message", func(t *testing.T) {
		s := seed(t)
		_, err := s.Thread(ctx, -100, 99)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("closed store", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.Close())
		_, err := s.Thread(ctx, -100, 3)
		require.Error(t, err)
	})
}

func TestStore_Thread_topicScope(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	require.NoError(t, s.UpsertBatch(ctx, []Message{
		topic(msgAt(-300, 1, 0), 10),
		topic(msgAt(-300, 2, time.Minute), 20),
		topic(msgAt(-300, 3, 2*time.Minute), 10),
		msgAt(-300, 4, 3*time.Minute), // general
	}))

	t.Run("span fill stays inside the topic", func(t *testing.T) {
		got, err := s.Thread(ctx, -300, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 3}, messageIDs(got.Messages))
	})

	t.Run("general picks up everything in the span", func(t *testing.T) {
		got, err := s.Thread(ctx, -300, 4)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2, 3, 4}, messageIDs(got.Messages))
	})
}

func TestStore_Thread_truncation(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)

	batch := make([]Message, 0, threadCap+100)
	for i := range threadCap + 100 {
		batch = append(batch, msgAt(-400, int64(i+1), time.Duration(i)*100*time.Millisecond))
	}
	require.NoError(t, s.UpsertBatch(ctx, batch))

	t.Run("anchor at the head of the span", func(t *testing.T) {
		got, err := s.Thread(ctx, -400, 1)
		require.NoError(t, err)
		assert.True(t, got.Truncated)
		require.Len(t, got.Messages, threadCap)
		assert.Equal(t, int64(1), got.Messages[0].MessageID, "the oldest of the span is kept")
	})

	t.Run("anchor survives the cut", func(t *testing.T) {
		anchor := int64(threadCap + 100)
		got, err := s.Thread(ctx, -400, anchor)
		require.NoError(t, err)
		assert.True(t, got.Truncated)
		require.Len(t, got.Messages, threadCap)
		ids := messageIDs(got.Messages)
		assert.Contains(t, ids, anchor,
			"a busy window must not cut away the message the caller asked about")
		assert.Equal(t, anchor, ids[len(ids)-1], "the anchor is the newest of the window")
		assert.Equal(t, int64(threadCap+100-threadCap+1), ids[0],
			"the kept window ends at the anchor instead of starting at the oldest of the span")
	})

	t.Run("anchor in the middle keeps both sides", func(t *testing.T) {
		anchor := int64(threadCap/2 + 100)
		got, err := s.Thread(ctx, -400, anchor)
		require.NoError(t, err)
		assert.True(t, got.Truncated)
		require.Len(t, got.Messages, threadCap)
		ids := messageIDs(got.Messages)
		at := slices.Index(ids, anchor)
		require.NotEqual(t, -1, at)
		assert.Equal(t, threadCap/2, at, "the window is centered on the anchor")
	})
}

// TestStore_Thread_replyCycle guards the chain walk against corrupted data pointing at itself.
func TestStore_Thread_replyCycle(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	require.NoError(t, s.UpsertBatch(ctx, []Message{
		reply(msgAt(-100, 1, 0), 2),
		reply(msgAt(-100, 2, time.Minute), 1),
	}))

	got, err := s.Thread(ctx, -100, 1)
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, messageIDs(got.Messages))
}

func reply(m Message, replyTo int64) Message {
	m.ReplyTo = replyTo
	return m
}

func topic(m Message, threadID int64) Message {
	m.ThreadID = threadID
	return m
}
