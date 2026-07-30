package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

const (
	listNewDefaultLimit = 100
	historyDefaultLimit = 200
	searchDefaultLimit  = 50
	// maxLimit caps what any listing tool returns in one call, whatever the caller asked for.
	maxLimit = 1000
	// historyWindow is how far back get_history looks when neither bound is given.
	historyWindow = 24 * time.Hour
	// snippetRunes bounds the excerpt of listings that show a preview instead of the full text.
	snippetRunes = 200
	ellipsis     = "…"
	// maxReplyRunes is the telegram limit for a single text message.
	maxReplyRunes = 4096
	// logSentTimeout bounds the write that logs a reply telegram already delivered; it runs on a
	// detached context, so it needs a deadline of its own.
	logSentTimeout = 5 * time.Second
)

// messageView is a stored message as the tools hand it out: telegram chat ids stay inside, the
// group is named by customer slug and label instead.
type messageView struct {
	MessageID int64  `json:"message_id"`
	Customer  string `json:"customer"`
	Label     string `json:"label,omitempty"`
	Sent      string `json:"sent"`
	Sender    string `json:"sender"`
	SenderID  int64  `json:"sender_id,omitempty"`
	FromBot   bool   `json:"from_bot,omitempty"`
	ReplyTo   int64  `json:"reply_to,omitempty"`
	ThreadID  int64  `json:"thread_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
	Mention   bool   `json:"mention,omitempty"`
	EditedAt  string `json:"edited_at,omitempty"`
	Media     string `json:"media,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
}

// messagesResult is what every listing tool returns; Truncated is only ever set by get_thread and
// NextCursor only by get_history.
type messagesResult struct {
	Messages   []messageView `json:"messages"`
	Truncated  bool          `json:"truncated,omitempty"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type groupView struct {
	Label  string `json:"label,omitempty"`
	Unread int    `json:"unread"`
}

type customerView struct {
	Customer string      `json:"customer"`
	Groups   []groupView `json:"groups"`
	Unread   int         `json:"unread"`
}

type listCustomersResult struct {
	Customers []customerView `json:"customers"`
}

type listNewParams struct {
	Customer string `json:"customer,omitempty" jsonschema:"customer slug, omit for every allowlisted group"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of messages, default 100, capped at 1000"`
}

type getThreadParams struct {
	Customer  string `json:"customer" jsonschema:"customer slug"`
	MessageID int64  `json:"message_id" jsonschema:"telegram message id to reconstruct the conversation around"`
	Label     string `json:"label,omitempty" jsonschema:"group label, required when the customer has several groups"`
}

type getHistoryParams struct {
	Customer string `json:"customer" jsonschema:"customer slug"`
	From     string `json:"from,omitempty" jsonschema:"start of the range as RFC3339, inclusive"`
	To       string `json:"to,omitempty" jsonschema:"end of the range as RFC3339, inclusive"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of messages, default 200, capped at 1000"`
	Label    string `json:"label,omitempty" jsonschema:"group label, required when the customer has several groups"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page, to read the messages before it; keep the other parameters as they were"`
}

type searchParams struct {
	Query    string `json:"query" jsonschema:"words to look for; quote a phrase to keep it together"`
	Customer string `json:"customer,omitempty" jsonschema:"customer slug, omit to search every allowlisted group"`
	From     string `json:"from,omitempty" jsonschema:"start of the range as RFC3339, inclusive"`
	To       string `json:"to,omitempty" jsonschema:"end of the range as RFC3339, inclusive"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of hits, default 50, capped at 1000"`
}

type sendReplyParams struct {
	Customer string `json:"customer" jsonschema:"customer slug"`
	Text     string `json:"text" jsonschema:"plain text of the reply, no markdown, at most 4096 characters"`
	ReplyTo  int64  `json:"reply_to,omitempty" jsonschema:"telegram message id to answer; the reply inherits its forum topic"`
	Label    string `json:"label,omitempty" jsonschema:"group label, needed when the customer has several groups and reply_to does not pin one"`
}

// sendReplyResult echoes the message as it was logged; Warning is set when it went out but could
// not be stored.
type sendReplyResult struct {
	Message messageView `json:"message"`
	Warning string      `json:"warning,omitempty"`
}

type markHandledParams struct {
	Customer  string `json:"customer" jsonschema:"customer slug"`
	MessageID int64  `json:"message_id" jsonschema:"telegram message id to mark as triaged, together with everything before it"`
	Label     string `json:"label,omitempty" jsonschema:"group label, required when the customer has several groups"`
}

type markHandledResult struct {
	Customer   string `json:"customer"`
	Label      string `json:"label,omitempty"`
	MarkedUpTo int64  `json:"marked_up_to"`
}

// registerTools wires the tools onto the MCP server.
func (s *Server) registerTools() {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "list_customers",
		Description: "List the customers tg-mcp logs, their telegram groups and how many messages " +
			"in each are waiting for triage.",
		Annotations: readOnly,
	}, s.listCustomers)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "list_new",
		Description: "List messages logged since the triage cursor, oldest first, with a short " +
			"excerpt and mention/media flags. Own replies are never listed.",
		Annotations: readOnly,
	}, s.listNew)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_thread",
		Description: "Reconstruct the conversation around a message: its reply chain, every reply " +
			"hanging off it, and everything the group said in the same time span.",
		Annotations: readOnly,
	}, s.getThread)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_history",
		Description: "Read a group's messages in chronological order; defaults to the last 24 " +
			"hours. A full page comes with next_cursor — pass it back as cursor to read what " +
			"came before it.",
		Annotations: readOnly,
	}, s.getHistory)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search",
		Description: "Full-text search over logged messages, newest first, returning excerpts and message ids.",
		Annotations: readOnly,
	}, s.search)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "get_file",
		Description: "Fetch the attachment of a message. Images and text come back in the result; " +
			"anything larger is downloaded from the returned url with the same bearer token.",
		Annotations: readOnly,
	}, s.getFile)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "send_reply",
		Description: "Post a plain text message into a customer group as the bot. Answering a " +
			"message keeps the conversation together and lands in its forum topic.",
	}, s.sendReply)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "mark_handled",
		Description: "Move the triage cursor of a group up to a message, so list_new stops " +
			"reporting it and everything before it.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, s.markHandled)
}

func (s *Server) sendReply(ctx context.Context, _ *mcp.CallToolRequest,
	in sendReplyParams) (*mcp.CallToolResult, sendReplyResult, error) {
	text := strings.TrimSpace(in.Text)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return nil, sendReplyResult{}, errors.New("reply text is empty")
	case n > maxReplyRunes:
		return nil, sendReplyResult{}, fmt.Errorf(
			"reply is %d characters, telegram caps a message at %d: split it into several replies",
			n, maxReplyRunes)
	}
	if s.telegram == nil {
		return nil, sendReplyResult{}, errors.New("no telegram client configured, replies cannot be sent")
	}

	chat, target, err := s.replyTarget(ctx, in.Customer, in.Label, in.ReplyTo)
	if err != nil {
		return nil, sendReplyResult{}, err
	}

	sent, err := s.telegram.SendMessage(ctx, chat.ID, text, in.ReplyTo, target.ThreadID)
	if err != nil {
		return nil, sendReplyResult{}, fmt.Errorf("send reply to customer %q: %w", chat.Customer, err)
	}
	slog.Info("reply sent", "customer", chat.Customer, "label", chat.Label, "chat_id", chat.ID,
		"message_id", sent.MessageID, "reply_to", in.ReplyTo, "thread_id", target.ThreadID, "text", text)

	// the message is out; logging it must not ride on the caller's context. A client that hung up
	// between delivery and this write would leave the thread without its answer for good, and
	// getUpdates never echoes our own messages back.
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logSentTimeout)
	defer cancel()

	msg := sentMessage(sent, chat.ID, in.ReplyTo, text)
	var res sendReplyResult
	if err := s.store.UpsertMessage(logCtx, msg); err != nil {
		slog.Error("sent reply not logged", "err", err, "chat_id", chat.ID, "message_id", msg.MessageID)
		res.Warning = "the reply was delivered but could not be logged: " + err.Error()
	}
	res.Message = view(msg, s.chatNamer())
	return nil, res, nil
}

// replyTarget resolves the group a reply goes to and the message it answers. A reply_to pins the
// group on its own when the customer has several and no label was given: the message id has to
// exist in exactly one of them.
func (s *Server) replyTarget(ctx context.Context, customer, label string,
	replyTo int64) (config.Chat, store.Message, error) {
	chat, err := s.singleChat(customer, label)
	var ambiguous *AmbiguousChatError
	switch {
	case err == nil:
		if replyTo == 0 {
			return chat, store.Message{}, nil
		}
		target, lookupErr := s.store.MessageByID(ctx, chat.ID, replyTo)
		if lookupErr != nil {
			return config.Chat{}, store.Message{}, fmt.Errorf("look up message %d: %w", replyTo, lookupErr)
		}
		return chat, target, nil
	case errors.As(err, &ambiguous) && replyTo > 0:
		return s.chatOfMessage(ctx, customer, replyTo, ambiguous)
	}
	return config.Chat{}, store.Message{}, err
}

// chatOfMessage finds which of a customer's groups holds a message id.
func (s *Server) chatOfMessage(ctx context.Context, customer string, messageID int64,
	ambiguous *AmbiguousChatError) (config.Chat, store.Message, error) {
	chats, err := s.customerChats(customer)
	if err != nil {
		return config.Chat{}, store.Message{}, err
	}

	var found []config.Chat
	var msg store.Message
	for _, c := range chats {
		m, err := s.store.MessageByID(ctx, c.ID, messageID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return config.Chat{}, store.Message{}, fmt.Errorf("look up message %d: %w", messageID, err)
		}
		found, msg = append(found, c), m
	}

	switch len(found) {
	case 1:
		return found[0], msg, nil
	case 0:
		return config.Chat{}, store.Message{},
			fmt.Errorf("message %d in any group of customer %q: %w", messageID, customer, store.ErrNotFound)
	}
	return config.Chat{}, store.Message{}, ambiguous
}

// sentMessage maps the message telegram created back into a stored one. The Bot API never echoes
// the bot's own messages through getUpdates, so this is the only way threads keep their answers.
func sentMessage(m telegram.Message, chatID, replyTo int64, text string) store.Message {
	senderID, senderName := m.Sender()
	msg := store.Message{
		ChatID:     chatID,
		MessageID:  m.MessageID,
		ThreadID:   m.MessageThreadID,
		Sent:       time.Now().UTC(),
		SenderID:   senderID,
		SenderName: senderName,
		FromBot:    true,
		ReplyTo:    replyTo,
		Text:       text,
	}
	if m.Chat.ID != 0 {
		msg.ChatID = m.Chat.ID
	}
	if m.Date != 0 {
		msg.Sent = m.SentAt()
	}
	if body := m.Body(); body != "" {
		msg.Text = body
	}
	if m.ReplyToMessage != nil {
		msg.ReplyTo = m.ReplyToMessage.MessageID
	}
	return msg
}

func (s *Server) markHandled(ctx context.Context, _ *mcp.CallToolRequest,
	in markHandledParams) (*mcp.CallToolResult, markHandledResult, error) {
	chat, err := s.singleChat(in.Customer, in.Label)
	if err != nil {
		return nil, markHandledResult{}, err
	}
	if _, err = s.store.MessageByID(ctx, chat.ID, in.MessageID); err != nil {
		return nil, markHandledResult{}, fmt.Errorf("look up message %d: %w", in.MessageID, err)
	}
	at, err := s.store.SetCursor(ctx, chat.ID, in.MessageID)
	if err != nil {
		return nil, markHandledResult{}, fmt.Errorf("advance triage cursor: %w", err)
	}

	customer, label := s.chatNamer()(chat.ID)
	return nil, markHandledResult{Customer: customer, Label: label, MarkedUpTo: at}, nil
}

func (s *Server) listCustomers(ctx context.Context, _ *mcp.CallToolRequest,
	_ struct{}) (*mcp.CallToolResult, listCustomersResult, error) {
	chats := s.chats.All()
	unread, err := s.store.UnreadCounts(ctx, chatIDsOf(chats))
	if err != nil {
		return nil, listCustomersResult{}, fmt.Errorf("read unread counts: %w", err)
	}

	res := listCustomersResult{Customers: []customerView{}}
	idx := map[string]int{}
	for _, c := range chats {
		i, ok := idx[c.Customer]
		if !ok {
			i = len(res.Customers)
			idx[c.Customer] = i
			res.Customers = append(res.Customers, customerView{Customer: c.Customer})
		}
		n := unread[c.ID]
		res.Customers[i].Groups = append(res.Customers[i].Groups, groupView{Label: c.Label, Unread: n})
		res.Customers[i].Unread += n
	}
	return nil, res, nil
}

func (s *Server) listNew(ctx context.Context, _ *mcp.CallToolRequest,
	in listNewParams) (*mcp.CallToolResult, messagesResult, error) {
	ids, err := s.chatIDs(in.Customer, "")
	if err != nil {
		return nil, messagesResult{}, err
	}
	msgs, err := s.store.ListNew(ctx, ids, limitOr(in.Limit, listNewDefaultLimit))
	if err != nil {
		return nil, messagesResult{}, fmt.Errorf("list new messages: %w", err)
	}
	return nil, messagesResult{Messages: s.snippetViews(msgs)}, nil
}

func (s *Server) getThread(ctx context.Context, _ *mcp.CallToolRequest,
	in getThreadParams) (*mcp.CallToolResult, messagesResult, error) {
	chat, err := s.singleChat(in.Customer, in.Label)
	if err != nil {
		return nil, messagesResult{}, err
	}
	thread, err := s.store.Thread(ctx, chat.ID, in.MessageID)
	if err != nil {
		return nil, messagesResult{}, fmt.Errorf("reconstruct thread: %w", err)
	}
	return nil, messagesResult{Messages: s.views(thread.Messages), Truncated: thread.Truncated}, nil
}

func (s *Server) getHistory(ctx context.Context, _ *mcp.CallToolRequest,
	in getHistoryParams) (*mcp.CallToolResult, messagesResult, error) {
	if in.Customer == "" {
		// an empty slug means "every group" to chatIDs; history is always read per customer
		return nil, messagesResult{}, &UnknownCustomerError{Customer: in.Customer}
	}
	ids, err := s.chatIDs(in.Customer, in.Label)
	if err != nil {
		return nil, messagesResult{}, err
	}
	from, to, err := timeRange(in.From, in.To)
	if err != nil {
		return nil, messagesResult{}, err
	}
	before, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, messagesResult{}, err
	}
	if from.IsZero() && to.IsZero() && before == nil {
		from = time.Now().Add(-historyWindow)
	}

	limit := limitOr(in.Limit, historyDefaultLimit)
	msgs, err := s.store.History(ctx, ids, from, to, before, limit)
	if err != nil {
		return nil, messagesResult{}, fmt.Errorf("read history: %w", err)
	}

	res := messagesResult{Messages: s.views(msgs)}
	if len(msgs) == limit {
		// the page is full, so older messages may be waiting; msgs are chronological, [0] is the
		// oldest and the next page picks up strictly before it
		res.NextCursor = encodeCursor(msgs[0])
	}
	return nil, res, nil
}

// encodeCursor marks where a history page stopped. The token is opaque on purpose — it carries a
// store row id that means nothing outside the process, and callers only ever hand it back.
func encodeCursor(m store.Message) string {
	raw := m.Sent.UTC().Format(time.RFC3339) + "|" + strconv.FormatInt(m.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (*store.HistoryCursor, error) {
	if token == "" {
		return nil, nil //nolint:nilnil // no cursor is not an error, and the store takes a nil one
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("cursor %q is not a token get_history handed out", token)
	}
	sent, id, found := strings.Cut(string(raw), "|")
	if !found {
		return nil, fmt.Errorf("cursor %q is not a token get_history handed out", token)
	}
	at, err := time.Parse(time.RFC3339, sent)
	if err != nil {
		return nil, fmt.Errorf("parse the timestamp of cursor %q: %w", token, err)
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse the position of cursor %q: %w", token, err)
	}
	return &store.HistoryCursor{Sent: at, ID: rowID}, nil
}

func (s *Server) search(ctx context.Context, _ *mcp.CallToolRequest,
	in searchParams) (*mcp.CallToolResult, messagesResult, error) {
	ids, err := s.chatIDs(in.Customer, "")
	if err != nil {
		return nil, messagesResult{}, err
	}
	from, to, err := timeRange(in.From, in.To)
	if err != nil {
		return nil, messagesResult{}, err
	}

	hits, err := s.store.Search(ctx, in.Query, ids, from, to, limitOr(in.Limit, searchDefaultLimit))
	if err != nil {
		return nil, messagesResult{}, fmt.Errorf("search messages: %w", err)
	}
	return nil, messagesResult{Messages: s.hitViews(hits)}, nil
}

// views renders messages with their full text.
func (s *Server) views(msgs []store.Message) []messageView {
	name := s.chatNamer()
	res := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		res = append(res, view(m, name))
	}
	return res
}

// snippetViews renders messages with a bounded excerpt instead of the full text.
func (s *Server) snippetViews(msgs []store.Message) []messageView {
	res := s.views(msgs)
	for i := range res {
		res[i].Snippet, res[i].Text = truncate(res[i].Text, snippetRunes), ""
	}
	return res
}

// hitViews renders search results, keeping the excerpt the store built around the match.
func (s *Server) hitViews(hits []store.SearchHit) []messageView {
	name := s.chatNamer()
	res := make([]messageView, 0, len(hits))
	for _, h := range hits {
		v := view(h.Message, name)
		v.Snippet, v.Text = h.Snippet, ""
		res = append(res, v)
	}
	return res
}

func view(m store.Message, name func(int64) (string, string)) messageView {
	customer, label := name(m.ChatID)
	v := messageView{
		MessageID: m.MessageID,
		Customer:  customer,
		Label:     label,
		Sent:      m.Sent.Format(time.RFC3339),
		Sender:    m.SenderName,
		SenderID:  m.SenderID,
		FromBot:   m.FromBot,
		ReplyTo:   m.ReplyTo,
		ThreadID:  m.ThreadID,
		Text:      m.Text,
		Mention:   m.IsMention,
	}
	if !m.EditedAt.IsZero() {
		v.EditedAt = m.EditedAt.Format(time.RFC3339)
	}
	if m.HasMedia() {
		v.Media, v.FileName, v.FileSize = m.MediaType, m.FileName, m.FileSize
	}
	return v
}

// chatNamer resolves a chat id to its customer slug and the label to show. The label is left out
// for customers with a single group, where it carries no information.
func (s *Server) chatNamer() func(chatID int64) (customer, label string) {
	groups := map[string]int{}
	for _, c := range s.chats.All() {
		groups[c.Customer]++
	}
	return func(chatID int64) (string, string) {
		info, ok := s.chats.ByChat(chatID)
		if !ok {
			return "", ""
		}
		if groups[info.Customer] > 1 {
			return info.Customer, info.Label
		}
		return info.Customer, ""
	}
}

// timeRange parses the optional RFC3339 bounds of a tool call.
func timeRange(from, to string) (fromT, toT time.Time, err error) {
	if fromT, err = parseBound("from", from); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if toT, err = parseBound("to", to); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return fromT, toT, nil
}

func parseBound(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s %q as RFC3339: %w", name, value, err)
	}
	return t, nil
}

// limitOr resolves the caller's limit, substituting the tool default for a non-positive value and
// capping it at maxLimit: the result is built in memory three times over (rows, views, json), so an
// unbounded limit from a caller that guessed lets one call pull the whole store into the heap.
func limitOr(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, maxLimit)
}

func truncate(s string, runes int) string {
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	return strings.TrimRight(string(r[:runes]), " ") + ellipsis
}
