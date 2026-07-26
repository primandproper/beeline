package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	// modernc.org/sqlite registers the pure-Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// Open opens the SQLite database at path, applies pending migrations, and returns a
// ready *sql.DB. Pass ":memory:" for an ephemeral database (tests). Foreign keys are
// enabled (the area_layers → areas cascade depends on them) and, for file databases,
// WAL journaling is set for better read/write concurrency.
//
// The pool is capped at a single open connection: SQLite allows only one writer, and
// a shared in-memory database lives only as long as its one connection, so a single
// long-lived connection is both correct and simplest for this low-volume,
// operator-driven store.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	if err = runMigrations(db); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// dsn builds a modernc.org/sqlite DSN. Pragmas are passed via _pragma= query params
// so they apply to every connection in the pool, not just the first.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	if path != ":memory:" {
		q.Add("_pragma", "journal_mode(WAL)")
	}

	return "file:" + path + "?" + q.Encode()
}
