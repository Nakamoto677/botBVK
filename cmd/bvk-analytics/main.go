package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"bot-bvk/internal/analytics"
	"bot-bvk/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("analytics stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "config.json", "bot calendar and monitoring configuration")
	migrate := flag.Bool("migrate", false, "create analytics schema with administrator connection and exit")
	once := flag.Bool("once", false, "publish one snapshot and exit")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := analytics.Store{Pool: pool}
	if *migrate {
		return store.Migrate(ctx)
	}
	opts := analytics.OptionsFromConfig(cfg)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		refreshCtx, stop := context.WithTimeout(ctx, 55*time.Second)
		snapshot, e := store.Refresh(refreshCtx, opts)
		stop()
		if e != nil {
			if *once {
				return e
			}
			slog.Error("analytics refresh failed", "error", e)
		} else {
			slog.Info("analytics published", "snapshot_at", snapshot.At, "tickets", len(snapshot.Tickets), "incomplete", snapshot.IncompleteCount)
		}
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
