// Package store persists telegram messages in a SQLite database (WAL) and caches attachments
// on disk next to it. Full-text search is served by a standalone FTS5 table kept in sync
// inside the same transaction as the message write.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite" // sqlite driver, no cgo
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/alkk/tg-mcp/pkg/config"
)

// ErrBadMessage marks a write the database rejected because of the row itself — a constraint
// violation, a type mismatch or an oversized value. Everything else (full disk, read-only mount,
// I/O error) is a database-wide failure that dropping the message cannot fix, so callers must
// keep retrying instead of skipping.
var ErrBadMessage = errors.New("message rejected by the database")

const dbFile = "tg-mcp.db"

// Message is a stored telegram message. Zero values mean "absent" for the optional fields:
// ThreadID, ReplyTo, EditedAt and the media block.
type Message struct {
	ID           int64 // surrogate primary key, also the FTS rowid
	ChatID       int64
	MessageID    int64
	ThreadID     int64
	Sent         time.Time
	SenderID     int64
	SenderName   string
	FromBot      bool
	ReplyTo      int64
	Text         string
	IsMention    bool
	EditedAt     time.Time
	MediaType    string
	FileID       string
	FileUniqueID string
	FileName     string
	FileSize     int64
}

// HasMedia reports whether the message carries an attachment.
func (m *Message) HasMedia() bool { return m.MediaType != "" }

// Store owns the database handle and the data directory holding it and the file cache.
type Store struct {
	db  *sql.DB
	dir string
}

// New opens (creating it if needed) the database under dir and applies the schema.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dir, err)
	}

	dsn := "file:" + filepath.Join(dir, dbFile) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database in %q: %w", dir, err)
	}

	s := &Store{db: db, dir: dir}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

// Dir returns the data directory the store was opened on.
func (s *Store) Dir() string { return s.dir }

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id             INTEGER PRIMARY KEY,
  chat_id        INTEGER NOT NULL,
  message_id     INTEGER NOT NULL,
  thread_id      INTEGER,
  sent           TEXT    NOT NULL,
  sender_id      INTEGER NOT NULL DEFAULT 0,
  sender_name    TEXT    NOT NULL,
  from_bot       INTEGER NOT NULL DEFAULT 0,
  reply_to       INTEGER,
  text           TEXT    NOT NULL DEFAULT '',
  is_mention     INTEGER NOT NULL DEFAULT 0,
  edited_at      TEXT,
  media_type     TEXT,
  file_id        TEXT,
  file_unique_id TEXT,
  file_name      TEXT,
  file_size      INTEGER,
  UNIQUE (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_sent     ON messages(chat_id, sent);
CREATE INDEX IF NOT EXISTS idx_messages_chat_reply_to ON messages(chat_id, reply_to);
CREATE TABLE IF NOT EXISTS chats (
  chat_id  INTEGER PRIMARY KEY,
  customer TEXT NOT NULL,
  label    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS cursors (
  chat_id                 INTEGER PRIMARY KEY,
  last_triaged_message_id INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(text);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

const upsertSQL = `
INSERT INTO messages (chat_id, message_id, thread_id, sent, sender_id, sender_name, from_bot,
                      reply_to, text, is_mention, edited_at, media_type, file_id, file_unique_id,
                      file_name, file_size)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
  file_size      = excluded.file_size
RETURNING id`

// UpsertMessage stores one message, updating an existing row on (chat_id, message_id).
func (s *Store) UpsertMessage(ctx context.Context, m Message) error {
	return s.UpsertBatch(ctx, []Message{m})
}

// UpsertBatch stores a batch of messages in a single transaction: either all of them and their
// FTS rows land, or none do.
func (s *Store) UpsertBatch(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range msgs {
		if err := upsertTx(ctx, tx, m); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit messages: %w", err)
	}
	return nil
}

// upsertTx writes one message and replaces its FTS row; the surrogate id is the FTS rowid, so
// an edit reindexes in place instead of leaving the old text searchable.
func upsertTx(ctx context.Context, tx *sql.Tx, m Message) error {
	var id int64
	err := tx.QueryRowContext(ctx, upsertSQL,
		m.ChatID, m.MessageID, nullInt(m.ThreadID), formatTime(m.Sent), m.SenderID, m.SenderName,
		boolInt(m.FromBot), nullInt(m.ReplyTo), m.Text, boolInt(m.IsMention), nullTime(m.EditedAt),
		nullStr(m.MediaType), nullStr(m.FileID), nullStr(m.FileUniqueID), nullStr(m.FileName),
		nullInt(m.FileSize),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("upsert message %d: %w", m.MessageID, classify(err))
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("clear fts row %d: %w", id, classify(err))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(rowid, text) VALUES (?, ?)`, id, m.Text); err != nil {
		return fmt.Errorf("index message %d: %w", id, classify(err))
	}
	return nil
}

// classify tags row-level rejections with ErrBadMessage; the primary result code carries the
// class, the extended one only refines it.
func classify(err error) error {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return err
	}
	switch serr.Code() & 0xff {
	case sqlite3.SQLITE_CONSTRAINT, sqlite3.SQLITE_MISMATCH, sqlite3.SQLITE_TOOBIG, sqlite3.SQLITE_RANGE:
		return fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	return err
}

// SyncChats refreshes the chats table from the config: config wins for known chats, rows of
// chats dropped from the allowlist stay so their history keeps a customer.
func (s *Store) SyncChats(ctx context.Context, chats []config.Chat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `INSERT INTO chats (chat_id, customer, label) VALUES (?,?,?)
	           ON CONFLICT(chat_id) DO UPDATE SET customer = excluded.customer, label = excluded.label`
	for _, c := range chats {
		if _, err := tx.ExecContext(ctx, q, c.ID, c.Customer, c.Label); err != nil {
			return fmt.Errorf("sync chat %d: %w", c.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit chats: %w", err)
	}
	return nil
}

// Chats returns the chats known to the database, ordered by customer and label.
func (s *Store) Chats(ctx context.Context) ([]config.Chat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, customer, label FROM chats ORDER BY customer, label, chat_id`)
	if err != nil {
		return nil, fmt.Errorf("query chats: %w", err)
	}
	defer rows.Close()

	var res []config.Chat
	for rows.Next() {
		var c config.Chat
		if err := rows.Scan(&c.ID, &c.Customer, &c.Label); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		res = append(res, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read chats: %w", err)
	}
	return res, nil
}

// ErrNotFound is returned when a message is not in the store.
//
// Error texts here never name a chat id: the tools hand store errors to the mcp client verbatim,
// and chat ids stay inside the process. Callers that need one for diagnostics log it themselves.
var ErrNotFound = errors.New("message not found")

// messageColumns lists the message columns in scanMessage order; queries always alias the
// messages table as m so this one list serves them all.
const messageColumns = `m.id, m.chat_id, m.message_id, m.thread_id, m.sent, m.sender_id,
	m.sender_name, m.from_bot, m.reply_to, m.text, m.is_mention, m.edited_at, m.media_type,
	m.file_id, m.file_unique_id, m.file_name, m.file_size`

// MessageByID returns a single message addressed by its chat and telegram message id.
func (s *Store) MessageByID(ctx context.Context, chatID, messageID int64) (Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages m WHERE m.chat_id = ? AND m.message_id = ?`,
		chatID, messageID)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fmt.Errorf("message %d: %w", messageID, ErrNotFound)
	}
	if err != nil {
		return Message{}, fmt.Errorf("query message %d: %w", messageID, err)
	}
	return m, nil
}

// Cursor returns the last triaged telegram message id of a chat, 0 when it was never triaged.
func (s *Store) Cursor(ctx context.Context, chatID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_triaged_message_id FROM cursors WHERE chat_id = ?`, chatID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query triage cursor: %w", err)
	}
	return id, nil
}

// SetCursor advances the triage cursor of a chat and returns where it ended up. It never moves
// backwards, so marking an older message handled does not resurface everything logged after it —
// and the caller learns that from the returned position rather than assuming its own argument.
func (s *Store) SetCursor(ctx context.Context, chatID, messageID int64) (int64, error) {
	const q = `INSERT INTO cursors (chat_id, last_triaged_message_id) VALUES (?,?)
	           ON CONFLICT(chat_id) DO UPDATE SET last_triaged_message_id =
	             MAX(cursors.last_triaged_message_id, excluded.last_triaged_message_id)
	           RETURNING last_triaged_message_id`
	var at int64
	if err := s.db.QueryRowContext(ctx, q, chatID, messageID).Scan(&at); err != nil {
		return 0, fmt.Errorf("set triage cursor: %w", err)
	}
	return at, nil
}

// unreadJoin selects messages above the chat cursor. Our own replies are never unread — they are
// persisted only so threads keep their answers.
const unreadJoin = ` LEFT JOIN cursors c ON c.chat_id = m.chat_id
	WHERE m.from_bot = 0 AND m.message_id > COALESCE(c.last_triaged_message_id, 0) AND m.chat_id IN `

// UnreadCounts returns the number of untriaged messages per chat. Chats without unread messages
// are absent from the result.
func (s *Store) UnreadCounts(ctx context.Context, chatIDs []int64) (map[int64]int, error) {
	res := make(map[int64]int, len(chatIDs))
	if len(chatIDs) == 0 {
		return res, nil
	}

	in, args := inClause(chatIDs)
	const countPrefix = `SELECT m.chat_id, count(*) FROM messages m` + unreadJoin
	//nolint:gosec // in is a generated placeholder list, the chat ids are bound as arguments
	q := countPrefix + in + ` GROUP BY m.chat_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query unread counts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var chatID int64
		var n int
		if err := rows.Scan(&chatID, &n); err != nil {
			return nil, fmt.Errorf("scan unread count: %w", err)
		}
		res[chatID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unread counts: %w", err)
	}
	return res, nil
}

// ListNew returns untriaged messages of the given chats oldest first. A limit of 0 or less means
// no limit; when the limit cuts the result, the newest messages are the ones left out.
func (s *Store) ListNew(ctx context.Context, chatIDs []int64, limit int) ([]Message, error) {
	if len(chatIDs) == 0 {
		return nil, nil
	}

	in, args := inClause(chatIDs)
	q := `SELECT ` + messageColumns + ` FROM messages m` + unreadJoin + in +
		` ORDER BY m.sent, m.id LIMIT ?`
	return s.queryMessages(ctx, q, append(args, sqlLimit(limit))...)
}

// HistoryCursor marks the message a history page stopped at, in the (sent, id) order History reads
// in. The timestamp alone cannot mark it: telegram stamps whole seconds, so a page boundary that
// falls inside a second would hand back the same rows forever and never reach the older ones.
type HistoryCursor struct {
	Sent time.Time
	ID   int64 // surrogate message id, the tie-breaker within one second
}

// History returns messages of the given chats in chronological order. Zero from or to drops that
// bound, both are inclusive. The limit keeps the newest messages of the range; a non-nil before
// continues strictly older than the message it marks, which is how paginating backwards makes
// progress whatever the timestamps look like.
func (s *Store) History(ctx context.Context, chatIDs []int64, from, to time.Time,
	before *HistoryCursor, limit int) ([]Message, error) {
	if len(chatIDs) == 0 {
		return nil, nil
	}

	in, args := inClause(chatIDs)
	q := `SELECT ` + messageColumns + ` FROM messages m WHERE m.chat_id IN ` + in
	cond, rangeArgs := sentRange(from, to)
	args = append(args, rangeArgs...)
	if before != nil {
		cond += ` AND (m.sent < ? OR (m.sent = ? AND m.id < ?))`
		sent := formatTime(before.Sent)
		args = append(args, sent, sent, before.ID)
	}
	q += cond + ` ORDER BY m.sent DESC, m.id DESC LIMIT ?`
	args = append(args, sqlLimit(limit))

	msgs, err := s.queryMessages(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}

func (s *Store) queryMessages(ctx context.Context, query string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var res []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		res = append(res, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	return res, nil
}

func inClause(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "(" + strings.TrimPrefix(strings.Repeat(",?", len(ids)), ",") + ")", args
}

// sqlLimit maps a non-positive limit to sqlite's "no limit".
func sqlLimit(limit int) int64 {
	if limit <= 0 {
		return -1
	}
	return int64(limit)
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanMessage(sc scanner) (Message, error) {
	var (
		m        Message
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
	err := sc.Scan(&m.ID, &m.ChatID, &m.MessageID, &thread, &sent, &m.SenderID, &m.SenderName, &fromBot,
		&replyTo, &m.Text, &mention, &edited, &mediaTyp, &fileID, &uniqueID, &fileName, &fileSize)
	if err != nil {
		return Message{}, err //nolint:wrapcheck // callers add the query context
	}

	m.ThreadID, m.ReplyTo = thread.Int64, replyTo.Int64
	m.FromBot, m.IsMention = fromBot != 0, mention != 0
	m.MediaType, m.FileID = mediaTyp.String, fileID.String
	m.FileUniqueID, m.FileName, m.FileSize = uniqueID.String, fileName.String, fileSize.Int64
	if m.Sent, err = parseTime(sent); err != nil {
		return Message{}, err
	}
	if edited.Valid {
		if m.EditedAt, err = parseTime(edited.String); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
