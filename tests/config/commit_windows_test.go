//go:build windows

package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/Polqt/spillway/internal/config"
	"golang.org/x/sys/windows"
)

func TestCommit_WindowsKeepsSecurityDescriptor(t *testing.T) {
	t.Parallel()
	doc, change, _ := commitFixture(t)
	before, err := windows.GetNamedSecurityInfo(doc.Path(), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	result, err := doc.Commit(t.Context(), change, config.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{doc.Path(), result.BackupPath} {
		after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if before.String() != after.String() {
			t.Errorf("security descriptor on %s changed: before %q, after %q", path, before.String(), after.String())
		}
	}
}

func TestCommit_FailedReplaceKeepsOriginalAndRemovesTemp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		doc, change, before := commitFixture(t)
		pathPtr, err := windows.UTF16PtrFromString(doc.Path())
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(pathPtr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("close config handle: %v", err)
			}
		}()
		if _, err := doc.Commit(t.Context(), change, config.CommitOptions{}); err == nil {
			t.Fatal("Commit succeeded while the target disallowed delete sharing")
		}
		got, err := os.ReadFile(doc.Path())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, before) {
			t.Fatal("failed replacement changed the original config")
		}
		temps, err := filepath.Glob(filepath.Join(filepath.Dir(doc.Path()), ".config.yaml.spillway-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(temps) != 0 {
			t.Errorf("temporary files remain after failed replacement: %v", temps)
		}
	})
}
