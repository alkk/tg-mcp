package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/store"
)

func TestParseArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		clearEnv(t)
		opts, err := parseArgs(nil)
		require.NoError(t, err)
		assert.Empty(t, opts.Telegram.Token)
		assert.Equal(t, "https://api.telegram.org", opts.Telegram.APIURL)
		assert.False(t, opts.Telegram.Local)
		assert.Empty(t, opts.AuthToken)
		assert.Equal(t, ":8080", opts.Listen)
		assert.Equal(t, "./data", opts.Data)
		assert.Equal(t, "chats.yml", opts.Chats)
		assert.Equal(t, 5*time.Minute, opts.FileLinkTTL)
		assert.Equal(t, 336*time.Hour, opts.PendingTTL)
		assert.False(t, opts.Dbg)
	})

	t.Run("all flags set", func(t *testing.T) {
		clearEnv(t)
		opts, err := parseArgs([]string{
			"--telegram.token=bot-token", "--telegram.api-url=http://localhost:8081", "--telegram.local",
			"--auth-token=secret", "--listen=127.0.0.1:9000", "--data=/tmp/tg", "--chats=/etc/chats.yml",
			"--file-link-ttl=30s", "--pending-ttl=48h", "--dbg",
		})
		require.NoError(t, err)
		assert.Equal(t, "bot-token", opts.Telegram.Token)
		assert.Equal(t, "http://localhost:8081", opts.Telegram.APIURL)
		assert.True(t, opts.Telegram.Local)
		assert.Equal(t, "secret", opts.AuthToken)
		assert.Equal(t, "127.0.0.1:9000", opts.Listen)
		assert.Equal(t, "/tmp/tg", opts.Data)
		assert.Equal(t, "/etc/chats.yml", opts.Chats)
		assert.Equal(t, 30*time.Second, opts.FileLinkTTL)
		assert.Equal(t, 48*time.Hour, opts.PendingTTL)
		assert.True(t, opts.Dbg)
	})

	t.Run("env vars applied", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TELEGRAM_TOKEN", "env-token")
		t.Setenv("TELEGRAM_API_URL", "http://api:8081")
		t.Setenv("TELEGRAM_LOCAL", "true")
		t.Setenv("AUTH_TOKEN", "env-secret")
		t.Setenv("LISTEN", ":7070")
		t.Setenv("DATA_DIR", "/var/lib/tg-mcp")
		t.Setenv("CHATS_FILE", "/cfg/chats.yml")
		t.Setenv("FILE_LINK_TTL", "90s")
		t.Setenv("PENDING_TTL", "72h")
		t.Setenv("DEBUG", "true")

		opts, err := parseArgs(nil)
		require.NoError(t, err)
		assert.Equal(t, "env-token", opts.Telegram.Token)
		assert.Equal(t, "http://api:8081", opts.Telegram.APIURL)
		assert.True(t, opts.Telegram.Local)
		assert.Equal(t, "env-secret", opts.AuthToken)
		assert.Equal(t, ":7070", opts.Listen)
		assert.Equal(t, "/var/lib/tg-mcp", opts.Data)
		assert.Equal(t, "/cfg/chats.yml", opts.Chats)
		assert.Equal(t, 90*time.Second, opts.FileLinkTTL)
		assert.Equal(t, 72*time.Hour, opts.PendingTTL)
		assert.True(t, opts.Dbg)
	})

	t.Run("flag beats env", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TELEGRAM_TOKEN", "env-token")
		t.Setenv("LISTEN", ":7070")

		opts, err := parseArgs([]string{"--telegram.token=flag-token"})
		require.NoError(t, err)
		assert.Equal(t, "flag-token", opts.Telegram.Token)
		assert.Equal(t, ":7070", opts.Listen)
	})

	t.Run("unknown flag", func(t *testing.T) {
		clearEnv(t)
		_, err := parseArgs([]string{"--no-such-flag"})
		require.Error(t, err)
		var flagsErr *flags.Error
		require.ErrorAs(t, err, &flagsErr)
		assert.Equal(t, flags.ErrUnknownFlag, flagsErr.Type)
	})

	t.Run("missing flag value", func(t *testing.T) {
		clearEnv(t)
		_, err := parseArgs([]string{"--listen"})
		require.Error(t, err)
		var flagsErr *flags.Error
		require.ErrorAs(t, err, &flagsErr)
		assert.Equal(t, flags.ErrExpectedArgument, flagsErr.Type)
	})
}

func TestRunStartupFailures(t *testing.T) {
	tests := []struct {
		name    string
		opts    func(t *testing.T, o *options)
		wantErr string
	}{
		{
			name:    "missing telegram token",
			opts:    func(_ *testing.T, o *options) { o.Telegram.Token = "" },
			wantErr: "telegram bot token is required",
		},
		{
			name:    "missing auth token",
			opts:    func(_ *testing.T, o *options) { o.AuthToken = "" },
			wantErr: "mcp auth token is required",
		},
		{
			name:    "negative file link ttl",
			opts:    func(_ *testing.T, o *options) { o.FileLinkTTL = -time.Second },
			wantErr: "file link ttl cannot be negative",
		},
		{
			name:    "negative pending ttl",
			opts:    func(_ *testing.T, o *options) { o.PendingTTL = -time.Second },
			wantErr: "pending ttl cannot be negative",
		},
		{
			name:    "missing chat map",
			opts:    func(t *testing.T, o *options) { o.Chats = filepath.Join(t.TempDir(), "absent.yml") },
			wantErr: "load chat map",
		},
		{
			name: "malformed chat map",
			opts: func(t *testing.T, o *options) {
				o.Chats = writeChats(t, "chats:\n  not-a-number:\n    customer: acme\n")
			},
			wantErr: "load chat map",
		},
		{
			name: "data dir is a file",
			opts: func(t *testing.T, o *options) {
				path := filepath.Join(t.TempDir(), "data")
				require.NoError(t, os.WriteFile(path, []byte("busy"), 0o600))
				o.Data = path
			},
			wantErr: "open store",
		},
		{
			name: "telegram rejects the token",
			opts: func(t *testing.T, o *options) {
				o.Telegram.APIURL = fakeTelegram(t, false).URL
			},
			wantErr: "identify bot",
		},
		{
			name:    "unusable listen address",
			opts:    func(_ *testing.T, o *options) { o.Listen = "127.0.0.1:not-a-port" },
			wantErr: "mcp server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := baseOptions(t)
			tt.opts(t, opts)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := run(ctx, opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateFileLinkTTL(t *testing.T) {
	tests := []struct {
		name    string
		ttl     time.Duration
		wantErr string
	}{
		{name: "zero means the server default", ttl: 0},
		{name: "positive", ttl: time.Minute},
		{name: "negative", ttl: -time.Minute, wantErr: "file link ttl cannot be negative (--file-link-ttl, FILE_LINK_TTL)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &options{AuthToken: "secret", FileLinkTTL: tt.ttl}
			opts.Telegram.Token = "bot-token"

			err := validate(opts)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidatePendingTTL(t *testing.T) {
	tests := []struct {
		name    string
		ttl     time.Duration
		wantErr string
	}{
		{name: "zero means the store default", ttl: 0},
		{name: "positive", ttl: 336 * time.Hour},
		{name: "negative", ttl: -time.Minute, wantErr: "pending ttl cannot be negative (--pending-ttl, PENDING_TTL)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &options{AuthToken: "secret", PendingTTL: tt.ttl}
			opts.Telegram.Token = "bot-token"

			err := validate(opts)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDrainPending(t *testing.T) {
	known := config.Chat{ID: -1001, ChatInfo: config.ChatInfo{Customer: "acme"}}

	t.Run("replays known chats and keeps unknown ones buffered", func(t *testing.T) {
		ctx := context.Background()
		st := newStore(t)
		require.NoError(t, st.UpsertPendingBatch(ctx, []store.Pending{
			pendingMsg(known.ID, 10, "acme hello", time.Now()),
			pendingMsg(-2002, 20, "stranger hello", time.Now()),
		}))

		require.NoError(t, drainPending(ctx, st, []config.Chat{known}, time.Hour))

		msg, err := st.MessageByID(ctx, known.ID, 10)
		require.NoError(t, err)
		assert.Equal(t, "acme hello", msg.Text)

		waiting, err := st.PendingChats(ctx)
		require.NoError(t, err)
		require.Len(t, waiting, 1)
		assert.Equal(t, int64(-2002), waiting[0].ChatID)
		assert.Equal(t, 1, waiting[0].Messages)
	})

	t.Run("replay beats the sweep", func(t *testing.T) {
		ctx := context.Background()
		st := newStore(t)
		stale := time.Now().Add(-30 * 24 * time.Hour)
		require.NoError(t, st.UpsertPendingBatch(ctx, []store.Pending{
			pendingMsg(known.ID, 10, "old but wanted", stale),
			pendingMsg(-2002, 20, "old and unwanted", stale),
		}))

		require.NoError(t, drainPending(ctx, st, []config.Chat{known}, 336*time.Hour))

		msg, err := st.MessageByID(ctx, known.ID, 10)
		require.NoError(t, err)
		assert.Equal(t, "old but wanted", msg.Text)

		waiting, err := st.PendingChats(ctx)
		require.NoError(t, err)
		assert.Empty(t, waiting, "the expired buffer of an unknown chat is swept")
	})

	t.Run("reports the replay and names every chat still missing from the map", func(t *testing.T) {
		ctx := context.Background()
		st := newStore(t)
		oldest := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
		stranger := store.Pending{
			Message:   pendingMsg(-2002, 20, "stranger hello", oldest).Message,
			ChatTitle: "Random Group",
			ChatType:  "supergroup",
			Received:  oldest,
		}
		require.NoError(t, st.UpsertPendingBatch(ctx, []store.Pending{
			pendingMsg(known.ID, 10, "acme hello", time.Now()), stranger,
		}))

		logs := captureLogs(t)
		require.NoError(t, drainPending(ctx, st, []config.Chat{known}, 336*time.Hour))

		replayed := logLines(logs(), "replayed buffered messages")
		require.Len(t, replayed, 1)
		assert.Contains(t, replayed[0], "customer=acme")
		assert.Contains(t, replayed[0], "messages=1")

		waiting := logLines(logs(), "buffered chat not in the allowlist")
		require.Len(t, waiting, 1, "the operator learns the id from this line alone")
		assert.Contains(t, waiting[0], "level=WARN")
		assert.Contains(t, waiting[0], "chat_id=-2002")
		assert.Contains(t, waiting[0], `title="Random Group"`)
		assert.Contains(t, waiting[0], "type=supergroup")
		assert.Contains(t, waiting[0], "messages=1")
		assert.Contains(t, waiting[0], "oldest="+oldest.Format(time.RFC3339))
	})

	t.Run("a store failure is reported, not logged away", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.Close())

		err := drainPending(context.Background(), st, []config.Chat{known}, time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "replay")
	})
}

func TestSweepPendingKeepsRunning(t *testing.T) {
	t.Run("expires what ages past the ttl without a restart", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st := newStore(t)
		require.NoError(t, st.UpsertPendingBatch(ctx, []store.Pending{
			pendingMsg(-2002, 20, "stranger hello", time.Now().Add(-2*time.Hour)),
		}))

		logs := captureLogs(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sweepPending(ctx, st, time.Hour, time.Millisecond)
		}()

		require.Eventually(t, func() bool {
			waiting, err := st.PendingChats(context.Background())
			return err == nil && len(waiting) == 0
		}, 5*time.Second, 10*time.Millisecond)

		cancel()
		<-done
		assert.NotEmpty(t, logLines(logs(), "expired buffered messages removed"))
	})

	t.Run("returns on cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		st := newStore(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sweepPending(ctx, st, time.Hour, time.Millisecond)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("sweepPending ignored the canceled context")
		}
	})
}

// captureLogs points the default logger at a buffer for the duration of the test.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// logLines returns the captured lines carrying the given message.
func logLines(logs, msg string) []string {
	var got []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			got = append(got, line)
		}
	}
	return got
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

func pendingMsg(chatID, messageID int64, text string, received time.Time) store.Pending {
	return store.Pending{
		Message: store.Message{
			ChatID:     chatID,
			MessageID:  messageID,
			Sent:       received,
			SenderID:   7,
			SenderName: "customer",
			Text:       text,
		},
		ChatTitle: "Some Group",
		ChatType:  "group",
		Received:  received,
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	opts := baseOptions(t)
	opts.Listen = freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, opts) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + opts.Listen + "/ping")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "server never came up")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

func baseOptions(t *testing.T) *options {
	t.Helper()
	opts := &options{
		AuthToken: "secret",
		Listen:    "127.0.0.1:0",
		Data:      t.TempDir(),
		Chats:     writeChats(t, "chats:\n  -1001:\n    customer: acme\n"),
	}
	opts.Telegram.Token = "bot-token"
	opts.Telegram.APIURL = fakeTelegram(t, true).URL
	return opts
}

func writeChats(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chats.yml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// fakeTelegram answers the calls run() makes at startup and keeps the poll loop idling.
func fakeTelegram(t *testing.T, authorized bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			if !authorized {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":42,"is_bot":true,"username":"support_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			select { // a short poll: long enough not to spin, short enough never to block Close
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// freeAddr picks a port the OS just handed out, so the test can reach the server it started.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestResolveVersion(t *testing.T) {
	t.Run("injected revision", func(t *testing.T) {
		orig := revision
		t.Cleanup(func() { revision = orig })
		revision = "master-abc1234-20260730T120000"
		assert.Equal(t, "master-abc1234-20260730T120000", resolveVersion())
	})

	t.Run("no revision falls back to build info", func(t *testing.T) {
		orig := revision
		t.Cleanup(func() { revision = orig })
		revision = "unknown"
		assert.NotEmpty(t, resolveVersion())
	})
}

func TestSetupLog(t *testing.T) {
	ctx := context.Background()

	t.Run("info level by default", func(t *testing.T) {
		setupLog(false)
		assert.False(t, slog.Default().Enabled(ctx, slog.LevelDebug))
		assert.True(t, slog.Default().Enabled(ctx, slog.LevelInfo))
	})

	t.Run("debug level with dbg", func(t *testing.T) {
		setupLog(true)
		assert.True(t, slog.Default().Enabled(ctx, slog.LevelDebug))
	})
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"TELEGRAM_TOKEN", "TELEGRAM_API_URL", "TELEGRAM_LOCAL",
		"AUTH_TOKEN", "LISTEN", "DATA_DIR", "CHATS_FILE", "FILE_LINK_TTL", "PENDING_TTL", "DEBUG",
	} {
		if v, ok := os.LookupEnv(k); ok {
			t.Setenv(k, v) // registers restore on cleanup
			require.NoError(t, os.Unsetenv(k))
		}
	}
}
