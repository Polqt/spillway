// Package store owns the opt-in local SQLite usage database.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the newest schema this binary can open.
const SchemaVersion = 2

// NewerSchemaError reports a database schema this binary cannot open.
type NewerSchemaError struct {
	Found     int
	Supported int
}

// Error describes the schema compatibility failure.
func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("store schema version %d is newer than supported version %d", e.Found, e.Supported)
}

const pruneInterval = time.Hour

//go:embed migrations/*.sql
var migrations embed.FS

// Options configures a usage store.
type Options struct {
	Path      string
	Retention time.Duration
	Now       func() time.Time
}

// Request contains metadata for one chat request and its provider attempts.
// It intentionally has no fields for prompts, responses, headers, keys or
// provider error text.
type Request struct {
	At               time.Time `json:"at"`
	ClientApp        string    `json:"client_app"`
	RequestedModel   string    `json:"requested_model"`
	Route            string    `json:"route"`
	Tier             string    `json:"tier,omitempty"`
	Provider         string    `json:"provider,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	ErrorKind        string    `json:"error_kind,omitempty"`
	Stream           bool      `json:"stream"`
	Cache            string    `json:"cache,omitempty"`
	LatencyMS        int64     `json:"latency_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	UsageSource      string    `json:"usage_source"`
	CostMicroUSD     int64     `json:"cost_micro_usd"`
	CostSource       string    `json:"cost_source"`
	Skips            []Skip    `json:"skips"`
	Attempts         []Attempt `json:"attempts"`
}

// Skip is a router candidate that was not sent to a provider.
type Skip struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Reason   string `json:"reason"`
}

// Attempt describes one provider request.
type Attempt struct {
	At               time.Time `json:"at"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Outcome          string    `json:"outcome"`
	LatencyMS        int64     `json:"latency_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	UsageSource      string    `json:"usage_source"`
	CostMicroUSD     int64     `json:"cost_micro_usd"`
	CostSource       string    `json:"cost_source"`
}

// Store owns the database connection and hourly pruner.
type Store struct {
	db             *sql.DB
	retention      time.Duration
	now            func() time.Time
	cancel         context.CancelFunc
	done           chan struct{}
	closeOnce      sync.Once
	closeErr       error
	pruneErrMu     sync.Mutex
	pruneErr       error
	recordFailures atomic.Int64
}

// Open creates or migrates the database, prunes expired rows and starts its
// hourly pruner.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.Path == "" {
		return nil, errors.New("store path is required")
	}
	if o.Retention < 24*time.Hour || o.Retention > 3650*24*time.Hour {
		return nil, errors.New("store retention must be from 1d to 3650d")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	file, err := os.OpenFile(o.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open store file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close store file: %w", err)
	}
	if err := os.Chmod(o.Path, 0o600); err != nil {
		return nil, fmt.Errorf("secure store file: %w", err)
	}

	db, err := sql.Open("sqlite", o.Path)
	if err != nil {
		return nil, fmt.Errorf("open store database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	closeDB := func() { _ = db.Close() }
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		closeDB()
		return nil, fmt.Errorf("configure store busy timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		closeDB()
		return nil, fmt.Errorf("enable store foreign keys: %w", err)
	}
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journal); err != nil {
		closeDB()
		return nil, fmt.Errorf("enable store WAL mode: %w", err)
	}
	if journal != "wal" {
		closeDB()
		return nil, fmt.Errorf("enable store WAL mode: SQLite selected %s", journal)
	}
	if err := migrate(ctx, db); err != nil {
		closeDB()
		return nil, err
	}
	s := &Store{db: db, retention: o.Retention, now: o.Now}
	if err := s.prune(ctx); err != nil {
		closeDB()
		return nil, fmt.Errorf("prune usage store: %w", err)
	}
	pruneCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.pruneHourly(pruneCtx)
	return s, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read store schema version: %w", err)
	}
	if version > SchemaVersion {
		return &NewerSchemaError{Found: version, Supported: SchemaVersion}
	}
	for next := version + 1; next <= SchemaVersion; next++ {
		name := fmt.Sprintf("migrations/%04d.sql", next)
		migration, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read store migration %d: %w", next, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin store migration %d: %w", next, err)
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply store migration %d: %w", next, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set store schema version %d: %w", next, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit store migration %d: %w", next, err)
		}
	}
	return nil
}

// Record writes a request and its attempts in one transaction.
func (s *Store) Record(ctx context.Context, r Request) (recordErr error) {
	defer func() {
		if recordErr != nil {
			s.recordFailures.Add(1)
		}
	}()
	if r.At.IsZero() {
		r.At = s.now()
	}
	if len(r.Attempts) > 16 {
		return errors.New("store request has more than 16 attempts")
	}
	if len(r.Skips) > 16 {
		return errors.New("store request has more than 16 skips")
	}
	skips := make([]Skip, len(r.Skips))
	for i, skip := range r.Skips {
		skips[i] = Skip{Provider: bounded(skip.Provider, 64), Model: bounded(skip.Model, 256), Reason: bounded(skip.Reason, 128)}
	}
	skipsJSON, err := json.Marshal(skips)
	if err != nil {
		return fmt.Errorf("encode usage skips: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage record: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO requests (
		at_ns, client_app, requested_model, route, tier, provider, model, status,
		error_kind, stream, cache, latency_ms, prompt_tokens, completion_tokens,
		usage_source, cost_micro_usd, cost_source, skips_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At.UTC().UnixNano(), r.ClientApp, bounded(r.RequestedModel, 256), bounded(r.Route, 512), bounded(r.Tier, 64),
		bounded(r.Provider, 64), bounded(r.Model, 256), r.Status, bounded(r.ErrorKind, 64), r.Stream, bounded(r.Cache, 16),
		r.LatencyMS, r.PromptTokens, r.CompletionTokens, bounded(r.UsageSource, 16), r.CostMicroUSD, bounded(r.CostSource, 16), string(skipsJSON))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insert usage request: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("read usage request id: %w", err)
	}
	for _, attempt := range r.Attempts {
		if attempt.At.IsZero() {
			attempt.At = r.At
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (
			request_id, at_ns, provider, model, outcome, latency_ms, prompt_tokens,
			completion_tokens, usage_source, cost_micro_usd, cost_source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, attempt.At.UTC().UnixNano(), bounded(attempt.Provider, 64), bounded(attempt.Model, 256),
			bounded(attempt.Outcome, 64), attempt.LatencyMS, attempt.PromptTokens, attempt.CompletionTokens,
			bounded(attempt.UsageSource, 16), attempt.CostMicroUSD, bounded(attempt.CostSource, 16)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert usage attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage record: %w", err)
	}
	return nil
}

// RecordFailures returns the number of failed usage record writes.
func (s *Store) RecordFailures() int64 {
	return s.recordFailures.Load()
}

// SpendSince returns recorded request costs at or after the UTC timestamp.
func (s *Store) SpendSince(ctx context.Context, since time.Time) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cost_micro_usd FROM attempts WHERE at_ns >= ?`, since.UTC().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("query usage spend: %w", err)
	}
	var total int64
	for rows.Next() {
		var cost int64
		if err := rows.Scan(&cost); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("read usage spend: %w", err)
		}
		if cost > 0 && total > int64(^uint64(0)>>1)-cost {
			total = int64(^uint64(0) >> 1)
		} else {
			total += cost
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read usage spend: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close usage spend query: %w", err)
	}
	return total, nil
}

func (s *Store) prune(ctx context.Context) error {
	cutoff := s.now().Add(-s.retention).UTC().UnixNano()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM requests WHERE at_ns < ?", cutoff); err != nil {
		return err
	}
	return nil
}

func (s *Store) pruneHourly(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.prune(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.pruneErrMu.Lock()
				s.pruneErr = errors.Join(s.pruneErr, fmt.Errorf("prune usage store: %w", err))
				s.pruneErrMu.Unlock()
			}
		}
	}
}

// Close stops the pruner and closes the database connection.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.closeErr = s.db.Close()
		s.pruneErrMu.Lock()
		s.closeErr = errors.Join(s.closeErr, s.pruneErr)
		s.pruneErrMu.Unlock()
	})
	return s.closeErr
}

func bounded(value string, max int) string {
	value = strings.ToValidUTF8(value, "?")
	var clean strings.Builder
	for _, r := range value {
		if !unicode.IsPrint(r) {
			r = '?'
		}
		if clean.Len()+utf8.RuneLen(r) > max {
			break
		}
		clean.WriteRune(r)
	}
	return clean.String()
}
