package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
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

	warnIfExposed(logger, *listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx, *listen)
}

// warnIfExposed shouts when the control plane is bound to anything other than
// loopback.
//
// There is no authentication on this API, and POST /tasks runs an arbitrary
// prompt through an agent that edits files and can push with your GitHub
// credentials. On loopback that is fine. On a routable interface it is remote
// code execution for anyone who can reach the port. Reach a remote control
// plane through an SSH tunnel or a private network instead — see the README.
func warnIfExposed(logger *log.Logger, listen string) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if isLoopback(host) {
		return
	}

	logger.Printf("**********************************************************************")
	logger.Printf("WARNING: listening on %q, which is not loopback.", listen)
	logger.Printf("")
	logger.Printf("This API has NO AUTHENTICATION. Anyone who can reach this port can")
	logger.Printf("queue a task, which runs an agent on this machine with your git and")
	logger.Printf("GitHub credentials. That is remote code execution.")
	logger.Printf("")
	logger.Printf("Bind to 127.0.0.1 and reach it over an SSH tunnel or a private")
	logger.Printf("network instead. See \"Remote access\" in the README.")
	logger.Printf("**********************************************************************")
}

func isLoopback(host string) bool {
	switch host {
	case "", "localhost":
		// An empty host means "all interfaces", which is the worst case.
		return host == "localhost"
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
