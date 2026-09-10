package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alkk/tg-mcp/pkg/config"
)

// Pending is a message from a chat outside the allowlist, held until the chat is added to the
// chat map. The embedded Message.ID is always zero: pending rows carry no surrogate key — that
// key exists to be the FTS rowid and pending rows are deliberately not indexed — they get one
// from upsertTx when they are replayed.
type Pending struct {
	Message
	ChatTitle string
	ChatType  string
	Received  time.Time
}

// pendingColumns lists the pending columns in scanPending order.
const pendingColumns = `chat_id, message_id, chat_title, chat_type, received, thread_id, sent,
	sender_id, sender_name, from_bot, reply_to, text, is_mention, edited_at, media_type, file_id,
	file_unique_id, file_name, file_size`

// upsertPendingSQL refreshes the message columns of an already buffered message but leaves sent
// (an edit keeps the original, as in upsertSQL) alone, and with it received, chat_title and
// chat_type: the row stays the snapshot of the chat as it was when the message was first
// buffered, so an edit neither restarts the hold clock nor rewrites the chat identity
// PendingChats reports as first seen.
const upsertPendingSQL = `
INSERT INTO pending_messages (` + pendingColumns + `)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chat_id, message_id) DO UPDATE SET
  thread_id      = excluded.thread_id,
  sender_id      = excluded.sender_id,
  sender_name    = excluded.sender_name,
  from_bot       = excluded.from_bot,
  reply_to       = excluded.reply_to,
  text           = excluded.text,
  is_mention     = excluded.is_mention,
  edited_at      = excluded.edited_at,
  media_type     = excluded.media_type,
  file_id        = excluded.file_id,
  file_unique_id = excluded.file_unique_id,
  file_name      = excluded.file_name,
  file_size      = excluded.file_size`

// UpsertPendingBatch buffers a batch of messages from chats outside the allowlist in a single
// transaction. Errors are classified like message writes, so a poison pending row is skippable.
func (s *Store) UpsertPendingBatch(ctx context.Context, msgs []Pending) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, p := range msgs {
		m := p.Message
		_, err := tx.ExecContext(ctx, upsertPendingSQL,
			m.ChatID, m.MessageID, p.ChatTitle, p.ChatType, formatTime(p.Received),
			nullInt(m.ThreadID), formatTime(m.Sent), m.SenderID, m.SenderName, boolInt(m.FromBot),
			nullInt(m.ReplyTo), m.Text, boolInt(m.IsMention), nullTime(m.EditedAt),
			nullStr(m.MediaType), nullStr(m.FileID), nullStr(m.FileUniqueID), nullStr(m.FileName),
			nullInt(m.FileSize),
		)
		if err != nil {
			return fmt.Errorf("buffer pending message %d: %w", m.MessageID, classify(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pending messages: %w", err)
	}
	return nil
}

func scanPending(sc scanner) (Pending, error) {
	var (
		p        Pending
		received string
		thread   sql.NullInt64
		replyTo  sql.NullInt64
		sent     string
		edited   sql.NullString
		fromBot  int64
		mention  int64
		mediaTyp sql.NullString
		fileID   sql.NullString
		uniqueID sql.NullString
		fileName sql.NullString
		fileSize sql.NullInt64
	)
	err := sc.Scan(&p.ChatID, &p.MessageID, &p.ChatTitle, &p.ChatType, &received, &thread, &sent,
		&p.SenderID, &p.SenderName, &fromBot, &replyTo, &p.Text, &mention, &edited, &mediaTyp,
		&fileID, &uniqueID, &fileName, &fileSize)
	if err != nil {
		return Pending{}, err //nolint:wrapcheck // callers add the query context
	}

	p.ThreadID, p.ReplyTo = thread.Int64, replyTo.Int64
	p.FromBot, p.IsMention = fromBot != 0, mention != 0
	p.MediaType, p.FileID = mediaTyp.String, fileID.String
	p.FileUniqueID, p.FileName, p.FileSize = uniqueID.String, fileName.String, fileSize.Int64

	// a timestamp that will not parse condemns this row and no other, so it is tagged
	// ErrBadMessage and the ids are kept: replay drops such a row instead of aborting on it.
	// formatTime does not guarantee a round trip — a bot api date past year 9999 writes fine and
	// parses back as an error — and an aborting replay would retry that row at every startup.
	if p.Received, err = parseTime(received); err != nil {
		return p, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if p.Sent, err = parseTime(sent); err != nil {
		return p, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if edited.Valid {
		if p.EditedAt, err = parseTime(edited.String); err != nil {
			return p, fmt.Errorf("%w: %w", ErrBadMessage, err)
		}
	}
	return p, nil
}

// Replayed reports one chat's contribution to a replay. Dropped names the buffered messages the
// messages table refused, by message id: they are gone from the buffer either way, so the caller
// logs them rather than expecting another attempt.
type Replayed struct {
	ChatID   int64
	Customer string
	Count    int
	Dropped  []int64
}

// ReplayPending moves the buffered messages of the given chats into messages, one transaction per
// chat: a failure on one chat leaves the others alone and each gets a meaningful count. Chats with
// nothing buffered produce no entry.
func (s *Store) ReplayPending(ctx context.Context, chats []config.Chat) ([]Replayed, error) {
	var res []Replayed
	for _, c := range chats {
		n, dropped, err := s.replayChat(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("replay buffered messages of customer %q: %w", c.Customer, err)
		}
		if n == 0 && len(dropped) == 0 {
			continue
		}
		res = append(res, Replayed{ChatID: c.ID, Customer: c.Customer, Count: n, Dropped: dropped})
	}
	return res, nil
}

// replayChat writes one chat's buffered messages through upsertTx — which is what keeps
// messages_fts correct, an INSERT..SELECT would have to redo the delete-then-insert by hand — and
// drops them in the same transaction.
//
// A row the messages table rejects for its own sake (ErrBadMessage) is skipped, not fatal: replay
// runs before anything else starts, so an aborting one is not a lost batch the way skipPoison's is
// but a process that cannot boot — and since nothing is dropped on abort, every later start hits
// the same row. Skipping needs no extra delete, the chat-wide one below takes the skipped rows
// with the rest. A database-wide failure still aborts the chat and stays fatal: retrying is the
// only thing that can fix it, and dropping a whole chat's backlog would be loss for nothing.
func (s *Store) replayChat(ctx context.Context, chatID int64) (int, []int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	msgs, dropped, err := pendingOf(ctx, tx, chatID)
	if err != nil {
		return 0, nil, err
	}
	if len(msgs) == 0 && len(dropped) == 0 {
		return 0, nil, nil
	}

	moved := 0
	for _, m := range msgs {
		err := replayOne(ctx, tx, m)
		switch {
		case errors.Is(err, ErrBadMessage):
			dropped = append(dropped, m.MessageID)
		case err != nil:
			return 0, nil, err
		default:
			moved++
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_messages WHERE chat_id = ?`, chatID); err != nil {
		return 0, nil, fmt.Errorf("drop buffered messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit replay: %w", err)
	}
	return moved, dropped, nil
}

// replaySavepoint is the savepoint replayOne wraps one message in. It is released before the next
// message takes it, so the name is never nested with itself.
const replaySavepoint = `replay_message`

// replayOne writes one buffered message inside a savepoint so that a rejected one leaves nothing
// behind: upsertTx spans three statements, and a row refused by the second or third would
// otherwise stay in messages with its FTS entry deleted and not rewritten — searchable content
// silently dropped out of the index, which is the corruption a standalone FTS table exists to
// avoid.
func replayOne(ctx context.Context, tx *sql.Tx, m Message) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+replaySavepoint); err != nil {
		return fmt.Errorf("open savepoint: %w", err)
	}
	upsertErr := upsertTx(ctx, tx, m)
	if upsertErr != nil {
		// ROLLBACK TO leaves the savepoint itself in place, so the RELEASE below still applies.
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO `+replaySavepoint); err != nil {
			return fmt.Errorf("roll back savepoint: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE `+replaySavepoint); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return upsertErr
}

// pendingOf reads one chat's buffered messages in the order they were sent, reporting separately
// the message ids of rows too corrupt to decode — the caller drops those instead of failing on
// them, for the reason replayChat gives. It closes the rows before returning: the caller writes on
// the same transaction, i.e. the same connection.
func pendingOf(ctx context.Context, tx *sql.Tx, chatID int64) (msgs []Message, bad []int64, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pendingColumns+
		` FROM pending_messages WHERE chat_id = ? ORDER BY sent, message_id`, chatID)
	if err != nil {
		return nil, nil, fmt.Errorf("read buffered messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		p, err := scanPending(rows)
		switch {
		case errors.Is(err, ErrBadMessage):
			bad = append(bad, p.MessageID)
		case err != nil:
			return nil, nil, fmt.Errorf("read buffered message: %w", err)
		default:
			msgs = append(msgs, p.Message)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read buffered messages: %w", err)
	}
	return msgs, bad, nil
}

// defaultPendingTTL is how long a buffered message is kept when SweepPending is given a zero ttl.
const defaultPendingTTL = 14 * 24 * time.Hour

// SweepPending drops buffered messages older than ttl and reports how many went. A zero ttl means
// the default, the way a zero file link TTL means five minutes.
func (s *Store) SweepPending(ctx context.Context, ttl time.Duration) (int, error) {
	if ttl == 0 {
		ttl = defaultPendingTTL
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending_messages WHERE received < ?`,
		formatTime(time.Now().Add(-ttl)))
	if err != nil {
		return 0, fmt.Errorf("sweep buffered messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep buffered messages: %w", err)
	}
	return int(n), nil
}

// PendingChat summarizes one chat's buffer for the startup report.
type PendingChat struct {
	ChatID   int64
	Title    string
	Type     string
	Messages int
	Oldest   time.Time
}

// pendingChatsSQL picks the earliest buffered row per chat with a window function rather than
// grouping on bare chat_title/chat_type next to MIN(received). Received is stamped once per
// fetched batch and stored to the second, so several rows routinely share the minimum, and
// SQLite's bare-column rule leaves the pick among tied rows undefined — a batch that straddles a
// rename would report either title. ROW_NUMBER over (received, message_id) makes the pick a total
// order: the lowest message id of the earliest received second. For new messages that is arrival
// order, since telegram's ids only grow, but an edit first buffered for an older id can outrank a
// newer message of the same second, so the reported identity is the lowest-id snapshot rather
// than literally the first update seen — a distinction with no consequence for a line the
// operator only needs to write the chats.yml entry. The other half of the contract is
// upsertPendingSQL's: it freezes chat_title and chat_type, and unfreezing them there would
// silently falsify this query.
const pendingChatsSQL = `
SELECT chat_id, chat_title, chat_type, messages, oldest
FROM (
  SELECT chat_id, chat_title, chat_type,
         COUNT(*)      OVER (PARTITION BY chat_id) AS messages,
         MIN(received) OVER (PARTITION BY chat_id) AS oldest,
         ROW_NUMBER()  OVER (PARTITION BY chat_id ORDER BY received, message_id) AS rn
  FROM pending_messages
)
WHERE rn = 1
ORDER BY oldest`

// PendingChats reports every chat with buffered messages, the ones still missing from the chat
// map, oldest backlog first.
func (s *Store) PendingChats(ctx context.Context) ([]PendingChat, error) {
	rows, err := s.db.QueryContext(ctx, pendingChatsSQL)
	if err != nil {
		return nil, fmt.Errorf("read buffered chats: %w", err)
	}
	defer rows.Close()

	var res []PendingChat
	for rows.Next() {
		var (
			c      PendingChat
			oldest string
		)
		if err = rows.Scan(&c.ChatID, &c.Title, &c.Type, &c.Messages, &oldest); err != nil {
			return nil, fmt.Errorf("read buffered chat: %w", err)
		}
		// an undatable row must not cost the operator the whole report — and with it the startup,
		// since drainPending is fatal. The chat id and the count are what the line exists to
		// deliver; a zero Oldest reads as the anomaly it is, where an error here would brick every
		// start (the sweep compares received as text, so such a row never expires either).
		if c.Oldest, err = parseTime(oldest); err != nil {
			c.Oldest = time.Time{}
		}
		res = append(res, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read buffered chats: %w", err)
	}
	return res, nil
}
