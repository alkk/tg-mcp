package store

import (
	"context"
	"fmt"
)

// TrackLatest loads the newest message of a chat and keeps it current from then on, so Latest
// answers without a query. Newest is the highest telegram message id, the order the live update in
// UpsertBatch follows too, so a restart never changes the answer. It is meant for startup, before
// any writer runs: the load and the cache update are not one atomic step. The chat is tracked even
// when the load fails, so its next message fills the cache.
func (s *Store) TrackLatest(ctx context.Context, chatID int64) error {
	s.latestMu.Lock()
	s.tracked[chatID] = struct{}{}
	s.latestMu.Unlock()

	msgs, err := s.queryMessages(ctx, `SELECT `+messageColumns+` FROM messages m WHERE m.chat_id = ?
		ORDER BY m.message_id DESC LIMIT 1`, chatID)
	if err != nil {
		return fmt.Errorf("load latest message: %w", err)
	}
	if len(msgs) > 0 {
		s.latestMu.Lock()
		s.latest[chatID] = msgs[0]
		s.latestMu.Unlock()
	}
	return nil
}

// Latest returns the newest message of a chat registered with TrackLatest. Its surrogate ID is not
// reliable: a message cached after startup carries whatever UpsertBatch was given.
func (s *Store) Latest(chatID int64) (Message, bool) {
	s.latestMu.RLock()
	defer s.latestMu.RUnlock()
	m, ok := s.latest[chatID]
	return m, ok
}

// noteLatest folds committed messages into the cache. An edit of the cached message keeps its
// sent, the way upsertSQL never updates sent on conflict.
func (s *Store) noteLatest(msgs []Message) {
	s.latestMu.Lock()
	defer s.latestMu.Unlock()
	for _, m := range msgs {
		if _, ok := s.tracked[m.ChatID]; !ok {
			continue
		}
		cur, ok := s.latest[m.ChatID]
		if ok && m.MessageID < cur.MessageID {
			continue
		}
		if ok && m.MessageID == cur.MessageID {
			m.Sent = cur.Sent
		}
		s.latest[m.ChatID] = m
	}
}
