// Package ingest runs the telegram long-poll loop: updates are filtered against the chat
// allowlist and the surviving messages are written to the store. The getUpdates offset advances
// only after a batch is committed, so a crash redelivers instead of losing messages.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

//go:generate moq -out mocks/bot_api.go -pkg mocks -skip-ensure -fmt goimports . botAPI:BotAPI
//go:generate moq -out mocks/message_store.go -pkg mocks -skip-ensure -fmt goimports . messageStore:MessageStore

// botAPI is the slice of the telegram client the ingest loop needs.
type botAPI interface {
	DeleteWebhook(ctx context.Context) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error)
}

// messageStore persists a batch of messages atomically.
type messageStore interface {
	UpsertBatch(ctx context.Context, msgs []store.Message) error
}

const (
	defaultPollTimeout = 30 * time.Second
	defaultBackoff     = time.Second
	maxBackoff         = 30 * time.Second
	storeRetries       = 3
)

// Params configures the ingest service; only API, Store and Chats are mandatory.
type Params struct {
	API         botAPI
	Store       messageStore
	Chats       *config.Config
	BotID       int64  // used to recognize our own messages and replies addressed to the bot
	BotUsername string // used for @mention detection
	PollTimeout time.Duration
}

// Service polls telegram and feeds the store.
type Service struct {
	api   botAPI
	store messageStore
	chats *config.Config

	botID       int64
	botUsername string
	pollTimeout time.Duration
	backoffBase time.Duration
	maxBackoff  time.Duration
	retries     int
}

// New creates an ingest service.
func New(p Params) *Service {
	s := &Service{
		api:         p.API,
		store:       p.Store,
		chats:       p.Chats,
		botID:       p.BotID,
		botUsername: strings.TrimPrefix(p.BotUsername, "@"),
		pollTimeout: p.PollTimeout,
		backoffBase: defaultBackoff,
		maxBackoff:  maxBackoff,
		retries:     storeRetries,
	}
	if s.pollTimeout <= 0 {
		s.pollTimeout = defaultPollTimeout
	}
	return s
}

// record pairs a mapped message with the update it came from, so a poison update can be logged
// with its raw payload before it is skipped.
type record struct {
	update telegram.Update
	msg    store.Message
}

// Run drives the poll loop until the context is canceled. A webhook or a competing poller
// (409 Conflict) is fatal, as is a token the api rejects outright (401/403/404); everything else
// is retried with exponential backoff.
func (s *Service) Run(ctx context.Context) error {
	if err := s.api.DeleteWebhook(ctx); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	slog.Info("ingest started", "poll_timeout", s.pollTimeout, "chats", len(s.chats.All()),
		"bot", s.botUsername)

	var offset, failedFrom int64
	var pollFails, storeFails int

	for {
		if stopped(ctx) {
			return nil
		}

		updates, err := s.api.GetUpdates(ctx, offset, s.pollTimeout)
		if stopped(ctx) { // shutdown, whatever the poll returned
			return nil
		}
		if err != nil {
			var apiErr *telegram.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.IsConflict():
				return fmt.Errorf("getUpdates rejected, a webhook or another poller is active: %w", err)
			case errors.As(err, &apiErr) && apiErr.IsAuthFailure():
				// backing off forever would leave ingestion dead while /ping keeps answering
				return fmt.Errorf("getUpdates rejected, the bot token is invalid or revoked: %w", err)
			}
			pollFails++
			slog.Warn("poll failed", "err", err, "attempt", pollFails)
			s.wait(ctx, s.backoff(pollFails))
			continue
		}
		pollFails = 0
		if len(updates) == 0 {
			continue
		}

		first, last := updates[0].UpdateID, updates[len(updates)-1].UpdateID
		recs := s.collect(updates)
		if err := s.persist(ctx, recs); err != nil {
			// keyed on the first id: a redelivery repeats it while the tail grows with whatever
			// arrived meanwhile, so keying on the last one would reset the counter forever
			if failedFrom != first {
				failedFrom, storeFails = first, 0
			}
			storeFails++
			slog.Error("store batch failed", "err", err, "attempt", storeFails, "messages", len(recs))
			// only a message the database rejects can be skipped: a database-wide failure (full
			// disk, read-only mount) is retried forever, dropping the batch would lose it for good
			if storeFails < s.retries || !errors.Is(err, store.ErrBadMessage) {
				s.wait(ctx, s.backoff(storeFails))
				continue // offset stays put, telegram redelivers the batch
			}
			if !s.skipPoison(ctx, recs) {
				s.wait(ctx, s.backoff(storeFails))
				continue
			}
			storeFails = 0
		}
		offset = last + 1
	}
}

// persist writes the whole batch in one transaction.
func (s *Service) persist(ctx context.Context, recs []record) error {
	if len(recs) == 0 {
		return nil
	}
	msgs := make([]store.Message, 0, len(recs))
	for _, r := range recs {
		msgs = append(msgs, r.msg)
	}
	if err := s.store.UpsertBatch(ctx, msgs); err != nil {
		return fmt.Errorf("store %d messages: %w", len(msgs), err)
	}
	return nil
}

// skipPoison retries the batch message by message so one bad update cannot stall every chat;
// what the database rejects is logged with its full payload and dropped. It reports whether the
// replay ran to the end: a database-wide failure aborts it so the caller keeps the offset pinned.
func (s *Service) skipPoison(ctx context.Context, recs []record) bool {
	for _, r := range recs {
		err := s.store.UpsertBatch(ctx, []store.Message{r.msg})
		switch {
		case err == nil:
		case errors.Is(err, store.ErrBadMessage):
			slog.Error("update skipped after repeated store failures", "err", err,
				"update_id", r.update.UpdateID, "chat_id", r.msg.ChatID,
				"message_id", r.msg.MessageID, "payload", payload(r.update))
		default:
			slog.Error("store unusable during per-message replay, batch kept", "err", err,
				"update_id", r.update.UpdateID, "chat_id", r.msg.ChatID)
			return false
		}
	}
	return true
}

func (s *Service) collect(updates []telegram.Update) []record {
	recs := make([]record, 0, len(updates))
	for _, u := range updates {
		msg, ok := s.convert(u)
		if !ok {
			continue
		}
		recs = append(recs, record{update: u, msg: msg})
	}
	return recs
}

// convert maps an update to a stored message; ok is false for everything filtered out.
func (s *Service) convert(u telegram.Update) (msg store.Message, ok bool) {
	m := u.Message
	if m == nil {
		m = u.EditedMessage
	}
	if m == nil {
		slog.Debug("update carries no message", "update_id", u.UpdateID)
		return store.Message{}, false
	}

	if m.MigrateToChatID != 0 {
		slog.Warn("chat migrated to a supergroup, update the chat map",
			"old_chat_id", m.Chat.ID, "new_chat_id", m.MigrateToChatID, "title", m.Chat.Title)
		return store.Message{}, false
	}
	if _, allowed := s.chats.ByChat(m.Chat.ID); !allowed {
		slog.Info("message from a chat outside the allowlist dropped",
			"chat_id", m.Chat.ID, "title", m.Chat.Title, "type", m.Chat.Type)
		return store.Message{}, false
	}

	media, hasMedia := m.Media()
	if m.Body() == "" && !hasMedia {
		slog.Debug("service message dropped", "chat_id", m.Chat.ID, "message_id", m.MessageID)
		return store.Message{}, false
	}

	senderID, senderName := m.Sender()
	msg = store.Message{
		ChatID:     m.Chat.ID,
		MessageID:  m.MessageID,
		ThreadID:   m.MessageThreadID,
		Sent:       m.SentAt(),
		SenderID:   senderID,
		SenderName: senderName,
		FromBot:    s.isBot(m.From),
		ReplyTo:    replyTo(m),
		Text:       m.Body(),
		IsMention:  m.MentionsBot(s.botUsername) || s.repliesToBot(m),
	}
	if edited, wasEdited := m.EditedAt(); wasEdited {
		msg.EditedAt = edited
	}
	if hasMedia {
		msg.MediaType, msg.FileID, msg.FileUniqueID = media.Type, media.FileID, media.FileUniqueID
		msg.FileName, msg.FileSize = media.FileName, media.FileSize
	}

	slog.Debug("message accepted", "chat_id", msg.ChatID, "message_id", msg.MessageID,
		"thread_id", msg.ThreadID, "mention", msg.IsMention, "media", msg.MediaType)
	return msg, true
}

// repliesToBot reports whether the message answers one of the bot's own messages, which counts
// as addressing the bot even without an @mention.
func (s *Service) repliesToBot(m *telegram.Message) bool {
	if m.ReplyToMessage == nil {
		return false
	}
	return s.isBot(m.ReplyToMessage.From)
}

func (s *Service) isBot(u *telegram.User) bool {
	switch {
	case u == nil:
		return false
	case s.botID != 0:
		return u.ID == s.botID
	case s.botUsername != "":
		return u.IsBot && strings.EqualFold(u.Username, s.botUsername)
	}
	return false
}

// stopped reports whether shutdown was requested.
func stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// wait sleeps for d or until the context is done; the loop rechecks the context on its next turn.
func (s *Service) wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// backoff doubles the base delay per consecutive failure, capped.
func (s *Service) backoff(attempt int) time.Duration {
	d := s.backoffBase
	for i := 1; i < attempt && d < s.maxBackoff; i++ {
		d *= 2
	}
	return min(d, s.maxBackoff)
}

func replyTo(m *telegram.Message) int64 {
	if m.ReplyToMessage == nil {
		return 0
	}
	return m.ReplyToMessage.MessageID
}

func payload(u telegram.Update) string {
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Sprintf("%+v", u)
	}
	return string(data)
}
