// Command aa-server runs the Agents Anywhere API.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/logx"
	"github.com/agents-anywhere/agents-anywhere/internal/server"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

func main() {
	migrateOnly := flag.Bool("migrate", false, "apply migrations and exit")
	flag.Parse()

	cfg := config.LoadServer()
	log := logx.Setup(cfg.LogLevel)

	// The signing key falls back to a constant when unset, which would let
	// anyone who has read the source forge user tokens. Refuse to serve with
	// it; local development opts in explicitly.
	if os.Getenv("AGENT_SERVER_SECRET") == "" {
		if os.Getenv("AA_ALLOW_DEV_SECRET") != "1" {
			log.Error("refusing to start with the development signing key",
				"hint", "set AGENT_SERVER_SECRET (production) or AA_ALLOW_DEV_SECRET=1 (local)")
			os.Exit(1)
		}
		log.Warn("using the development signing key",
			"hint", "set AGENT_SERVER_SECRET before exposing this instance")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		log.Error("open database", "error", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("database ready", "path", cfg.DBPath)
	if *migrateOnly {
		return
	}
	if err := db.EnsureFirstPartyOAuthClients(ctx); err != nil {
		log.Error("seed oauth clients", "error", err)
		os.Exit(1)
	}

	app := server.New(cfg, db, log)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the websocket upgrade and long-poll handlers own
		// their own deadlines and a server-wide cap would cut them off.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
}
