//go:build !windows

package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBinary_SIGTERMAndSIGINTExitZero(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			addr := freeLoopbackAddr(t)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			const localKey = "sk-test-binary-signal"
			config := fmt.Sprintf("server:\n  listen: %q\n  shutdown_timeout: 2s\n  api_keys: [%s]\nproviders:\n  mock: {}\n", addr, localKey)
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal("write signal test config:", err)
			}

			command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal("start spillway serve:", err)
			}
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			t.Cleanup(func() {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
					<-wait
				}
			})
			waitForBinaryServer(t, "http://"+addr, wait)

			if err := command.Process.Signal(signal); err != nil {
				t.Fatalf("send %s to spillway: %v", signal, err)
			}
			select {
			case err := <-wait:
				if err != nil {
					t.Errorf("spillway exited after %s: %v; stdout: %s; stderr: %s", signal, err, stdout.String(), stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("spillway did not exit after %s within 5s", signal)
			}
			if strings.Contains(stdout.String()+stderr.String(), localKey) {
				t.Error("serve output contains the local key")
			}
		})
	}
}
