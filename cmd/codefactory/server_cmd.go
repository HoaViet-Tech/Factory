package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/ingest"
	"github.com/HoaViet-Tech/factory/internal/server"
	"github.com/HoaViet-Tech/factory/internal/store"
)

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dbPath := fs.String("db", "./factory.db", "path to the SQLite database")
	listen := fs.String("listen", "127.0.0.1:7337", "host:port to listen on")
	dryRun := fs.Bool("github-dry-run", false, "read GitHub but never write to it")
	pollInterval := fs.Duration("poll-interval", 0, "poll GitHub automatically on this interval (0 = only on demand via POST /github/poll)")
	pollLimit := fs.Int("poll-limit", 30, "maximum issues read per label per repository")
	leaseDuration := fs.Duration("lease", 2*time.Minute, "how long a claimed task stays leased to a worker")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Run the control plane server.\n\nUsage: codefactory server [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := log.New(os.Stdout, "[server] ", log.LstdFlags)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	logger.Printf("database: %s", *dbPath)

	// GitHub is optional. Without it the server still runs the full local
	// pipeline; only issue ingest is unavailable, and it says so clearly.
	var gh ingest.IssueLister
	ghClient := githubcli.New(*dryRun)
	ghClient.Logf = func(format string, args ...any) { logger.Printf(format, args...) }
	if err := ghClient.Available(); err != nil {
		logger.Printf("GitHub ingest disabled: %v", err)
	} else {
		gh = ghClient
		logger.Printf("GitHub ingest enabled (dry-run=%v)", *dryRun)
	}

	srv := server.New(server.Config{
		Store:        st,
		GitHub:       gh,
		DryRun:       *dryRun,
		DefaultLease: *leaseDuration,
		PollInterval: *pollInterval,
		PollLimit:    *pollLimit,
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx, *listen)
}
