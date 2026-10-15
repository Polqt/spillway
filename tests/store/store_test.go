package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/store"
	_ "modernc.org/sqlite"
)

func TestOpen_CreatesCurrentSchemaWithPrivateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := store.Open(t.Context(), store.Options{Path: path, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != store.SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, store.SchemaVersion)
	}
	var journal string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	if runtime.GOOS != "windows" {
		info, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Stat(info)
		if err != nil {
			t.Fatal(err)
		}
		if file.Mode().Perm() != 0o600 {
			t.Errorf("file permissions = %04o, want 0600", file.Mode().Perm())
		}
	}
}

func TestStore_SpendSinceSumsPersistedRequestCosts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 2, 12, 0, 0, 0, time.UTC)
	s, err := store.Open(t.Context(), store.Options{Path: filepath.Join(t.TempDir(), "usage.db"), Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, request := range []store.Request{
		{At: now.Add(-48 * time.Hour), CostMicroUSD: 900, Attempts: []store.Attempt{{At: now.Add(-48 * time.Hour), CostMicroUSD: 900}}},
		{At: now.Add(-time.Hour), CostMicroUSD: 1234, Attempts: []store.Attempt{{At: now.Add(-time.Hour), CostMicroUSD: 1234}}},
		{At: now, CostMicroUSD: 5678, Attempts: []store.Attempt{{At: now, CostMicroUSD: 5678}}},
	} {
		if err := s.Record(t.Context(), request); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	got, err := s.SpendSince(t.Context(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("SpendSince: %v", err)
	}
	if got != 6912 {
		t.Errorf("SpendSince = %d micro-USD, want 6912", got)
	}
}

func TestStore_SpendSinceUsesAttemptCompletionPeriod(t *testing.T) {
	t.Parallel()
	requestAt := time.Date(2026, time.June, 30, 23, 59, 0, 0, time.UTC)
	attemptAt := requestAt.Add(2 * time.Minute)
	s, err := store.Open(t.Context(), store.Options{Path: filepath.Join(t.TempDir(), "usage.db"), Retention: 90 * 24 * time.Hour, Now: func() time.Time { return attemptAt }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(t.Context(), store.Request{
		At: requestAt, CostMicroUSD: 123,
		Attempts: []store.Attempt{{At: attemptAt, CostMicroUSD: 123}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := s.SpendSince(t.Context(), time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SpendSince: %v", err)
	}
	if got != 123 {
		t.Errorf("SpendSince after UTC month start = %d, want attempt cost 123", got)
	}
}

func TestOpen_BackfillsAttemptCostTimes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.May, 4, 9, 0, 0, 0, time.UTC).UnixNano()
	for _, statement := range []string{
		"CREATE TABLE requests (id INTEGER PRIMARY KEY, at_ns INTEGER NOT NULL)",
		"CREATE TABLE attempts (id INTEGER PRIMARY KEY, request_id INTEGER NOT NULL, cost_micro_usd INTEGER NOT NULL)",
		"INSERT INTO requests (id, at_ns) VALUES (1, " + strconv.FormatInt(at, 10) + ")",
		"INSERT INTO attempts (id, request_id, cost_micro_usd) VALUES (1, 1, 77)",
		"PRAGMA user_version = 1",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("seed v1 store: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), store.Options{Path: path, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return time.Unix(0, at) }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.SpendSince(t.Context(), time.Unix(0, at).Add(-time.Second))
	if err != nil || got != 77 {
		t.Errorf("SpendSince migrated data = %d, %v; want 77, nil", got, err)
	}
}

func TestOpen_RefusesNewerSchemaAndInvalidPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	future := filepath.Join(dir, "future.db")
	db, err := sql.Open("sqlite", future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(t.Context(), store.Options{Path: future, Retention: 24 * time.Hour})
	if err == nil || !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "supported") {
		t.Errorf("Open future schema error = %v, want both schema versions", err)
	}

	_, err = store.Open(t.Context(), store.Options{Path: filepath.Join(dir, "missing", "usage.db"), Retention: 24 * time.Hour})
	if err == nil {
		t.Error("Open missing directory succeeded, want error")
	}
}

func TestOpen_PrunesExpiredRowsAtStartup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	first, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Record(t.Context(), store.Request{At: time.Now().Add(-48 * time.Hour), ClientApp: "expired"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	second, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("rows after startup prune = %d, want 0", count)
	}
}

func TestPruner_RemovesExpiredRowsHourlyAndCloseStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	synctest.Test(t, func(t *testing.T) {
		s, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
		if err := s.Record(t.Context(), store.Request{At: time.Now().Add(-48 * time.Hour), ClientApp: "old", RequestedModel: "mock/echo"}); err != nil {
			t.Fatalf("Record old row: %v", err)
		}
		if err := s.Record(t.Context(), store.Request{At: time.Now(), ClientApp: "current", RequestedModel: "mock/echo"}); err != nil {
			t.Fatalf("Record current row: %v", err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM requests").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("rows before hourly prune = %d, want 2", count)
		}
		synctest.Wait()
		<-time.After(time.Hour)
		synctest.Wait()
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM requests").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("rows after hourly prune = %d, want 1", count)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		synctest.Wait()
	})
}

func TestOpen_RequiresPathAndRetention(t *testing.T) {
	t.Parallel()
	_, err := store.Open(t.Context(), store.Options{})
	if err == nil {
		t.Fatal("Open empty options succeeded, want error")
	}
}

func TestRecord_CountsWriteFailures(t *testing.T) {
	s, err := store.Open(t.Context(), store.Options{Path: filepath.Join(t.TempDir(), "usage.db"), Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(t.Context(), store.Request{RequestedModel: "mock/echo"}); err == nil {
		t.Fatal("Record on closed store succeeded, want failure")
	}
	if got := s.RecordFailures(); got != 1 {
		t.Errorf("RecordFailures() = %d, want 1", got)
	}
}

func TestRecord_BoundsAndCleansRequestedModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(t.Context(), store.Request{RequestedModel: "mock/\x01" + strings.Repeat("m", 260)}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got string
	if err := db.QueryRowContext(t.Context(), "SELECT requested_model FROM requests").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 256 {
		t.Fatalf("requested model bytes = %d, want 256", len(got))
	}
	if strings.ContainsRune(got, '\x01') {
		t.Fatalf("requested model retained non-printable byte: %q", got)
	}
	if got[5] != '?' {
		t.Fatalf("requested model non-printable byte = %q, want '?': %q", got[5], got)
	}
}
