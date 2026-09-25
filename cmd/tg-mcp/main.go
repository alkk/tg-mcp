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
	PendingTTL  time.Duration `long:"pending-ttl" env:"PENDING_TTL" default:"336h" description:"how long messages from chats outside the allowlist are kept for replay"`
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

	if err = drainPending(ctx, st, chats.All(), opts.PendingTTL); err != nil {
		return fmt.Errorf("drain buffered messages: %w", err)
	}

	trackPublic(ctx, st, chats.All())

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
		sweepPending(grpCtx, st, opts.PendingTTL, pendingSweepEvery)
		return nil
	})
	grp.Go(func() error {
		if rerr := srv.Run(grpCtx); rerr != nil {
			return fmt.Errorf("mcp server: %w", rerr)
		}
		return nil
	})
	return grp.Wait() //nolint:wrapcheck // the goroutines wrap their own errors
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
	case opts.PendingTTL < 0:
		return errors.New("pending ttl cannot be negative (--pending-ttl, PENDING_TTL)")
	}
	return nil
}

// trackPublic loads the newest message of every public chat. A chat whose newest row will not load
// is logged and left empty rather than failing startup: the row would still be there on the next
// boot, so an error here would brick every start over a decorative ticker.
func trackPublic(ctx context.Context, st *store.Store, chats []config.Chat) {
	for _, c := range chats {
		if c.Public == "" {
			continue
		}
		if err := st.TrackLatest(ctx, c.ID); err != nil {
			slog.Warn("latest public message not loaded, it fills on the next message", "err", err,
				"public", c.Public, "customer", c.Customer, "label", c.Label, "chat_id", c.ID)
		}
	}
}

// drainPending replays the messages buffered for chats the chat map now knows, drops what has
// waited longer than ttl and reports the chats still missing from the map. Replay runs first, so
// adding a chat wins over the TTL: its backlog is recovered instead of swept by the very startup
// that was meant to replay it.
func drainPending(ctx context.Context, st *store.Store, chats []config.Chat, ttl time.Duration) error {
	replayed, err := st.ReplayPending(ctx, chats)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	for _, r := range replayed {
		slog.Info("replayed buffered messages", "customer", r.Customer, "chat_id", r.ChatID, "messages", r.Count)
		if len(r.Dropped) > 0 {
			slog.Warn("buffered messages dropped, the messages table refused them", "customer", r.Customer,
				"chat_id", r.ChatID, "message_ids", r.Dropped)
		}
	}

	swept, err := st.SweepPending(ctx, ttl)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	if swept > 0 {
		slog.Info("expired buffered messages removed", "messages", swept, "ttl", ttl)
	}

	waiting, err := st.PendingChats(ctx)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}
	for _, c := range waiting {
		slog.Warn("buffered chat not in the allowlist", "chat_id", c.ChatID, "title", c.Title,
			"type", c.Type, "messages", c.Messages, "oldest", c.Oldest.Format(time.RFC3339))
	}
	return nil
}

// pendingSweepEvery is how often the TTL sweep runs while the process is up. The drain is
// restart-only because it needs a re-read chats.yml; the sweep needs nothing but the clock, and a
// startup-only one would leave --pending-ttl unenforced for the whole life of a deployment that
// never restarts — a chat that keeps talking after being taken out of the chat map, or a stranger
// who added the bot to a group, would buffer without bound.
const pendingSweepEvery = time.Hour

// sweepPending drops expired buffered messages until the context is canceled. A failing sweep is
// logged and retried on the next tick: it is garbage collection, not something worth ending the
// process over.
func sweepPending(ctx context.Context, st *store.Store, ttl, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			swept, err := st.SweepPending(ctx, ttl)
			switch {
			case err != nil && ctx.Err() == nil:
				slog.Warn("sweep buffered messages", "err", err)
			case err == nil && swept > 0:
				slog.Info("expired buffered messages removed", "messages", swept, "ttl", ttl)
			}
		}
	}
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
