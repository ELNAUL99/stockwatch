// Command migrate applies or rolls back database migrations.
//
// The server also migrates on boot, so this binary is not required for a normal
// deploy. It exists for the cases the boot path deliberately does not cover:
// rolling back, and running migrations from CI or a one-off job without starting
// a server.
//
// Rollback is a separate manual act on purpose. Automatically reverting on boot
// — say, because a rolled-back deploy shipped an older migration set — is a
// reliable way to drop a production table.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ELNAUL99/stockwatch/internal/storage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	var (
		down    = flag.Bool("down", false, "roll back the most recent migration")
		all     = flag.Bool("all", false, "with -down, roll back every migration (destructive)")
		status  = flag.Bool("status", false, "print applied migrations and exit")
		timeout = flag.Duration("timeout", 2*time.Minute, "overall timeout")
	)
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	// Honour Ctrl-C so a long migration can be abandoned deliberately rather
	// than by killing the process mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	switch {
	case *status:
		return printStatus(ctx, pool)

	case *down:
		reverted, err := storage.Rollback(ctx, pool, *all)
		// Report progress before the error. Both calls return what they managed
		// to do even when they fail partway, and for a rollback that list names
		// the tables that have already been dropped — the operator needs to see
		// it before deciding what to do next.
		report("rolled back", reverted, err)
		if err != nil {
			return err
		}
		if len(reverted) == 0 {
			fmt.Println("nothing to roll back")
		}
		return nil

	default:
		applied, err := storage.MigrateVerbose(ctx, pool)
		report("applied", applied, err)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("already up to date")
		}
		return nil
	}
}

// report prints each completed migration, noting when the run stopped early.
func report(verb string, names []string, err error) {
	for _, name := range names {
		fmt.Printf("%s %s\n", verb, name)
	}
	if err != nil && len(names) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nstopped after %d migration(s); the schema is partially changed\n", len(names))
	}
}

func printStatus(ctx context.Context, pool *pgxpool.Pool) error {
	versions, err := storage.AppliedMigrations(ctx, pool)
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		fmt.Println("no migrations applied")
		return nil
	}
	for _, m := range versions {
		fmt.Printf("%04d  %-24s %s\n", m.Version, m.Name, m.AppliedAt.Format(time.RFC3339))
	}
	return nil
}
