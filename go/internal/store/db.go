// Package store owns the SQLite database: opening it, migrating it, and the
// transaction plumbing everything else uses.
//
// SQLite replaces PostgreSQL for this deployment, which changes how writes are
// coordinated. The design here follows three rules:
//
//   - One writer. The write pool holds a single connection, so two write
//     transactions never contend for the file lock.
//   - Every write transaction begins immediately. The DSN sets _txlock=immediate
//     so a read-then-write cannot deadlock halfway through.
//   - One mutex per session. Revision numbers must be handed out in the order
//     the events happened within a session; different sessions still write
//     back to back through the single writer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is the database handle shared by every part of the server.
type Store struct {
	read  *sql.DB
	write *sql.DB
	path  string

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// Open prepares the database file, applies pending migrations, and returns the
// two handles: many readers, one writer.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	base := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=wal_autocheckpoint(1000)" +
		"&_txlock=immediate"

	write, err := sql.Open("sqlite", base)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	if err := write.PingContext(ctx); err != nil {
		write.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := migrate(ctx, write); err != nil {
		write.Close()
		return nil, err
	}

	read, err := sql.Open("sqlite", base)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	read.SetMaxOpenConns(8)
	read.SetMaxIdleConns(8)

	return &Store{read: read, write: write, path: path, locks: map[string]*sync.Mutex{}}, nil
}

// Close releases both pools and folds the write-ahead log back into the main
// file so a copied database is complete.
func (s *Store) Close() error {
	if s.write != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.write.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		cancel()
	}
	var first error
	if s.read != nil {
		if err := s.read.Close(); err != nil {
			first = err
		}
	}
	if s.write != nil {
		if err := s.write.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Read returns the pool for queries.
func (s *Store) Read() *sql.DB { return s.read }

// Path returns the database file location, for logging and backups.
func (s *Store) Path() string { return s.path }

// Write runs fn inside a write transaction. The single write connection
// serialises callers, so fn must do only SQL — never network or file I/O.
func (s *Store) Write(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WithSession serialises work for one session, so revisions are handed out in
// the order the events happened. Callers take this lock, then call Write.
func (s *Store) WithSession(sessionID string, fn func() error) error {
	s.locksMu.Lock()
	lock, ok := s.locks[sessionID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[sessionID] = lock
	}
	s.locksMu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// ErrNoRows is re-exported so callers do not need database/sql.
var ErrNoRows = sql.ErrNoRows

// ErrNotFound is the store-level "no such row".
var ErrNotFound = errors.New("not found")

// migrate applies every embedded migration that has not run yet, in filename
// order, each inside its own transaction.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, entry := range entries {
		version := strings.TrimSuffix(filepath.Base(entry), ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(body)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %s: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// splitStatements breaks a migration file on semicolons that end a line. The
// schema has no triggers or stored procedures, so a simple scan is enough.
func splitStatements(body string) []string {
	var out []string
	var current strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(current.String())
			if stmt != "" && stmt != ";" {
				out = append(out, stmt)
			}
			current.Reset()
		}
	}
	if tail := strings.TrimSpace(current.String()); tail != "" {
		out = append(out, tail)
	}
	return out
}
