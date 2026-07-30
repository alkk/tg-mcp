package main

import (
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
		assert.False(t, opts.Dbg)
	})

	t.Run("all flags set", func(t *testing.T) {
		clearEnv(t)
		opts, err := parseArgs([]string{
			"--telegram.token=bot-token", "--telegram.api-url=http://localhost:8081", "--telegram.local",
			"--auth-token=secret", "--listen=127.0.0.1:9000", "--data=/tmp/tg", "--chats=/etc/chats.yml", "--dbg",
		})
		require.NoError(t, err)
		assert.Equal(t, "bot-token", opts.Telegram.Token)
		assert.Equal(t, "http://localhost:8081", opts.Telegram.APIURL)
		assert.True(t, opts.Telegram.Local)
		assert.Equal(t, "secret", opts.AuthToken)
		assert.Equal(t, "127.0.0.1:9000", opts.Listen)
		assert.Equal(t, "/tmp/tg", opts.Data)
		assert.Equal(t, "/etc/chats.yml", opts.Chats)
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
		"AUTH_TOKEN", "LISTEN", "DATA_DIR", "CHATS_FILE", "DEBUG",
	} {
		if v, ok := os.LookupEnv(k); ok {
			t.Setenv(k, v) // registers restore on cleanup
			require.NoError(t, os.Unsetenv(k))
		}
	}
}
