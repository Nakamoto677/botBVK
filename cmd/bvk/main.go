package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
	if *check && *skipTelegramCheck {
		return fmt.Errorf("-check cannot be combined with -skip-telegram-check: Telegram verification would be skipped")
	}
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
	clientGroups := make(map[int64]telegram.ClientGroup, len(cfg.ClientGroups))
	for _, group := range cfg.ClientGroups {
		companyID, err := store.CompanyIDByIdentifier(ctx, group.Company)
		if err != nil {
			return fmt.Errorf("client group %d: company %q: %w", group.ChatID, group.Company, err)
		}
		clientGroups[group.ChatID] = telegram.ClientGroup{CompanyID: companyID, Link: group.Link}
	}
	if *migrate {
		return nil
	}
	if err = cfg.ValidateTelegram(); err != nil {
		return err
	}
	tc := telegram.Config{Departments: map[domain.Department]telegram.Team{}, ClientGroups: clientGroups, GroupReceipt: cfg.GroupReceipt, AdminIDs: []int64{cfg.AdminID}, ArchiveDir: cfg.ArchiveDir, StartHour: cfg.StartHour, EndHour: cfg.EndHour}
	for dept, t := range cfg.Departments {
		tc.Departments[dept] = telegram.Team{ChatID: t.ChatID, QueueTopicID: t.QueueTopicID, Tags: t.Tags}
	}
	client := telegram.NewClient(cfg.Token)
	// A company chat that Telegram refuses is a configuration defect, not a
	// reason to stop private support. Departments stay fatal: they carry every
	// ticket. Broken chats are removed from the map so a misconfigured id cannot
	// admit messages it was never verified for, and the counter below makes the
	// loss visible on /ready and /metrics instead of only in the journal.
	var disabledGroups int
	if *skipTelegramCheck {
		// The remote check is getMe plus getChat and getChatMember for every department,
		// so a network that blocks Telegram aborts the whole start even though the bot is
		// configured correctly. Skipping it keeps a valid bot runnable on such a network;
		// the local shape of the configuration was validated above.
		slog.Warn("starting without the remote Telegram permission check", "reason", "skip-telegram-check")
		if len(clientGroups) > 0 {
			slog.Warn("client group permissions and @mention hints are not verified in this mode", "client_groups", len(clientGroups))
		}
	} else {
		problems, err := client.CheckRuntime(ctx, tc)
		if err != nil {
			var api *telegram.APIError
			if errors.As(err, &api) && api.Ambiguous {
				// The retries inside CheckRuntime already absorbed a short blip.
				// Naming the flag here saves the operator from guessing whether
				// the token or the network is at fault.
				return fmt.Errorf("Telegram setup: %w (Telegram is unreachable from this machine after retries: check the network, or start with -skip-telegram-check)", err)
			}
			return fmt.Errorf("Telegram setup: %w", err)
		}
		disabledGroups = disableBrokenClientGroups(clientGroups, problems)
		if disabledGroups > 0 {
			slog.Error("company group channel partially disabled: private clients and departments keep working", "disabled_client_groups", disabledGroups, "configured_client_groups", disabledGroups+len(clientGroups))
		}
	}
	if client.Username != "" {
		slog.Info("telegram bot verified", "username", client.Username)
	}
	tc.BotUsername = client.Username
	if *check {
		slog.Info("database and Telegram setup verified", "client_groups", len(clientGroups))
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
		return fmt.Errorf("another bot instance is active; pilot permits one instance%s", singletonHolder(ctx, lock, cfg.HealthAddress))
	}
	configureBotCommands(ctx, client, cfg)
	calendar := tickets.DefaultCalendar()
	calendar.StartHour = cfg.StartHour
	calendar.EndHour = cfg.EndHour
	calendar.Holidays = map[string]bool{}
	for _, day := range cfg.Holidays {
		calendar.Holidays[day] = true
	}
	service := tickets.New(store)
	scheduler := &monitor.Scheduler{Store: store, Config: cfg, Calendar: calendar}
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
	server := &http.Server{Addr: cfg.HealthAddress, Handler: healthHandler(store, adapter.LastPoll, client.Metrics, disabledGroups), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		e := server.ListenAndServe()
		if !errors.Is(e, http.ErrServerClosed) {
			errs <- e
		}
	}()
	launch(adapter.Run)
	launch(worker.Run)
	launch(scheduler.Run)
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

// Commands are shortcuts to the visible UI. Private staff and department groups
// get their own short lists; company chats receive no bot command menu.
func configureBotCommands(ctx context.Context, client *telegram.Client, cfg config.Config) {
	clientCommands := []map[string]string{
		{"command": "start", "description": "Открыть поддержку"},
		{"command": "menu", "description": "Открыть меню"},
		{"command": "new", "description": "Создать заявку"},
		{"command": "tickets", "description": "Посмотреть мои заявки"},
		{"command": "current", "description": "Открыть активную заявку"},
		{"command": "form", "description": "Продолжить черновик"},
		{"command": "help", "description": "Как получить помощь"},
	}
	staffCommands := []map[string]string{
		{"command": "menu", "description": "Открыть меню"},
		{"command": "tickets", "description": "Посмотреть заявки"},
		{"command": "history", "description": "Открыть историю текущей заявки"},
		{"command": "help", "description": "Как работать с заявками"},
	}
	adminCommands := append(append([]map[string]string{}, staffCommands...),
		map[string]string{"command": "requests", "description": "Проверить запросы доступа"},
		map[string]string{"command": "report", "description": "Получить отчёт"})
	set := func(scope map[string]any, commands []map[string]string) {
		if err := client.Call(ctx, "setMyCommands", map[string]any{"commands": commands, "scope": scope}, nil); err != nil {
			slog.Warn("could not configure Telegram commands", "scope", scope, "error", err)
		}
	}
	set(map[string]any{"type": "all_private_chats"}, clientCommands)
	seen := map[int64]bool{}
	for _, staff := range cfg.Staff {
		if staff.ID <= 0 || seen[staff.ID] {
			continue
		}
		seen[staff.ID] = true
		commands := staffCommands
		if staff.Role == "admin" {
			commands = adminCommands
		}
		set(map[string]any{"type": "chat", "chat_id": staff.ID}, commands)
	}
	if cfg.AdminID > 0 && !seen[cfg.AdminID] {
		set(map[string]any{"type": "chat", "chat_id": cfg.AdminID}, adminCommands)
	}
	for _, department := range cfg.Departments {
		if department.ChatID < 0 && !seen[department.ChatID] {
			seen[department.ChatID] = true
			set(map[string]any{"type": "chat", "chat_id": department.ChatID}, staffCommands)
		}
	}
}

// singletonHolder names where the running instance is, because "another bot
// instance is active" alone leaves the operator with nothing to act on: the
// holder may be a forgotten nohup process from hours ago. The advisory lock
// names the PostgreSQL backend that owns it, and the health port answers only
// when a live process on this host is serving it.
func singletonHolder(ctx context.Context, lock *pgx.Conn, healthAddress string) string {
	var (
		pid     int64
		started time.Time
	)
	err := lock.QueryRow(ctx, `select l.pid, a.backend_start
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'advisory' and l.objid = 731902 and l.granted
		limit 1`).Scan(&pid, &started)
	if err != nil {
		return ""
	}
	hint := fmt.Sprintf(" (lock 731902 is held by PostgreSQL backend %d since %s", pid, started.Local().Format("2006-01-02 15:04:05"))
	if conn, dialErr := net.DialTimeout("tcp", healthAddress, 300*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		hint += fmt.Sprintf("; the health server on %s answers, so the process is on this host: find it with lsof -nP -iTCP:%s -sTCP:LISTEN", healthAddress, healthAddress)
	}
	return hint + ")"
}

// disableBrokenClientGroups drops the company chats whose verification failed
// for a reason an operator has to fix. A transient answer keeps the chat: the
// next getUpdates round exercises it again without a restart, and disabling a
// healthy chat would silently lose its tickets until someone notices.
func disableBrokenClientGroups(groups map[int64]telegram.ClientGroup, problems []telegram.ClientGroupProblem) int {
	disabled := 0
	for _, problem := range problems {
		if problem.Transient {
			slog.Warn("client group check failed; keeping it enabled because Telegram may answer differently", "chat_id", problem.ChatID, "error", problem.Err)
			continue
		}
		delete(groups, problem.ChatID)
		disabled++
		slog.Error("client group disabled until the configuration is fixed", "chat_id", problem.ChatID, "error", problem.Err, "hint", problem.Hint)
	}
	return disabled
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
func healthHandler(store *postgres.Store, lastPoll func() time.Time, timing *metrics.Metrics, disabledGroups int) http.Handler {
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
		if disabledGroups > 0 {
			// The bot serves every other channel, so readiness stays 200: a 503
			// would take the whole installation out of monitoring for a problem
			// that only affects one company chat. The line is still impossible to
			// miss for an operator reading /ready.
			fmt.Fprintf(w, "ready\nwarning: %d client group(s) disabled after the start check; fix config.json and restart\n", disabledGroups)
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
		fmt.Fprintf(w, "bvk_outbox_pending %d\nbvk_outbox_failed %d\nbvk_outbox_oldest_seconds %.0f\nbvk_updates_quarantined %d\nbvk_client_groups_disabled %d\n", pending, failed, age, quarantined, disabledGroups)
		// Stage timings collected in-process: rate-limiter waiting, per-method
		// Telegram latency, whole operation duration and message relay time.
		if timing != nil {
			fmt.Fprint(w, timing.Render())
		}
	})
	return mux
}
