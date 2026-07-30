package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/server/mocks"
)

const testToken = "s3cret"

// chatMap with acme as a single-group customer and globex holding two labeled groups.
const chatMap = `
chats:
  -1001:
    customer: acme
  -1002:
    customer: globex
    label: main
  -1003:
    customer: globex
    label: escalations
`

func testConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chats.yml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	return cfg
}

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Params{Store: &mocks.MessageStore{}, Telegram: &mocks.TelegramAPI{},
		Chats: testConfig(t, chatMap), AuthToken: testToken, Listen: "127.0.0.1:0"})
	require.NoError(t, err)
	return s
}

func TestNew(t *testing.T) {
	cfg := testConfig(t, chatMap)
	tests := []struct {
		name    string
		params  Params
		wantErr string
	}{
		{
			name:   "all mandatory params",
			params: Params{Store: &mocks.MessageStore{}, Chats: cfg, AuthToken: testToken},
		},
		{
			name:    "no auth token",
			params:  Params{Store: &mocks.MessageStore{}, Chats: cfg},
			wantErr: "auth token is required",
		},
		{
			name:    "no store",
			params:  Params{Chats: cfg, AuthToken: testToken},
			wantErr: "store is required",
		},
		{
			name:    "no chat map",
			params:  Params{Store: &mocks.MessageStore{}, AuthToken: testToken},
			wantErr: "chat map is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.params)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, s)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, s.mcp)
		})
	}
}

func TestServerPing(t *testing.T) {
	srv := httptest.NewServer(newServer(t).Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/ping")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("WWW-Authenticate"), "health check is not behind auth")
}

func TestServerAuth(t *testing.T) {
	s := newServer(t)
	protected := s.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "valid token", header: "Bearer " + testToken, wantStatus: http.StatusTeapot},
		{name: "scheme is case insensitive", header: "bearer " + testToken, wantStatus: http.StatusTeapot},
		{name: "no header", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "token prefix only", header: "Bearer s3c", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic " + testToken, wantStatus: http.StatusUnauthorized},
		{name: "bare token", header: testToken, wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", http.NoBody)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantStatus == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
				assert.NotContains(t, rec.Body.String(), testToken)
			}
		})
	}
}

// TestServerMCPTransport drives the real MCP handshake over streamable HTTP, with and without
// the bearer token.
func TestServerMCPTransport(t *testing.T) {
	srv := httptest.NewServer(newServer(t).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("connects with token", func(t *testing.T) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: srv.URL + "/mcp", HTTPClient: bearerClient(testToken)}, nil)
		require.NoError(t, err)
		defer func() { _ = session.Close() }()

		res, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
			assert.NotEmpty(t, tool.Description, "tool %q has no description", tool.Name)
		}
		assert.Equal(t, []string{"get_file", "get_history", "get_thread", "list_customers", "list_new",
			"mark_handled", "search", "send_reply"}, names)
	})

	t.Run("rejected without token", func(t *testing.T) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
		_, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: srv.URL + "/mcp", MaxRetries: -1}, nil)
		require.Error(t, err)
	})
}

func TestServerRun(t *testing.T) {
	t.Run("serves until context is canceled", func(t *testing.T) {
		s := newServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		require.Eventually(t, func() bool { return s.Addr() != nil }, time.Second, 5*time.Millisecond)
		resp, err := http.Get("http://" + s.Addr().String() + "/ping")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("server did not shut down")
		}
	})

	t.Run("bad listen address", func(t *testing.T) {
		s := newServer(t)
		s.listen = "127.0.0.1:not-a-port"
		require.ErrorContains(t, s.Run(context.Background()), "listen")
	})
}

func TestServerSingleChat(t *testing.T) {
	s := newServer(t)
	unknownCustomer := func(err error) bool {
		var e *UnknownCustomerError
		return errors.As(err, &e)
	}
	ambiguous := func(err error) bool {
		var e *AmbiguousChatError
		return errors.As(err, &e)
	}
	unknownLabel := func(err error) bool {
		var e *UnknownLabelError
		return errors.As(err, &e)
	}

	tests := []struct {
		name     string
		customer string
		label    string
		wantID   int64
		wantErr  func(error) bool
	}{
		{name: "single group customer", customer: "acme", wantID: -1001},
		{name: "labeled group", customer: "globex", label: "main", wantID: -1002},
		{name: "other labeled group", customer: "globex", label: "escalations", wantID: -1003},
		{name: "unknown customer", customer: "initech", wantErr: unknownCustomer},
		{name: "empty customer", wantErr: unknownCustomer},
		{name: "several groups, no label", customer: "globex", wantErr: ambiguous},
		{name: "unknown label", customer: "globex", label: "billing", wantErr: unknownLabel},
		{name: "label on single group customer", customer: "acme", label: "main", wantErr: unknownLabel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat, err := s.singleChat(tt.customer, tt.label)
			if tt.wantErr != nil {
				require.Error(t, err)
				assert.True(t, tt.wantErr(err), "unexpected error type: %T", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, chat.ID)
		})
	}
}

func TestServerSingleChatErrorMessages(t *testing.T) {
	s := newServer(t)

	_, err := s.singleChat("initech", "")
	require.EqualError(t, err, `unknown customer "initech"`)

	_, err = s.singleChat("globex", "")
	require.EqualError(t, err, `customer "globex" has 2 groups, pass label: escalations, main`)

	_, err = s.singleChat("globex", "billing")
	require.EqualError(t, err, `customer "globex" has no group labeled "billing", available labels: escalations, main`)

	_, err = s.singleChat("acme", "main")
	require.EqualError(t, err, `customer "acme" has no group labeled "main", available labels: ""`)
}

func TestServerChatIDs(t *testing.T) {
	s := newServer(t)
	tests := []struct {
		name     string
		customer string
		label    string
		want     []int64
		wantErr  string
	}{
		{name: "no customer means every allowlisted chat", want: []int64{-1001, -1003, -1002}},
		{name: "single group customer", customer: "acme", want: []int64{-1001}},
		{name: "every group of a customer", customer: "globex", want: []int64{-1003, -1002}},
		{name: "one labeled group", customer: "globex", label: "main", want: []int64{-1002}},
		{name: "label without customer", label: "main", wantErr: "label needs a customer"},
		{name: "unknown customer", customer: "initech", wantErr: `unknown customer "initech"`},
		{name: "unknown label", customer: "globex", label: "billing", wantErr: "available labels"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := s.chatIDs(tt.customer, tt.label)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, ids)
		})
	}
}

func TestServerChatIDsEmptyChatMap(t *testing.T) {
	s, err := New(Params{Store: &mocks.MessageStore{}, Chats: testConfig(t, "chats:\n"), AuthToken: testToken})
	require.NoError(t, err)

	ids, err := s.chatIDs("", "")
	require.NoError(t, err)
	assert.Empty(t, ids)

	var unknown *UnknownCustomerError
	_, err = s.chatIDs("acme", "")
	require.ErrorAs(t, err, &unknown)
}

// bearerClient returns an http client attaching the bearer token to every request.
func bearerClient(token string) *http.Client {
	return &http.Client{Transport: bearerTransport{token: token}}
}

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone) //nolint:wrapcheck // transport errors pass through
}
