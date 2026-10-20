package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/config"
)

func TestCommit_ReplacesContentAndWritesBackup(t *testing.T) {
	t.Parallel()
	doc, change, before := commitFixture(t)
	now := time.Date(2026, time.October, 20, 9, 15, 2, 0, time.FixedZone("UTC+8", 8*60*60))
	result, err := doc.Commit(t.Context(), change, config.CommitOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result.Path != doc.Path() {
		t.Errorf("path = %q, want %q", result.Path, doc.Path())
	}
	if filepath.Base(result.BackupPath) != "config.yaml.20261020T011502Z.bak" {
		t.Errorf("backup = %q", result.BackupPath)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(before) {
		t.Errorf("backup content differs from original")
	}
	updated, err := os.ReadFile(doc.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "gemini:") {
		t.Errorf("updated config lacks gemini provider")
	}
}

func TestCommit_BackupNameUsesUTCTimestamp(t *testing.T) {
	t.Parallel()
	doc, change, _ := commitFixture(t)
	local := time.Date(2026, 10, 20, 9, 15, 2, 0, time.FixedZone("test", 8*60*60))
	result, err := doc.Commit(t.Context(), change, config.CommitOptions{Now: func() time.Time { return local }})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(result.BackupPath), "config.yaml.20261020T011502Z.bak"; got != want {
		t.Errorf("backup basename = %q, want %q", got, want)
	}
}

func TestCommit_KeepsFiveNewestBackupsOnly(t *testing.T) {
	t.Parallel()
	doc, change, _ := commitFixture(t)
	dir := filepath.Dir(doc.Path())
	for _, timestamp := range []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z", "20260104T000000Z", "20260105T000000Z", "20260106T000000Z"} {
		path := doc.Path() + "." + timestamp + ".bak"
		if err := os.WriteFile(path, []byte("prior backup"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "config.yaml.not-a-backup")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc.Path()+".20260107T000000Z.bak.extra", []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)
	if _, err := doc.Commit(t.Context(), change, config.CommitOptions{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(doc.Path() + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(backups)
	if len(backups) != 5 {
		t.Fatalf("backups = %d, want 5: %v", len(backups), backups)
	}
	for _, path := range []string{unrelated, doc.Path() + ".20260107T000000Z.bak.extra"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated file %q was removed: %v", path, err)
		}
	}
}

func TestCommit_ConcurrentChangeAborts(t *testing.T) {
	t.Parallel()
	doc, change, _ := commitFixture(t)
	want := "# another writer\n"
	if err := os.WriteFile(doc.Path(), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := doc.Commit(t.Context(), change, config.CommitOptions{})
	if !errors.Is(err, config.ErrConcurrentEdit) {
		t.Fatalf("Commit error = %v, want ErrConcurrentEdit", err)
	}
	got, err := os.ReadFile(doc.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("concurrent content changed: %q", got)
	}
	backups, _ := filepath.Glob(doc.Path() + ".*.bak")
	if len(backups) != 0 {
		t.Errorf("concurrent edit created backups: %v", backups)
	}
}

func TestCommit_RejectsChangeFromAnotherDocument(t *testing.T) {
	t.Parallel()
	first, change, _ := commitFixture(t)
	second, _, _ := commitFixture(t)
	_, err := second.Commit(t.Context(), change, config.CommitOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("Commit error = %v", err)
	}
	if _, err := os.Stat(first.Path()); err != nil {
		t.Fatal(err)
	}
}

func TestCommit_FollowsSymlink(t *testing.T) {
	t.Parallel()
	doc, _, _ := commitFixture(t)
	target := doc.Path()
	link := filepath.Join(filepath.Dir(target), "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	linkedDoc, err := config.OpenDocument(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkedDoc.Path() != target {
		t.Fatalf("resolved path = %q, want %q", linkedDoc.Path(), target)
	}
	edit := linkedDoc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linkedDoc.Commit(t.Context(), change, config.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced, mode = %v", info.Mode())
	}
}

func commitFixture(t *testing.T) (*config.Document, config.Change, []byte) {
	t.Helper()
	before, err := os.ReadFile(filepath.Join("testdata", "edit", "init-groq.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatal(err)
	}
	return doc, change, before
}
