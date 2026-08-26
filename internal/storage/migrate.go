package storage

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ELNAUL99/stockwatch/migrations"
)

// Why a hand-rolled migrator instead of golang-migrate or goose:
//
// The brief allows one external dependency, and pgx spends it. What a migration
// tool actually has to do here is small — order the files, remember which ran,
// run the rest in a transaction, and refuse to run twice concurrently. That is
// the ~90 lines below.
//
// This is a genuine trade-off, not a free win. golang-migrate brings things this
// does not have: down-migration sequencing, dirty-state recovery, drivers for
// other databases, and a CLI that works without the app binary. If this service
// grew a second datastore or a team large enough to need out-of-band migration
// tooling, I would take the dependency. For one Postgres and one binary, this is
// less machinery to explain and nothing to keep upgraded.

// migrationsTable records what has already been applied.
const migrationsTable = "schema_migrations"

// advisoryLockKey namespaces the session lock that serialises migrations.
//
// Without it, two replicas starting at once both see an empty schema_migrations
// and both try to CREATE TABLE products — one wins, the other crashes on boot.
// A Postgres advisory lock is the standard fix: the second replica blocks until
// the first finishes, then finds the work already done and proceeds. The number
// is arbitrary but must be stable across deploys.
const advisoryLockKey int64 = 8080550119

// migration is one numbered pair of SQL files.
type migration struct {
	version int
	name    string
	upSQL   string
	// downSQL is empty when no matching .down.sql exists, which makes the
	// migration irreversible rather than silently rolling back to nothing.
	downSQL string
}

// AppliedMigration is one row of schema_migrations, for the status command.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt time.Time
}

// Migrate applies every pending migration in version order.
//
// It is safe to call on every boot and safe to call from several replicas at
// once. Callers pass a context so a slow migration cannot hang a deploy
// indefinitely.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := MigrateVerbose(ctx, pool)
	return err
}

// MigrateVerbose is Migrate, reporting which migrations it applied.
//
// Two entry points rather than one because the server wants only an error while
// the CLI wants to print what happened, and threading a logger or an io.Writer
// into the storage layer to satisfy the CLI would be worse than returning data
// and letting the caller decide how to render it.
func MigrateVerbose(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	all, err := loadMigrations()
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}

	var applied []string
	err = withMigrationLock(ctx, pool, func(conn *pgx.Conn) error {
		done, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}

		for _, m := range all {
			if done[m.version] {
				continue
			}
			if err := applyOne(ctx, conn, m); err != nil {
				return err
			}
			applied = append(applied, fmt.Sprintf("%04d_%s", m.version, m.name))
		}
		return nil
	})
	// applied is returned even alongside an error. Each migration commits in its
	// own transaction, so a failure at number three leaves one and two genuinely
	// applied — discarding that list would tell the operator "it failed" while
	// hiding which half of the schema actually changed.
	return applied, err
}

// Rollback reverts the most recently applied migration, or all of them.
//
// Destructive by nature: the down files drop tables. This is never called on
// boot — reverting automatically because a rolled-back deploy shipped an older
// migration set is a reliable way to lose production data.
func Rollback(ctx context.Context, pool *pgxpool.Pool, all bool) ([]string, error) {
	byVersion, err := loadMigrationsByVersion()
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}

	var reverted []string
	err = withMigrationLock(ctx, pool, func(conn *pgx.Conn) error {
		for {
			versions, err := appliedMigrationsOn(ctx, conn)
			if err != nil {
				return err
			}
			if len(versions) == 0 {
				return nil
			}

			// Reverse order: the newest migration comes off first, since a later
			// one may depend on tables an earlier one created.
			latest := versions[len(versions)-1]

			m, ok := byVersion[latest.Version]
			if !ok {
				return fmt.Errorf("migration %d (%s) is recorded as applied but "+
					"its files are missing; cannot roll back", latest.Version, latest.Name)
			}
			if m.downSQL == "" {
				return fmt.Errorf("migration %04d_%s has no .down.sql and cannot be "+
					"rolled back", m.version, m.name)
			}

			if err := revertOne(ctx, conn, m); err != nil {
				return err
			}
			reverted = append(reverted, fmt.Sprintf("%04d_%s", m.version, m.name))

			if !all {
				return nil
			}
		}
	})
	// Same reasoning as MigrateVerbose, and it matters more here: a partial
	// rollback has already dropped tables, and the operator needs to know
	// exactly which ones before deciding what to do next.
	return reverted, err
}

// AppliedMigrations lists what has been applied, oldest first.
func AppliedMigrations(ctx context.Context, pool *pgxpool.Pool) ([]AppliedMigration, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if err := ensureMigrationsTable(ctx, conn.Conn()); err != nil {
		return nil, err
	}
	return appliedMigrationsOn(ctx, conn.Conn())
}

// withMigrationLock runs fn holding the advisory lock on a dedicated connection.
//
// Extracted because every entry point needs the identical dance and getting one
// of them subtly wrong — releasing on a different connection than acquired,
// releasing with a cancelled context — produces a lock that is never freed and a
// service that will not boot again.
func withMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func(*pgx.Conn) error) error {
	// A dedicated connection for the whole operation. The advisory lock is
	// session-scoped, so it must be acquired and released on the *same*
	// connection — asking the pool each time could hand us a different one.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	// Release on every exit path. The deferred call gets its own background
	// context deliberately: if ctx was cancelled — a deploy timeout, a SIGTERM —
	// using it here would fail to release the lock and every future boot would
	// block behind it. Cleanup must not inherit the cancellation that triggered it.
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	if err := ensureMigrationsTable(ctx, conn.Conn()); err != nil {
		return err
	}
	return fn(conn.Conn())
}

func ensureMigrationsTable(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
			version     INTEGER     PRIMARY KEY,
			name        TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create %s: %w", migrationsTable, err)
	}
	return nil
}

// applyOne runs a single migration and records it, atomically.
//
// The DDL and the schema_migrations insert share one transaction, so a migration
// can never be half-applied-but-recorded or applied-but-unrecorded. Postgres
// supports transactional DDL, which is what makes this possible and is a real
// advantage over MySQL.
func applyOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx for migration %d: %w", m.version, err)
	}
	// Rollback after a successful Commit is a no-op that returns
	// pgx.ErrTxClosed, so this unconditional defer is safe and is the idiomatic
	// way to guarantee cleanup on every early return. The error is explicitly
	// discarded to make that intent visible rather than leaving a naked call.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.upSQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+migrationsTable+` (version, name) VALUES ($1, $2)`,
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	return nil
}

// revertOne runs a down migration and deletes its record, atomically.
func revertOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx for rollback %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.downSQL); err != nil {
		return fmt.Errorf("roll back migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM `+migrationsTable+` WHERE version = $1`, m.version,
	); err != nil {
		return fmt.Errorf("unrecord migration %d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollback %d: %w", m.version, err)
	}
	return nil
}

// appliedMigrationsOn reads schema_migrations in version order.
func appliedMigrationsOn(ctx context.Context, conn *pgx.Conn) ([]AppliedMigration, error) {
	rows, err := conn.Query(ctx,
		`SELECT version, name, applied_at FROM `+migrationsTable+` ORDER BY version ASC`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		if err := rows.Scan(&m.Version, &m.Name, &m.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return out, nil
}

// loadMigrationsByVersion indexes the embedded migrations for lookup.
func loadMigrationsByVersion() (map[int]migration, error) {
	all, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int]migration, len(all))
	for _, m := range all {
		byVersion[m.version] = m
	}
	return byVersion, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}
		applied[v] = true
	}
	// rows.Err reports failures that happened mid-iteration — a dropped
	// connection looks exactly like a clean end-of-results without this check.
	// Forgetting it is the classic database/sql and pgx bug.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

// loadMigrations reads and sorts the embedded .up.sql files.
//
// Filenames must look like 0001_init.up.sql. Down files are read by the Makefile
// target, not by the service — rolling back automatically on boot is a way to
// lose production data, so it is a deliberate manual act.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}

	var out []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		version, label, err := parseMigrationName(name)
		if err != nil {
			return nil, err
		}

		body, err := fs.ReadFile(migrations.FS, path.Join(".", name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		// The matching down file is optional. A missing one leaves downSQL empty,
		// which Rollback reports as "cannot be rolled back" — better than a
		// rollback that appears to succeed while changing nothing.
		var downSQL string
		downName := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		if downBody, err := fs.ReadFile(migrations.FS, path.Join(".", downName)); err == nil {
			downSQL = string(downBody)
		}

		out = append(out, migration{
			version: version,
			name:    label,
			upSQL:   string(body),
			downSQL: downSQL,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	// Duplicate version numbers mean two people numbered a migration the same on
	// separate branches. Whichever sorted first would silently mark the other as
	// applied and skip it. Fail loudly at boot instead.
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)",
				out[i].version, out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// parseMigrationName splits "0001_init.up.sql" into 1 and "init".
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".up.sql")

	numPart, label, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("migration %q must be named <version>_<name>.up.sql", filename)
	}

	version, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q has a non-numeric version %q", filename, numPart)
	}
	return version, label, nil
}
