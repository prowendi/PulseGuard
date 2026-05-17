// Package store contains the SQLite-backed persistence layer.
//
// All SQL statements use ? placeholders; time fields are stored as Unix
// milliseconds (INTEGER) and translated to time.Time at the repo boundary.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/prowendi/PulseGuard/internal/config"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite database at cfg.Path with PulseGuard's required
// pragmas (WAL journaling, foreign keys, busy timeout). The directory
// containing the file must already exist; ":memory:" and "file:" URIs
// are accepted verbatim.
func Open(cfg config.Database) (*sql.DB, error) {
	dsn, err := buildDSN(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

// buildDSN wraps the configured path into a file: URI carrying the
// pragmas modernc.org/sqlite recognises via the _pragma query parameter.
func buildDSN(cfg config.Database) (string, error) {
	if cfg.Path == "" {
		return "", fmt.Errorf("database.path is empty")
	}
	busyMs := cfg.BusyTimeout.Std().Milliseconds()
	if busyMs <= 0 {
		busyMs = 5000
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMs))

	// Pass-through for tests that already provided a file: URI.
	if strings.HasPrefix(cfg.Path, "file:") {
		sep := "?"
		if strings.Contains(cfg.Path, "?") {
			sep = "&"
		}
		return cfg.Path + sep + q.Encode(), nil
	}
	abs := cfg.Path
	if cfg.Path != ":memory:" {
		var err error
		abs, err = filepath.Abs(cfg.Path)
		if err != nil {
			return "", fmt.Errorf("abs %s: %w", cfg.Path, err)
		}
	}
	return "file:" + filepath.ToSlash(abs) + "?" + q.Encode(), nil
}
