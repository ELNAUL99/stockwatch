package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setEnv sets variables for one test and restores them afterwards.
//
// t.Setenv is the stdlib helper for this and handles cleanup automatically. It
// also refuses to run in a parallel test, which is correct — environment
// variables are process-global, so a parallel test mutating them would race
// against every other test in the binary.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear everything this package reads, so a variable set in the developer's
	// shell cannot change the result of a test.
	for _, key := range []string{
		"DATABASE_URL", "ADDR", "REQUEST_TIMEOUT", "SHUTDOWN_TIMEOUT",
		"READ_TIMEOUT", "WRITE_TIMEOUT", "IDLE_TIMEOUT", "MAX_BODY_BYTES",
		"DB_MAX_CONNS", "DB_MIN_CONNS", "DB_MAX_CONN_LIFETIME",
		"DB_MAX_CONN_IDLE_TIME", "DB_HEALTH_CHECK_PERIOD",
		"HISTORY_WINDOW_DAYS", "DEMAND_ALPHA", "LOG_LEVEL", "LOG_FORMAT",
	} {
		t.Setenv(key, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

const validDSN = "postgres://u:p@localhost:5432/stockwatch?sslmode=disable"

func TestLoadConfigDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": validDSN})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		field     string
		got, want any
	}{
		{"Addr", cfg.Addr, ":8080"},
		{"RequestTimeout", cfg.RequestTimeout, 10 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 20 * time.Second},
		{"WriteTimeout", cfg.WriteTimeout, 15 * time.Second},
		{"MaxConns", cfg.MaxConns, int32(10)},
		{"MinConns", cfg.MinConns, int32(2)},
		{"MaxConnLifetime", cfg.MaxConnLifetime, time.Hour},
		{"HistoryWindowDays", cfg.HistoryWindowDays, 28},
		{"Alpha", cfg.Alpha, 0.10},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"LogFormat", cfg.LogFormat, "json"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.field, tt.got, tt.want)
			}
		})
	}
}

func TestLoadConfigFailsFastOnMissingRequired(t *testing.T) {
	setEnv(t, nil)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error, want a failure for a missing DATABASE_URL")
	}
	// The message must name the variable. A config error that says only
	// "invalid configuration" costs an operator a trip to the source.
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error does not mention DATABASE_URL: %v", err)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL":        validDSN,
		"ADDR":                ":9999",
		"REQUEST_TIMEOUT":     "3s",
		"WRITE_TIMEOUT":       "1m",
		"DB_MAX_CONNS":        "25",
		"DB_MIN_CONNS":        "5",
		"HISTORY_WINDOW_DAYS": "56",
		"DEMAND_ALPHA":        "0.25",
		"LOG_LEVEL":           "debug",
		"LOG_FORMAT":          "text",
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.RequestTimeout != 3*time.Second {
		t.Errorf("RequestTimeout = %v", cfg.RequestTimeout)
	}
	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns = %d", cfg.MaxConns)
	}
	if cfg.HistoryWindowDays != 56 {
		t.Errorf("HistoryWindowDays = %d", cfg.HistoryWindowDays)
	}
	if cfg.Alpha != 0.25 {
		t.Errorf("Alpha = %v", cfg.Alpha)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantInText string
	}{
		{
			name:       "unparseable duration",
			env:        map[string]string{"REQUEST_TIMEOUT": "10"},
			wantInText: "REQUEST_TIMEOUT",
		},
		{
			name:       "non-numeric pool size",
			env:        map[string]string{"DB_MAX_CONNS": "lots"},
			wantInText: "DB_MAX_CONNS",
		},
		{
			name:       "unknown log format",
			env:        map[string]string{"LOG_FORMAT": "xml"},
			wantInText: "LOG_FORMAT",
		},
		{
			name:       "unknown log level",
			env:        map[string]string{"LOG_LEVEL": "verbose"},
			wantInText: "LOG_LEVEL",
		},
		{
			name:       "min conns above max conns",
			env:        map[string]string{"DB_MAX_CONNS": "5", "DB_MIN_CONNS": "10"},
			wantInText: "DB_MIN_CONNS",
		},
		{
			name: "write timeout below request timeout",
			// The cross-field check that matters: otherwise the server severs
			// the connection before a slow handler can report its own timeout,
			// and the client sees a truncated response instead of a clean 504.
			env:        map[string]string{"REQUEST_TIMEOUT": "30s", "WRITE_TIMEOUT": "5s"},
			wantInText: "WRITE_TIMEOUT",
		},
		{
			name:       "alpha outside the unit interval",
			env:        map[string]string{"DEMAND_ALPHA": "1.5"},
			wantInText: "DEMAND_ALPHA",
		},
		{
			name:       "history window too short for a weekly cycle",
			env:        map[string]string{"HISTORY_WINDOW_DAYS": "3"},
			wantInText: "HISTORY_WINDOW_DAYS",
		},
		{
			name:       "negative duration",
			env:        map[string]string{"SHUTDOWN_TIMEOUT": "-5s"},
			wantInText: "SHUTDOWN_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": validDSN}
			for k, v := range tt.env {
				env[k] = v
			}
			setEnv(t, env)

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("LoadConfig() = nil error, want a failure")
			}
			if !strings.Contains(err.Error(), tt.wantInText) {
				t.Errorf("error does not mention %s: %v", tt.wantInText, err)
			}
		})
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	// A misconfigured deployment should reveal all its faults in one boot,
	// rather than one per restart.
	setEnv(t, map[string]string{
		"LOG_FORMAT":   "xml",
		"DEMAND_ALPHA": "9",
		"DB_MAX_CONNS": "nope",
	})

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() = nil error, want a failure")
	}

	for _, want := range []string{"DATABASE_URL", "LOG_FORMAT", "DEMAND_ALPHA", "DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s; problems are being reported one at a time.\ngot: %v",
				want, err)
		}
	}
}
