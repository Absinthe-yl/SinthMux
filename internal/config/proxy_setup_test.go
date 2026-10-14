package config

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubProxySecretFromEnv(t *testing.T) {
	t.Setenv("SINTHMUX_PROXY_SECRET", "test-only")
	if HubFromEnv().ProxySecret != "test-only" {
		t.Fatal("proxy configuration was not loaded")
	}
}

func TestProxySecretMigration(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not available")
	}
	script, err := filepath.Abs("../../scripts/ensure-proxy-secret.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func(path string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command(bash, script, path)
		// Tests must not evaluate user shell startup files.
		cmd.Env = append(os.Environ(), "BASH_ENV=/dev/null", "ENV=/dev/null")
		return cmd.CombinedOutput()
	}
	t.Run("existing credentials preserved and repeated upgrade is idempotent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "compose.env")
		original := "SINTHMUX_COMPOSE_DB_PASSWORD=test-only\n# untouched comment\nOTHER=$(must-not-execute)"
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if output, err := run(path); err != nil || len(output) != 0 {
			t.Fatalf("migration failed or printed credentials: err=%v output bytes=%d", err, len(output))
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		prefix := original + "\nSINTHMUX_PROXY_SECRET="
		if !strings.HasPrefix(string(after), prefix) {
			t.Fatal("migration replaced existing configuration")
		}
		secret := strings.TrimSuffix(strings.TrimPrefix(string(after), prefix), "\n")
		decoded, err := hex.DecodeString(secret)
		if err != nil || len(decoded) != 32 {
			t.Fatal("migration did not generate a 256-bit hex secret")
		}
		if output, err := run(path); err != nil || len(output) != 0 {
			t.Fatal("repeated migration failed or exposed output")
		}
		repeated, err := os.ReadFile(path)
		if err != nil || string(repeated) != string(after) {
			t.Fatal("repeated migration changed the secret or other configuration")
		}
	})
	t.Run("invalid and duplicate secrets fail without changing the file", func(t *testing.T) {
		for _, original := range []string{
			"SINTHMUX_PROXY_SECRET=\n",
			"SINTHMUX_PROXY_SECRET=invalid\n",
			"SINTHMUX_PROXY_SECRET=" + strings.Repeat("a", 64) + "\nSINTHMUX_PROXY_SECRET=" + strings.Repeat("b", 64),
		} {
			path := filepath.Join(t.TempDir(), "compose.env")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := run(path); err == nil {
				t.Fatal("invalid migration unexpectedly succeeded")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original {
				t.Fatal("invalid migration modified the environment file")
			}
		}
	})
	t.Run("missing env does not create a new deployment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.env")
		if _, err := run(path); err == nil {
			t.Fatal("missing env unexpectedly accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("missing deployment env was created")
		}
	})
}
