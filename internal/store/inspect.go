package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
)

// Inspection reports a database schema without creating or migrating a store.
type Inspection struct {
	Path      string
	Version   int
	Supported int
	Newer     bool
}

// Inspect opens path read-only and reports its SQLite schema version.
func Inspect(ctx context.Context, path string) (Inspection, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Inspection{}, fmt.Errorf("resolve store path: %w", err)
	}
	fileURL := url.URL{Scheme: "file", Opaque: filepath.ToSlash(absolute)}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_pragma", "busy_timeout(2000)")
	fileURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", fileURL.String())
	if err != nil {
		return Inspection{}, fmt.Errorf("open store read-only: %w", err)
	}
	defer func() { _ = db.Close() }()
	var version string
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return Inspection{}, fmt.Errorf("read store schema version: %w", err)
	}
	parsed, err := strconv.Atoi(version)
	if err != nil {
		return Inspection{}, fmt.Errorf("parse store schema version: %w", err)
	}
	return Inspection{Path: absolute, Version: parsed, Supported: SchemaVersion, Newer: parsed > SchemaVersion}, nil
}
