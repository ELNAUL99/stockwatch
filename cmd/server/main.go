package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ELNAUL99/stockwatch/internal/httpapi"
	"github.com/ELNAUL99/stockwatch/internal/inventory"
	"github.com/ELNAUL99/stockwatch/internal/storage"
)

// Build metadata, injected at link time by the Dockerfile:
//
//	go build -ldflags="-X main.version=1.2.3 -X main.commit=$(git rev-parse HEAD)"
//
// They are plain vars rather than consts because -X can only rewrite a variable
// — a const is folded into the code at compile time and has no symbol to patch.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	// A self-contained health check, so the container can probe itself.
	//
	// The runtime image is distroless: no shell, no curl, no wget. The usual
	// compose healthcheck therefore has nothing to run. Giving the binary a mode
	// that curls its own /healthz solves that without reintroducing a shell into
	// the image, which is the property that makes distroless worth using.
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz endpoint and exit 0 or 1")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("stockwatch %s (%s)\n", version, commit)
		return
	}

	if *healthcheck {
		if err := selfCheck(); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// main does two things only: call run, and translate its error into an exit
	// code. Everything else lives in run so that it can return errors normally
	// and so that deferred cleanup actually executes — os.Exit skips defers, so
	// any code that calls it directly cannot also clean up after itself.
	if err := run(); err != nil {
		// Logging with the default logger rather than the configured one,
		// because a config failure is exactly the case where the configured
		// logger does not exist yet.
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// selfCheck probes the local /healthz. Used by the container healthcheck.
func selfCheck() error {
	addr := envString("ADDR", ":8080")
	// ADDR is typically ":8080" with no host, which is not a valid URL on its
	// own, so the loopback host is filled in.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	// Draining before closing lets the connection be reused rather than
	// abandoned mid-stream. Irrelevant for a one-shot probe, but it is the habit
	// that avoids leaking connections in code that loops.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	// signal.NotifyContext gives a context cancelled on SIGTERM or SIGINT.
	// SIGTERM is what Kubernetes and Docker send; SIGINT is Ctrl-C. This is the
	// modern replacement for wiring up a chan os.Signal by hand.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := newPool(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrate on boot. Safe across replicas because Migrate takes a Postgres
	// advisory lock; see internal/storage/migrate.go.
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 60*time.Second)
	defer cancelMigrate()
	if err := storage.Migrate(migrateCtx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	logger.Info("migrations applied")

	store := storage.New(pool)
	svc := inventory.NewService(store, store, inventory.Config{
		HistoryWindowDays: cfg.HistoryWindowDays,
		Alpha:             cfg.Alpha,
	})

	handler := httpapi.NewRouter(svc, store, httpapi.Options{
		Logger:             logger,
		RequestTimeout:     cfg.RequestTimeout,
		MaxBodyBytes:       cfg.MaxBodyBytes,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,

		// These four are the difference between a toy server and one that
		// survives the open internet. Left unset they are all zero, meaning "no
		// limit", and a handful of connections that open and never send a byte
		// will hold file descriptors indefinitely — the Slowloris attack.
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		// ReadHeaderTimeout specifically bounds the header phase, which is what
		// Slowloris exploits. Kept tight regardless of ReadTimeout.
		ReadHeaderTimeout: 5 * time.Second,

		// Route the server's own errors into slog rather than the standard
		// logger, so every line in the process shares one format.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// ListenAndServe blocks, so it runs in its own goroutine and reports back
	// over a buffered channel. Buffered with capacity 1 so that if shutdown wins
	// the race, this send does not block forever on a channel nobody reads —
	// which would leak the goroutine.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server listening",
			slog.String("addr", cfg.Addr),
			slog.Int("db_max_conns", int(cfg.MaxConns)),
			slog.String("request_timeout", cfg.RequestTimeout.String()))

		// A clean Shutdown makes ListenAndServe return ErrServerClosed. That is
		// the expected outcome, not a failure, so it is filtered out here.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Wait for either a signal or the server falling over on its own.
	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
		return nil

	case <-ctx.Done():
		logger.Info("shutdown signal received",
			slog.String("drain_timeout", cfg.ShutdownTimeout.String()))
	}

	// Stop intercepting signals now that shutdown has begun. A second Ctrl-C
	// then kills the process immediately, which is what an operator expects when
	// a drain is taking too long.
	stop()

	// The drain context deliberately does NOT derive from ctx. ctx is already
	// cancelled — that is what woke us — so deriving from it would give
	// Shutdown a dead context and turn a graceful drain into an instant close.
	// This is the single easiest mistake to make in a Go shutdown path.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelDrain()

	// Shutdown stops accepting new connections and waits for in-flight requests
	// to finish. It does not interrupt them — that is what the per-request
	// Timeout middleware is for, and why the drain timeout should exceed the
	// request timeout.
	if err := srv.Shutdown(drainCtx); err != nil {
		// The drain window expired with requests still running. Close severs
		// them rather than leaving the process hanging past its grace period,
		// after which the orchestrator would SIGKILL us anyway.
		logger.Error("graceful shutdown timed out; forcing close",
			slog.String("error", err.Error()))
		if closeErr := srv.Close(); closeErr != nil {
			return fmt.Errorf("force close: %w", closeErr)
		}
		return fmt.Errorf("shutdown exceeded %s drain timeout: %w", cfg.ShutdownTimeout, err)
	}

	logger.Info("shutdown complete")
	return nil
}

// newPool builds the database pool with explicit sizing.
func newPool(ctx context.Context, cfg *Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		// Deliberately not wrapping with %w and not including the URL: a parse
		// failure message from pgx can echo the DSN, and the DSN holds the
		// password. This is one of the few places where dropping the underlying
		// error is the right call.
		return nil, errors.New("DATABASE_URL is not a valid Postgres connection string")
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	// NewWithConfig is lazy — it does not connect. Ping here so a bad host or
	// bad credentials fails at boot with a clear message, rather than on the
	// first request minutes later.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	logger.Info("database connected",
		slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Int("min_conns", int(cfg.MinConns)))

	return pool, nil
}

// newLogger builds the structured logger.
//
// JSON by default because logs are consumed by a collector in every environment
// that matters; text is available for local work where a human reads them.
func newLogger(cfg *Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Logs go to stdout, not stderr. The twelve-factor convention is that a
	// process writes its event stream to stdout and lets the platform route it;
	// splitting across both just means two places to look.
	return slog.New(handler)
}
