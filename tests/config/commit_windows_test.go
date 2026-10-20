//go:build windows

package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/Polqt/spillway/internal/config"
	"golang.org/x/sys/windows"
)

func TestCommit_WindowsPreservesAccessRules(t *testing.T) {
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
		beforeOwnerGroup, beforeRules := windowsAccessRules(before.String())
		afterOwnerGroup, afterRules := windowsAccessRules(after.String())
		if beforeOwnerGroup != afterOwnerGroup || !sameStringSet(beforeRules, afterRules) {
			t.Errorf("access rules on %s changed: before %q, after %q", path, before.String(), after.String())
		}
	}
}

func windowsAccessRules(descriptor string) (string, map[string]struct{}) {
	rules := make(map[string]struct{})
	daclStart := strings.Index(descriptor, "D:")
	if daclStart < 0 {
		return descriptor, rules
	}
	for remaining := descriptor[daclStart+2:]; ; {
		start := strings.IndexByte(remaining, '(')
		if start < 0 {
			break
		}
		end := strings.IndexByte(remaining[start+1:], ')')
		if end < 0 {
			break
		}
		fields := strings.Split(remaining[start+1:start+1+end], ";")
		if len(fields) >= 6 {
			// ReplaceFileW may represent the same inherited rule as both explicit and inherited.
			rules[fields[0]+";"+strings.Join(fields[2:], ";")] = struct{}{}
		}
		remaining = remaining[start+1+end+1:]
	}
	return descriptor[:daclStart], rules
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
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
