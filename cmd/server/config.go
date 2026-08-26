package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the process reads from its environment.
//
// It lives in package main rather than internal/config because it is genuinely
// specific to this binary: a second binary (the seeder in phase 5) needs the
// database URL and nothing else. Promoting it to a shared package would invite
// unrelated binaries to grow a dependency on each other's settings.
type Config struct {
	// Required.
	DatabaseURL string

	// Server.
	Addr            string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	MaxBodyBytes    int64

	// CORSAllowedOrigins lists the browser origins permitted to call this API.
	// A single "*" entry allows any origin, which is safe here only because the
	// API uses no cookies or credentials — a browser will not attach either to a
	// wildcard-origin response. Lock it to explicit origins the moment auth lands.
	CORSAllowedOrigins []string

	// Connection pool. Explicit rather than left to pgx's defaults, because
	// pool sizing is a property of the deployment — instance count, database
	// max_connections, expected concurrency — and a default that silently works
	// in development is exactly what falls over in production.
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration

	// Domain tuning.
	HistoryWindowDays int
	Alpha             float64

	// Logging.
	LogLevel  slog.Level
	LogFormat string // "json" or "text"
}

// configError collects every problem before returning, so a misconfigured
// deployment reports all its faults in one boot rather than revealing them one
// restart at a time.
type configError struct {
	problems []string
}

func (e *configError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.problems, "\n  - ")
}

// LoadConfig reads configuration from the environment.
//
// Fail-fast is the design: a missing DATABASE_URL stops the process at startup
// with a clear message, rather than producing a service that boots healthy and
// then 500s on first traffic. Everything else has a working default.
func LoadConfig() (*Config, error) {
	cfg := &Config{}
	errs := &configError{}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		errs.problems = append(errs.problems,
			"DATABASE_URL is required (e.g. postgres://user:pass@host:5432/stockwatch?sslmode=disable)")
	}

	cfg.Addr = envString("ADDR", ":8080")

	// Comma-separated list, e.g. "https://app.example.com,https://admin.example.com".
	// Defaults to "*" so a fresh deployment works before any origin is known;
	// tighten it once the frontend URL is fixed.
	cfg.CORSAllowedOrigins = splitAndTrim(envString("CORS_ALLOWED_ORIGINS", "*"))

	// Timeout relationships that must hold, checked below rather than assumed:
	// RequestTimeout bounds handler work, WriteTimeout bounds the whole
	// response, so WriteTimeout must exceed RequestTimeout or the server cuts
	// the connection before a slow handler can report its own timeout.
	cfg.RequestTimeout = envDuration("REQUEST_TIMEOUT", 10*time.Second, errs)
	cfg.ShutdownTimeout = envDuration("SHUTDOWN_TIMEOUT", 20*time.Second, errs)
	cfg.ReadTimeout = envDuration("READ_TIMEOUT", 10*time.Second, errs)
	cfg.WriteTimeout = envDuration("WRITE_TIMEOUT", 15*time.Second, errs)
	cfg.IdleTimeout = envDuration("IDLE_TIMEOUT", 60*time.Second, errs)
	cfg.MaxBodyBytes = int64(envInt("MAX_BODY_BYTES", 1<<20, errs))

	// Default of 10 assumes a handful of replicas against a Postgres with the
	// stock max_connections of 100: 4 replicas x 10 leaves headroom for
	// migrations, psql sessions and a seed job. Raise it with the database, not
	// on its own.
	cfg.MaxConns = int32(envInt("DB_MAX_CONNS", 10, errs))
	cfg.MinConns = int32(envInt("DB_MIN_CONNS", 2, errs))
	// Recycling connections hourly keeps a long-lived pool from pinning stale
	// server-side state and lets a failover redistribute connections.
	cfg.MaxConnLifetime = envDuration("DB_MAX_CONN_LIFETIME", time.Hour, errs)
	cfg.MaxConnIdleTime = envDuration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute, errs)
	cfg.HealthCheckPeriod = envDuration("DB_HEALTH_CHECK_PERIOD", time.Minute, errs)

	cfg.HistoryWindowDays = envInt("HISTORY_WINDOW_DAYS", 28, errs)
	cfg.Alpha = envFloat("DEMAND_ALPHA", 0.10, errs)

	cfg.LogLevel = envLogLevel("LOG_LEVEL", slog.LevelInfo, errs)
	cfg.LogFormat = envString("LOG_FORMAT", "json")

	// Cross-field validation. Each of these is a setting that parses fine on its
	// own but produces a broken server in combination.
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		errs.problems = append(errs.problems,
			fmt.Sprintf("LOG_FORMAT must be \"json\" or \"text\", got %q", cfg.LogFormat))
	}
	if cfg.MaxConns < 1 {
		errs.problems = append(errs.problems,
			fmt.Sprintf("DB_MAX_CONNS must be at least 1, got %d", cfg.MaxConns))
	}
	if cfg.MinConns < 0 || cfg.MinConns > cfg.MaxConns {
		errs.problems = append(errs.problems,
			fmt.Sprintf("DB_MIN_CONNS (%d) must be between 0 and DB_MAX_CONNS (%d)",
				cfg.MinConns, cfg.MaxConns))
	}
	if cfg.WriteTimeout <= cfg.RequestTimeout {
		errs.problems = append(errs.problems, fmt.Sprintf(
			"WRITE_TIMEOUT (%s) must exceed REQUEST_TIMEOUT (%s), or the server "+
				"closes the connection before a slow handler can report its own timeout",
			cfg.WriteTimeout, cfg.RequestTimeout))
	}
	if cfg.HistoryWindowDays < 7 {
		errs.problems = append(errs.problems, fmt.Sprintf(
			"HISTORY_WINDOW_DAYS is %d; fewer than 7 days cannot cover a full "+
				"weekly cycle and makes day-of-week factors meaningless", cfg.HistoryWindowDays))
	}
	if cfg.Alpha <= 0 || cfg.Alpha > 1 {
		errs.problems = append(errs.problems,
			fmt.Sprintf("DEMAND_ALPHA must be in (0, 1], got %v", cfg.Alpha))
	}

	if len(errs.problems) > 0 {
		return nil, errs
	}
	return cfg, nil
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitAndTrim turns "a, b ,c" into ["a","b","c"], dropping empties. Used for
// list-valued env vars where trailing spaces after a comma are a common typo.
func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The env* helpers below record a problem and return the fallback rather than
// returning an error each time. That keeps LoadConfig readable as a flat list of
// settings instead of forty lines of error checks, while still collecting every
// fault for a single report.

func envDuration(key string, fallback time.Duration, errs *configError) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	// time.ParseDuration accepts "20s", "1h30m", "500ms" — far less error-prone
	// than a bare integer whose unit lives only in the variable's name.
	d, err := time.ParseDuration(raw)
	if err != nil {
		errs.problems = append(errs.problems,
			fmt.Sprintf("%s must be a duration such as \"20s\" or \"1h\", got %q", key, raw))
		return fallback
	}
	if d <= 0 {
		errs.problems = append(errs.problems,
			fmt.Sprintf("%s must be positive, got %s", key, d))
		return fallback
	}
	return d
}

func envInt(key string, fallback int, errs *configError) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		errs.problems = append(errs.problems,
			fmt.Sprintf("%s must be an integer, got %q", key, raw))
		return fallback
	}
	return n
}

func envFloat(key string, fallback float64, errs *configError) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		errs.problems = append(errs.problems,
			fmt.Sprintf("%s must be a number, got %q", key, raw))
		return fallback
	}
	return f
}

func envLogLevel(key string, fallback slog.Level, errs *configError) slog.Level {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	var level slog.Level
	// slog.Level implements encoding.TextUnmarshaler, so it parses "debug",
	// "INFO", "warn", "error" and offset forms like "info+2" for free.
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		errs.problems = append(errs.problems, fmt.Sprintf(
			"%s must be debug, info, warn or error, got %q", key, raw))
		return fallback
	}
	return level
}
