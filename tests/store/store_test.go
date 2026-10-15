package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
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
