package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/store"
)

func TestStoreInspect_ReadOnlyNeverMigrates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	opened, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- path is created inside t.TempDir above.
	// #nosec G304 -- path is created inside t.TempDir above.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Version != store.SchemaVersion || inspection.Newer {
		t.Errorf("inspection = %+v", inspection)
	}
	// #nosec G304 -- path is created inside t.TempDir above.
	// #nosec G304 -- path is created inside t.TempDir above.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only inspection changed the store bytes")
	}
}

func TestStoreInspect_NewerSchemaNeverSuggestsDeletion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	opened, err := store.Open(t.Context(), store.Options{Path: path, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- path is created inside t.TempDir above.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Newer || inspection.Version != 999 {
		t.Errorf("inspection = %+v", inspection)
	}
	// #nosec G304 -- path is created inside t.TempDir above.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("newer schema inspection changed the store bytes")
	}
}
