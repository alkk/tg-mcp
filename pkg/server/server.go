// Package server exposes the message store to MCP clients: a streamable HTTP endpoint at /mcp
// guarded by a static bearer token, plus an unauthenticated /ping health check. Every tool
// addresses chats by customer slug and optional group label — raw telegram chat ids never leave
// the process.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

//go:generate moq -out mocks/message_store.go -pkg mocks -skip-ensure -fmt goimports . messageStore:MessageStore
//go:generate moq -out mocks/telegram_api.go -pkg mocks -skip-ensure -fmt goimports . telegramAPI:TelegramAPI

// messageStore is the slice of the store the tools need.
type messageStore interface {
	ListNew(ctx context.Context, chatIDs []int64, limit int) ([]store.Message, error)
	History(ctx context.Context, chatIDs []int64, from, to time.Time, before *store.HistoryCursor,
		limit int) ([]store.Message, error)
	Thread(ctx context.Context, chatID, messageID int64) (store.Thread, error)
	Search(ctx context.Context, query string, chatIDs []int64, from, to time.Time, limit int) ([]store.SearchHit, error)
	MessageByID(ctx context.Context, chatID, messageID int64) (store.Message, error)
	UpsertMessage(ctx context.Context, m store.Message) error
	UnreadCounts(ctx context.Context, chatIDs []int64) (map[int64]int, error)
	SetCursor(ctx context.Context, chatID, messageID int64) (int64, error)
	SaveFile(fileUniqueID string, write func(w io.Writer) error) (string, error)
	Cached(fileUniqueID string) (path string, ok bool)
}

// telegramAPI is the slice of the bot api the action tools need.
type telegramAPI interface {
	SendMessage(ctx context.Context, chatID int64, text, parseMode string, replyTo, threadID int64) (telegram.Message, error)
	GetFile(ctx context.Context, fileID string) (telegram.File, error)
	Download(ctx context.Context, filePath string, dst io.Writer) error
}

const (
	serverName      = "tg-mcp"
	shutdownTimeout = 5 * time.Second
)

// Params configures the MCP server; Store, Chats and AuthToken are mandatory.
type Params struct {
	Store     messageStore
	Telegram  telegramAPI
	Chats     *config.Config
	AuthToken string
	Listen    string
	Version   string
}

// Server serves MCP over streamable HTTP.
type Server struct {
	store    messageStore
	telegram telegramAPI
	chats    *config.Config
	token    string
	listen   string
	mcp      *mcp.Server

	mu   sync.RWMutex
	addr net.Addr
}

// New creates the MCP server and registers its tools.
func New(p Params) (*Server, error) {
	switch {
	case p.AuthToken == "":
		return nil, errors.New("auth token is required")
	case p.Store == nil:
		return nil, errors.New("store is required")
	case p.Chats == nil:
		return nil, errors.New("chat map is required")
	}

	version := p.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{
		store:    p.Store,
		telegram: p.Telegram,
		chats:    p.Chats,
		token:    p.AuthToken,
		listen:   p.Listen,
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)
	s.registerTools()
	return s, nil
}

// Handler builds the HTTP routing: /mcp behind bearer auth, /ping open for health checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "pong")
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
	mux.Handle("/mcp", s.auth(s.trackBase(mcpHandler)))
	mux.Handle("GET "+filesRoute+"{id}", s.auth(http.HandlerFunc(s.serveFile)))
	return mux
}

// Run serves until the context is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", s.listen, err)
	}
	s.setAddr(ln.Addr())
	slog.Info("mcp server listening", "addr", ln.Addr().String())

	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// draining outlives the canceled context it was triggered by
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown failed", "err", err)
		}
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	<-stopped
	slog.Info("mcp server stopped")
	return nil
}

// Addr returns the address the server bound to, nil before Run listens. Useful when the
// configured address ends in :0.
func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

func (s *Server) setAddr(addr net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = addr
}

// auth rejects anything without the configured bearer token, telling the client nothing beyond
// the required scheme.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			slog.Warn("unauthorized request", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trackBase pins the externally visible base url onto the mcp request itself, so the tool call it
// carries builds its download urls from the host that very client reached us through — a second
// client on another hostname cannot repoint them mid-call, and a forwarded header only ever
// affects the request that carried it. Set unconditionally: a client-supplied value never survives.
func (s *Server) trackBase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if base := requestBase(r); base != "" {
			r.Header.Set(baseHeader, base)
		} else {
			r.Header.Del(baseHeader)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.token)) == 1
}

// UnknownCustomerError is returned for a slug that is not in the chat map.
type UnknownCustomerError struct{ Customer string }

func (e *UnknownCustomerError) Error() string {
	return fmt.Sprintf("unknown customer %q", e.Customer)
}

// AmbiguousChatError is returned when a customer has several groups and no label picks one.
type AmbiguousChatError struct {
	Customer string
	Labels   []string
}

func (e *AmbiguousChatError) Error() string {
	return fmt.Sprintf("customer %q has %d groups, pass label: %s",
		e.Customer, len(e.Labels), strings.Join(e.Labels, ", "))
}

// UnknownLabelError is returned when a label matches none of the customer's groups.
type UnknownLabelError struct {
	Customer string
	Label    string
	Labels   []string
}

func (e *UnknownLabelError) Error() string {
	return fmt.Sprintf("customer %q has no group labeled %q, available labels: %s",
		e.Customer, e.Label, strings.Join(e.Labels, ", "))
}

// customerChats returns every group of a customer, ordered by label.
func (s *Server) customerChats(customer string) ([]config.Chat, error) {
	chats := s.chats.ByCustomer(customer)
	if len(chats) == 0 {
		return nil, &UnknownCustomerError{Customer: customer}
	}
	return chats, nil
}

// singleChat resolves a customer and an optional label to exactly one group: the only group of
// the customer when no label is given, otherwise the one carrying it.
func (s *Server) singleChat(customer, label string) (config.Chat, error) {
	chats, err := s.customerChats(customer)
	if err != nil {
		return config.Chat{}, err
	}
	if label == "" {
		if len(chats) == 1 {
			return chats[0], nil
		}
		return config.Chat{}, &AmbiguousChatError{Customer: customer, Labels: chatLabels(chats)}
	}
	for _, c := range chats {
		if c.Label == label {
			return c, nil
		}
	}
	return config.Chat{}, &UnknownLabelError{Customer: customer, Label: label, Labels: chatLabels(chats)}
}

// chatIDs resolves the chats to read from: every allowlisted chat when the customer is empty,
// every group of the customer when only the slug is given, one group when a label is given too.
func (s *Server) chatIDs(customer, label string) ([]int64, error) {
	if customer == "" {
		if label != "" {
			return nil, errors.New("label needs a customer")
		}
		return chatIDsOf(s.chats.All()), nil
	}
	chats, err := s.customerChats(customer)
	if err != nil {
		return nil, err
	}
	if label == "" {
		return chatIDsOf(chats), nil
	}
	chat, err := s.singleChat(customer, label)
	if err != nil {
		return nil, err
	}
	return []int64{chat.ID}, nil
}

// chatLabels renders the labels of a customer's groups for an error message; an unlabeled group
// is only possible when it is the customer's only one.
func chatLabels(chats []config.Chat) []string {
	res := make([]string, 0, len(chats))
	for _, c := range chats {
		if c.Label == "" {
			res = append(res, `""`)
			continue
		}
		res = append(res, c.Label)
	}
	return res
}

func chatIDsOf(chats []config.Chat) []int64 {
	res := make([]int64, 0, len(chats))
	for _, c := range chats {
		res = append(res, c.ID)
	}
	return res
}
