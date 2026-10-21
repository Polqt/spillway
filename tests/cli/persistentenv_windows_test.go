//go:build windows

package cli_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestPersistentEnv_RandomNameNotFound(t *testing.T) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "SPILLWAY_TEST_UNSET_" + hex.EncodeToString(suffix[:])
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(fmt.Sprintf(inspectConfig, ""))
	data = []byte(strings.Replace(string(data), "GROQ_CUSTOM_KEY", name, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	options := cli.Options{GOOS: "windows", LookupEnv: noEnv}
	if code := cli.RunWithOptions(t.Context(), []string{"provider", "show", "groq", "-config", path}, nil, &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not set anywhere") {
		t.Fatalf("output = %q", stdout.String())
	}
}
