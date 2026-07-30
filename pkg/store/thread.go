package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"
)

const (
	// threadCap bounds a reconstructed thread; anything beyond it is cut and flagged.
	threadCap = 500
	// spanPad widens the span of the reply chain on both ends, catching answers typed into the
	// chat without hitting "reply".
	spanPad = 5 * time.Minute
)

// Thread is a reconstructed conversation: chronological messages plus whether the cap cut them.
type Thread struct {
	Messages  []Message
	Truncated bool
}

// Thread reconstructs the conversation around a message: the reply chain from it up to its root,
// every reply hanging off any chain member, then every message the chat saw between the first and
// last of those, padded by five minutes on both ends. When the anchor sits in a forum topic the
// span fill stays inside that topic. Result is chronological and capped at 500 messages around
// the anchor.
func (s *Store) Thread(ctx context.Context, chatID, messageID int64) (Thread, error) {
	anchor, err := s.MessageByID(ctx, chatID, messageID)
	if err != nil {
		return Thread{}, err
	}

	core, err := s.replyChain(ctx, anchor)
	if err != nil {
		return Thread{}, err
	}
	if core, err = s.appendReplies(ctx, chatID, core); err != nil {
		return Thread{}, err
	}

	from, to := core[0].Sent, core[0].Sent
	for _, m := range core[1:] {
		if m.Sent.Before(from) {
			from = m.Sent
		}
		if m.Sent.After(to) {
			to = m.Sent
		}
	}

	filled, err := s.spanFill(ctx, chatID, anchor, from.Add(-spanPad), to.Add(spanPad))
	if err != nil {
		return Thread{}, err
	}

	msgs := mergeMessages(core, filled)
	if len(msgs) > threadCap {
		return Thread{Messages: capAround(msgs, anchor.ID), Truncated: true}, nil
	}
	return Thread{Messages: msgs}, nil
}

// capAround trims a chronological set to threadCap messages centered on the anchor, so a busy
// window cannot cut away the very message the caller asked about.
func capAround(msgs []Message, anchorID int64) []Message {
	at := slices.IndexFunc(msgs, func(m Message) bool { return m.ID == anchorID })
	start := max(at-threadCap/2, 0)
	start = min(start, len(msgs)-threadCap)
	return msgs[start : start+threadCap]
}

// replyChain walks up from the anchor to the root of its reply chain. A chain running into a
// message logged before the bot joined simply ends there.
func (s *Store) replyChain(ctx context.Context, anchor Message) ([]Message, error) {
	chain := []Message{anchor}
	seen := map[int64]bool{anchor.MessageID: true}
	for cur := anchor; cur.ReplyTo != 0 && !seen[cur.ReplyTo]; {
		parent, err := s.MessageByID(ctx, cur.ChatID, cur.ReplyTo)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		seen[parent.MessageID] = true
		chain = append(chain, parent)
		cur = parent
	}
	return chain, nil
}

// appendReplies walks down from the collected messages, adding everything that replies to one of
// them until the tree is exhausted, so a reply to a reply stays part of the thread.
func (s *Store) appendReplies(ctx context.Context, chatID int64, collected []Message) ([]Message, error) {
	seen := make(map[int64]bool, len(collected))
	frontier := make([]int64, 0, len(collected))
	for _, m := range collected {
		seen[m.MessageID] = true
		frontier = append(frontier, m.MessageID)
	}

	for len(frontier) > 0 && len(collected) < threadCap {
		in, args := inClause(frontier)
		q := `SELECT ` + messageColumns + ` FROM messages m WHERE m.chat_id = ? AND m.reply_to IN ` + in +
			` ORDER BY m.sent, m.id LIMIT ?`
		args = append([]any{chatID}, append(args, int64(threadCap))...)

		replies, err := s.queryMessages(ctx, q, args...)
		if err != nil {
			return nil, err
		}

		frontier = frontier[:0]
		for _, m := range replies {
			if seen[m.MessageID] {
				continue
			}
			seen[m.MessageID] = true
			collected = append(collected, m)
			frontier = append(frontier, m.MessageID)
		}
	}
	return collected, nil
}

// spanFill returns what the chat saw in the given window, scoped to a forum topic when the anchor
// has one. The window is walked outwards from the anchor in both directions rather than from its
// start: a busy window holds far more than the cap, and taking the head of it would answer with
// messages predating the anchor while dropping the ones surrounding it.
func (s *Store) spanFill(ctx context.Context, chatID int64, anchor Message, from, to time.Time) ([]Message, error) {
	where := `m.chat_id = ? AND m.sent >= ? AND m.sent <= ?`
	args := []any{chatID, formatTime(from), formatTime(to)}
	if anchor.ThreadID != 0 {
		where += ` AND m.thread_id = ?`
		args = append(args, anchor.ThreadID)
	}
	sent := formatTime(anchor.Sent)

	// ties on sent are broken by the surrogate id, the same order the merged result uses
	before, err := s.queryMessages(ctx,
		`SELECT `+messageColumns+` FROM messages m WHERE `+where+
			` AND (m.sent < ? OR (m.sent = ? AND m.id <= ?)) ORDER BY m.sent DESC, m.id DESC LIMIT ?`,
		append(slices.Clone(args), sent, sent, anchor.ID, int64(threadCap+1))...)
	if err != nil {
		return nil, err
	}
	after, err := s.queryMessages(ctx,
		`SELECT `+messageColumns+` FROM messages m WHERE `+where+
			` AND (m.sent > ? OR (m.sent = ? AND m.id > ?)) ORDER BY m.sent, m.id LIMIT ?`,
		append(slices.Clone(args), sent, sent, anchor.ID, int64(threadCap+1))...)
	if err != nil {
		return nil, err
	}
	return append(before, after...), nil
}

// mergeMessages unions two message sets by surrogate id and orders them chronologically.
func mergeMessages(a, b []Message) []Message {
	seen := make(map[int64]bool, len(a)+len(b))
	res := make([]Message, 0, len(a)+len(b))
	for _, set := range [][]Message{a, b} {
		for _, m := range set {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			res = append(res, m)
		}
	}
	slices.SortFunc(res, func(x, y Message) int {
		if c := x.Sent.Compare(y.Sent); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return res
}
