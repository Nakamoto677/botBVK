package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"bot-bvk/internal/archive"
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/metrics"
	"bot-bvk/internal/monitor"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/report"
	"bot-bvk/internal/telegram"
	"bot-bvk/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "config.json", "configuration JSON path")
	migrate := flag.Bool("migrate", false, "apply migrations and bootstrap only")
	check := flag.Bool("check", false, "check database and Telegram rights, then exit")
	backup := flag.Bool("backup", false, "create backup and verify by restoring a temporary database")
	skipTelegramCheck := flag.Bool("skip-telegram-check", false, "start without the remote Telegram permission check (use when the network blocks Telegram)")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := postgres.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database connection: %w", err)
	}
	defer store.Close()
	if *backup {
		dir, err := archive.Create(ctx, cfg.DatabaseURL, cfg.ArchiveDir, cfg.BackupDir)
		if err != nil {
			return err
		}
		slog.Info("backup restored and verified", "directory", dir)
		return nil
	}
	if err = store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err = store.BootstrapCompanies(ctx, cfg.AdminID, cfg.Companies); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err = bootstrapStaff(ctx, store, cfg.Staff); err != nil {
		return err
	}
	if *migrate {
		return nil
	}
	if err = cfg.ValidateTelegram(); err != nil {
		return err
	}
	tc := telegram.Config{Departments: map[domain.Department]telegram.Team{}, AdminIDs: []int64{cfg.AdminID}, ArchiveDir: cfg.ArchiveDir}
	for dept, t := range cfg.Departments {
		tc.Departments[dept] = telegram.Team{ChatID: t.ChatID, QueueTopicID: t.QueueTopicID}
	}
	client := telegram.NewClient(cfg.Token)
	if *skipTelegramCheck {
		// The remote check is getMe plus getChat and getChatMember for every department,
		// so a network that blocks Telegram aborts the whole start even though the bot is
		// configured correctly. Skipping it keeps a valid bot runnable on such a network;
		// the local shape of the configuration was validated above.
		slog.Warn("starting without the remote Telegram permission check", "reason", "skip-telegram-check")
	} else if err = client.Check(ctx, tc); err != nil {
		return fmt.Errorf("Telegram setup: %w", err)
	}
	if *check {
		slog.Info("database and Telegram setup verified")
		return nil
	}
	if err = os.MkdirAll(cfg.ArchiveDir, 0700); err != nil {
		return err
	}
	// Pilot is deliberately single-active. A dedicated session owns the lock;
	// dropping it cancels all workers so a second process cannot operate silently.
	lock, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer lock.Close(context.Background())
	var acquired bool
	if err = lock.QueryRow(ctx, "SELECT pg_try_advisory_lock(731902)").Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("another bot instance is active; pilot permits one instance")
	}
	calendar := tickets.DefaultCalendar()
	calendar.StartHour = cfg.StartHour
	calendar.EndHour = cfg.EndHour
	calendar.Holidays = map[string]bool{}
	for _, day := range cfg.Holidays {
		calendar.Holidays[day] = true
	}
	service := tickets.New(store)
	scheduler := &monitor.Scheduler{Store: store, Config: cfg, Calendar: calendar}
	scheduler.Backup = func(ctx context.Context) (string, error) {
		return archive.Create(ctx, cfg.DatabaseURL, cfg.ArchiveDir, cfg.BackupDir)
	}
	tc.BeforeDelete = scheduler.BeforeDelete
	tc.BusinessHoursText = fmt.Sprintf("Обработка: Пн–Пт, %02d:00–%02d:00 по Иркутску. Заявки принимаются круглосуточно.", cfg.StartHour, cfg.EndHour)
	tc.ReportCSV = func(ctx context.Context) (string, error) { return report.CSV(ctx, store, calendar) }
	adapter := telegram.NewAdapter(client, store, service, tc)
	worker := telegram.NewWorker(client, store, tc)
	errs := make(chan error, 6)
	var runners sync.WaitGroup
	launch := func(fn func(context.Context) error) {
		runners.Add(1)
		go func() { defer runners.Done(); errs <- fn(ctx) }()
	}
	launch(func(ctx context.Context) error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				pingCtx, stop := context.WithTimeout(ctx, time.Second)
				e := lock.Ping(pingCtx)
				stop()
				if e != nil {
					return fmt.Errorf("singleton lock connection lost: %w", e)
				}
			}
		}
	})
	server := &http.Server{Addr: cfg.HealthAddress, Handler: healthHandler(store, adapter.LastPoll, client.Metrics), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		e := server.ListenAndServe()
		if !errors.Is(e, http.ErrServerClosed) {
			errs <- e
		}
	}()
	launch(adapter.Run)
	launch(worker.Run)
	launch(scheduler.Run)
	launch(scheduler.RunCleanup)
	slog.Info("support bot started", "business_timezone", "Asia/Irkutsk", "health_address", cfg.HealthAddress)
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
		cancel()
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	runners.Wait()
	return err
}
func bootstrapStaff(ctx context.Context, store domain.Store, staff []config.Staff) error {
	return store.Transact(ctx, func(tx domain.Tx) error {
		for _, s := range staff {
			_, err := tx.GetUser(ctx, s.ID)
			if err == nil {
				continue
			}
			if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			if err = tx.SaveUser(ctx, domain.User{ID: s.ID, Name: s.Name, Role: s.Role, Departments: s.Departments, Approved: true}); err != nil {
				return err
			}
		}
		return nil
	})
}
func healthHandler(store *postgres.Store, lastPoll func() time.Time, timing *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); fmt.Fprintln(w, "alive") })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		poll := lastPoll()
		if poll.IsZero() || time.Since(poll) > 2*time.Minute {
			http.Error(w, "Telegram polling unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, failed, oldest, err := store.QueueHealth(ctx)
		if err != nil || failed > 0 || (!oldest.IsZero() && time.Since(oldest) > 5*time.Minute) {
			http.Error(w, "delivery queue requires attention", http.StatusServiceUnavailable)
			return
		}
		// A quarantined update is accepted work that can never complete on its own
		// and is never selected again. Unreported, it silently freezes the account
		// it belongs to, so readiness must surface it with the way out.
		quarantined, err := store.QuarantineCount(ctx)
		if err != nil {
			http.Error(w, "inbox state unavailable", http.StatusServiceUnavailable)
			return
		}
		if quarantined > 0 {
			http.Error(w, fmt.Sprintf("inbox requires attention: %d quarantined update(s); inspect with /quarantine and release with /quarantine clear <id>", quarantined), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		pending, failed, oldest, err := store.QueueHealth(ctx)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		age := 0.0
		if !oldest.IsZero() {
			age = time.Since(oldest).Seconds()
		}
		quarantined, err := store.QuarantineCount(ctx)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "bvk_outbox_pending %d\nbvk_outbox_failed %d\nbvk_outbox_oldest_seconds %.0f\nbvk_updates_quarantined %d\n", pending, failed, age, quarantined)
		// Stage timings collected in-process: rate-limiter waiting, per-method
		// Telegram latency, whole operation duration and message relay time.
		if timing != nil {
			fmt.Fprint(w, timing.Render())
		}
	})
	return mux
}
