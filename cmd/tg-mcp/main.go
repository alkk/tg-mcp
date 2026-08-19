package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"golang.org/x/sync/errgroup"

	"github.com/alkk/tg-mcp/pkg/config"
	"github.com/alkk/tg-mcp/pkg/ingest"
	"github.com/alkk/tg-mcp/pkg/server"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

var revision = "unknown"

type options struct {
	Telegram struct {
		Token  string `long:"token" env:"TOKEN" description:"telegram bot token"`
		APIURL string `long:"api-url" env:"API_URL" default:"https://api.telegram.org" description:"telegram bot api base url"`
		Local  bool   `long:"local" env:"LOCAL" description:"bot api server runs with --local, getFile returns filesystem paths"`
	} `group:"telegram" namespace:"telegram" env-namespace:"TELEGRAM"`

	AuthToken   string        `long:"auth-token" env:"AUTH_TOKEN" description:"bearer token for the mcp and files endpoints, and the secret download links are signed with"`
	Listen      string        `long:"listen" env:"LISTEN" default:":8080" description:"http listen address"`
	Data        string        `long:"data" env:"DATA_DIR" default:"./data" description:"data directory for sqlite db and file cache"`
	Chats       string        `long:"chats" env:"CHATS_FILE" default:"chats.yml" description:"chat map file"`
	FileLinkTTL time.Duration `long:"file-link-ttl" env:"FILE_LINK_TTL" default:"5m" description:"lifetime of get_file download links"`
	Dbg         bool          `long:"dbg" env:"DEBUG" description:"debug logging"`
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1) // go-flags already reported the problem
	}

	setupLog(opts.Dbg)
	slog.Info("tg-mcp", "revision", resolveVersion())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, opts); err != nil {
		slog.Error("terminated", "err", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (*options, error) {
	var opts options
	p := flags.NewParser(&opts, flags.Default)
	if _, err := p.ParseArgs(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}
	return &opts, nil
}

// run wires the three components together and runs them until the context is canceled or one
// of them fails.
func run(ctx context.Context, opts *options) error {
	if err := validate(opts); err != nil {
		return err
	}

	chats, err := config.Load(opts.Chats)
	if err != nil {
		return fmt.Errorf("load chat map: %w", err)
	}

	st, err := store.New(opts.Data)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			slog.Warn("close store", "err", cerr)
		}
	}()

	if err = st.SyncChats(ctx, chats.All()); err != nil {
		return fmt.Errorf("sync chats: %w", err)
	}

	tg := telegram.New(opts.Telegram.Token, opts.Telegram.APIURL, opts.Telegram.Local)
	me, err := tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("identify bot: %w", err)
	}
	slog.Info("bot identified", "id", me.ID, "username", me.Username, "chats", len(chats.All()))

	srv, err := server.New(server.Params{
		Store:       st,
		Telegram:    tg,
		Chats:       chats,
		AuthToken:   opts.AuthToken,
		Listen:      opts.Listen,
		Version:     resolveVersion(),
		FileLinkTTL: opts.FileLinkTTL,
	})
	if err != nil {
		return fmt.Errorf("create mcp server: %w", err)
	}

	ing := ingest.New(ingest.Params{
		API:         tg,
		Store:       st,
		Chats:       chats,
		BotID:       me.ID,
		BotUsername: me.Username,
	})

	grp, grpCtx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		if rerr := ing.Run(grpCtx); rerr != nil {
			return fmt.Errorf("ingest: %w", rerr)
		}
		return nil
	})
	grp.Go(func() error {
		if rerr := srv.Run(grpCtx); rerr != nil {
			return fmt.Errorf("mcp server: %w", rerr)
		}
		return nil
	})
	return grp.Wait() //nolint:wrapcheck // both goroutines wrap their own errors
}

// validate catches the startup mistakes that would otherwise surface as confusing runtime errors.
func validate(opts *options) error {
	switch {
	case opts.Telegram.Token == "":
		return errors.New("telegram bot token is required (--telegram.token, TELEGRAM_TOKEN)")
	case opts.AuthToken == "":
		return errors.New("mcp auth token is required (--auth-token, AUTH_TOKEN)")
	case opts.FileLinkTTL < 0:
		return errors.New("file link ttl cannot be negative (--file-link-ttl, FILE_LINK_TTL)")
	}
	return nil
}

func setupLog(dbg bool) {
	level := slog.LevelInfo
	if dbg {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func resolveVersion() string {
	if revision != "unknown" {
		return revision
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return revision
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return revision
}
